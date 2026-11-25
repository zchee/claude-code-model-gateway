// Project child program text into virtual source files. Keep an offset map
// through template substitutions and string escapes; do not evaluate them.
'use strict';
const ts = require('typescript');
const path = require('node:path');
const assert = require('node:assert/strict');
const plain = n => n && (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n));
function childPrograms(sources, checker) {
  const programs = new Map(), unknown = new Map();
  function init(n) {
    const sym = checker.getSymbolAtLocation(n);
    return (sym?.valueDeclaration ?? sym?.declarations?.[0])?.initializer;
  }
  function rawPart(node) {
    const text = node.text, raw = node.getText(), start = node.getStart();
    const end = raw.length - ([ts.SyntaxKind.TemplateHead,ts.SyntaxKind.TemplateMiddle].includes(node.kind) ? 2 : 1);
    const offsets = [];
    for (let i = 1; i < end;) {
      const old = i, ch = raw[i++];
      let length = 1;
      if (ch === '\\') {
        const escaped = raw[i++];
        if (escaped === 'x') i += 2;
        else if (escaped === 'u' && raw[i] === '{') {
          i = raw.indexOf('}', i) + 1;
          length = String.fromCodePoint(parseInt(raw.slice(old+3,i-1),16)).length;
        } else if (escaped === 'u') i += 4;
        else if (escaped === '\n' || escaped === '\r') { length = 0; if (escaped === '\r' && raw[i] === '\n') i++; }
      } else if (ch === '\r' && raw[i] === '\n') i++;
      for (let j = 0; j < length; j++) offsets.push(start + old);
    }
    assert.equal(offsets.length, text.length, `unmappable literal: ${node.getSourceFile().fileName}:${start}`);
    return {text, offsets};
  }
  const combine = chunks => chunks.every(Boolean) ? {text: chunks.map(c => c.text).join(''), offsets: chunks.flatMap(c => c.offsets)} : null;
  // Module paths are data: interpret only literal components anchored at
  // __dirname or the static specifier of require.resolve, never user code.
  function modulePath(n, seen = new Set()) {
    if (!n || seen.has(n)) return null;
    seen.add(n);
    if (ts.isIdentifier(n)) return modulePath(init(n),seen);
    if (!ts.isCallExpression(n)) return null;
    if (n.expression.getText() === 'require.resolve' && plain(n.arguments[0])) return n.arguments[0].text;
    if (/^path\.(?:join|resolve)$/.test(n.expression.getText()) && n.arguments[0]?.getText() === '__dirname' && n.arguments.slice(1).every(plain)) {
      const relative = path.posix.join(...n.arguments.slice(1).map(a => a.text));
      return relative.startsWith('../') ? relative : './' + relative;
    }
    return null;
  }
  function project(n, seen = new Set()) {
    if (!n || seen.has(n)) return null;
    const next = new Set(seen).add(n);
    if (plain(n)) return rawPart(n);
    if (ts.isIdentifier(n)) return project(init(n), next);
    if (ts.isParenthesizedExpression(n)) return project(n.expression, next);
    if (ts.isBinaryExpression(n) && n.operatorToken.kind === ts.SyntaxKind.PlusToken) return combine([project(n.left,next),project(n.right,next)]);
    if (ts.isTemplateExpression(n)) {
      const chunks = [rawPart(n.head)];
      for (const span of n.templateSpans) {
        const expr = span.expression;
        // Preserve imports of actual helpers even when the host embeds an
        // absolute path via JSON.stringify(require.resolve('./helper.js')).
        const inner = ts.isCallExpression(expr) && expr.expression.getText() === 'JSON.stringify' && expr.arguments[0];
        const spec = inner && modulePath(inner);
        const replacement = spec ? JSON.stringify(spec) : '__embedded_value';
        chunks.push({text: replacement, offsets: Array(replacement.length).fill(expr.getStart())}, rawPart(span.literal));
      }
      return combine(chunks);
    }
    if (ts.isCallExpression(n) && ts.isPropertyAccessExpression(n.expression) && n.expression.name.text === 'join' && ts.isArrayLiteralExpression(n.expression.expression)) {
      const sep = n.arguments[0];
      if (!plain(sep)) return null;
      return combine(n.expression.expression.elements.flatMap((e,i) => i ? [rawPart(sep),project(e,next)] : [project(e,next)]));
    }
    return null;
  }
  const callers = new Map(), attempted = new Set();
  function index(n) {
    if (ts.isCallExpression(n)) {
      const d = checker.getResolvedSignature(n)?.declaration;
      if (d && d.body) {
        if (!callers.has(d)) callers.set(d,[]);
        callers.get(d).push(n);
      }
    }
    ts.forEachChild(n,index);
  }
  for (const sf of sources) index(sf);
  function add(n, how, required) {
    const sf = n.getSourceFile(), id = `${sf.fileName}:${n.getStart()}`;
    if (attempted.has(id)) return;
    attempted.add(id);
    const source = project(n);
    if (!source) {
      const d = ts.isIdentifier(n) && checker.getSymbolAtLocation(n)?.valueDeclaration;
      const calls = d && ts.isParameter(d) && callers.get(d.parent);
      if (calls?.length) {
        const i = d.parent.parameters.indexOf(d);
        for (const call of calls) {
          if (call.arguments[i]) add(call.arguments[i], how, required);
          else unknown.set(id,`${how}:missing-argument`);
        }
      } else if (required) unknown.set(id,how);
      return;
    }
    if (!source.text.trim()) return;
    const virtual = `${sf.fileName}.${n.getStart()}.embedded.cjs`;
    const parsed = ts.createSourceFile(virtual, source.text, ts.ScriptTarget.ESNext, true, ts.ScriptKind.JS);
    if (parsed.parseDiagnostics.length) { unknown.set(id, `${how}:generated-syntax`); return; }
    programs.set(id, {...source, virtual, sf, how});
  }
  function jsFilename(n, seen = new Set()) {
    if (!n || seen.has(n)) return false;
    seen.add(n);
    if (plain(n)) return /\.[cm]?js$/.test(n.text);
    if (ts.isIdentifier(n)) return jsFilename(init(n), seen);
    return ts.isCallExpression(n) && /\.(?:join|resolve)$/.test(n.expression.getText()) && jsFilename(n.arguments.at(-1), seen);
  }
  function visit(n) {
    if (ts.isArrayLiteralExpression(n)) for (let i = 0; i < n.elements.length-1; i++) {
      const arg = n.elements[i];
      if (plain(arg) && ['-e','--eval','-p','--print'].includes(arg.text)) add(n.elements[i+1], 'node-eval', true);
    }
    if (ts.isCallExpression(n) && /(?:^|\.)(?:writeFileSync|writeFile)$/.test(n.expression.getText()) && n.arguments.length >= 2 && jsFilename(n.arguments[0])) add(n.arguments[1], 'written-script', true);
    // Also recognize helpers which write scripts through their own sink or
    // receive a caller's literal as a parameter rather than a local variable.
    if (plain(n) || ts.isTemplateExpression(n)) {
      const t = plain(n) ? n.text : [n.head,...n.templateSpans.map(s => s.literal)].map(s => s.text).join(' ');
      if (/\brequire\s*\(\s*['"](?:node:)?(?:assert(?:\/strict)?|test)['"]|\bassert\s*[.(]/.test(t)) add(n, 'child-script', true);
    }
    ts.forEachChild(n,visit);
  }
  for (const sf of sources) visit(sf);
  return {programs, unknown};
}
module.exports = {childPrograms};
