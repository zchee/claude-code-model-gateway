// Static may-reach analysis. Source under inspection is never required or evaluated.
'use strict';
const ts = require('typescript');
const fs = require('node:fs');
const path = require('node:path');
const assert = require('node:assert/strict');
const {isBuiltin} = require('node:module');
const {childPrograms} = require('./child-programs.cjs');
const {launches} = require('./launches.cjs');

function descendants(node, visit) {
  if (visit(node) !== false) ts.forEachChild(node, child => { descendants(child, visit); });
}
const isFunction = n => ts.isFunctionLike(n) && !!n.body;
const literal = n => ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n);
const isRequire = n => ts.isCallExpression(n) && n.expression.getText() === 'require';
const isAssertModule = n => isRequire(n) && n.arguments.length === 1 && literal(n.arguments[0]) && /^(?:node:)?assert(?:\/strict)?$/.test(n.arguments[0].text);
const key = values => values.join('\t');
const flat = text => text.replace(/\s+/g, ' ');
const assertionMethods = new Set('deepEqual deepStrictEqual doesNotMatch doesNotReject doesNotThrow equal fail ifError match notDeepEqual notDeepStrictEqual notEqual notStrictEqual ok partialDeepStrictEqual rejects strict strictEqual throws'.split(' '));

function analyze(plugin, cases) {
  const root = path.join(plugin, 'test');
  const files = fs.readdirSync(root, {recursive:true}).filter(f => /\.[cm]?js$/.test(f)).map(f => path.join(root,f));
  const options = {allowJs:true, checkJs:true, noEmit:true, target:ts.ScriptTarget.ESNext,
    module:ts.ModuleKind.Node16, moduleResolution:ts.ModuleResolutionKind.Node16,
    types:['node'], typeRoots:[path.join(__dirname,'node_modules/@types')]};
  const host = ts.createCompilerHost(options,true);
  const outer = ts.createProgram(files,options,host);
  const local = sf => sf.fileName.startsWith(root + path.sep);
  const {programs, unknown} = childPrograms(outer.getSourceFiles().filter(local),outer.getTypeChecker());
  const children = new Map([...programs.values()].map(c => [c.virtual,c]));
  const program = ts.createProgram([...files,...children.keys()],options,{
    ...host,
    fileExists: name => children.has(name) || host.fileExists(name),
    getSourceFile: (name,...args) => children.has(name) ? ts.createSourceFile(name,children.get(name).text,options.target,true,ts.ScriptKind.JS) : host.getSourceFile(name,...args),
  });
  const checker = program.getTypeChecker();
  const sourceFiles = program.getSourceFiles().filter(local);
  const {classify,functions,boundFunctions,productFiles,value} = require('./resolve.cjs').resolver(program,plugin,sourceFiles);
  const callRules = new Map(), resolvedCalls = new Map();
  const position = (sf,pos) => {
    const child = children.get(sf.fileName);
    if (child) { pos = child.offsets[pos]; sf = child.sf; }
    const {line,character} = sf.getLineAndCharacterOfPosition(pos);
    return [path.relative(plugin,sf.fileName),line+1,character+1];
  };
  const sites = new Map(), assertions = new Map(), registrations = new Map(), hooks = new Map();
  const checked = new Set();
  // A TestContext also exposes assertion methods. Distinguish its `assert`
  // property from arbitrary project objects with the same property name.
  function contextAssert(n) {
    if (!ts.isPropertyAccessExpression(n) || n.name.text !== 'assert') return false;
    const member = checker.getSymbolAtLocation(n.name)?.declarations || [];
    if (member.some(d => d.getSourceFile().fileName.endsWith('/@types/node/test.d.ts'))) return true;
    const d = checker.getSymbolAtLocation(n.expression)?.valueDeclaration;
    if (!d || !ts.isParameter(d) || d.parent.parameters[0] !== d) return false;
    const registration = d.parent.parent;
    return ts.isCallExpression(registration) && registration.arguments.at(-1) === d.parent && /^(?:test|\w+\.test)$/.test(registration.expression.getText());
  }
  function handoff(n) {
    const p = n.parent;
    if (ts.isPropertyAssignment(p) && p.initializer === n || ts.isShorthandPropertyAssignment(p) && p.name === n) return p.name.getText();
    if (ts.isVariableDeclaration(p) && p.initializer === n && ts.isIdentifier(p.name)) return p.name.text;
    if (ts.isBinaryExpression(p) && p.right === n && p.operatorToken.kind === ts.SyntaxKind.EqualsToken) return p.left.getText();
    if ((ts.isCallExpression(p) || ts.isNewExpression(p)) && p.arguments?.includes(n)) return p.expression.getText();
    if (ts.isReturnStatement(p) || ts.isArrowFunction(p) && p.body === n) return 'return';
    if (ts.isArrayLiteralExpression(p)) return 'array element';
    if (ts.isParenthesizedExpression(p) || ts.isConditionalExpression(p) && p.condition !== n) return handoff(p);
    return null;
  }
  function assertionValue(n, seen = new Set()) {
    if (!n || seen.has(n)) return false;
    seen.add(n);
    if (isAssertModule(n) || contextAssert(n)) return true;
    if (ts.isPropertyAccessExpression(n)) return contextAssert(n.expression) || assertionMethods.has(n.name.text) && assertionValue(n.expression,seen);
    if (ts.isElementAccessExpression(n)) {
      const method=value(n.argumentExpression);
      return (method===null || assertionMethods.has(method)) && assertionValue(n.expression,seen);
    }
    if (!ts.isIdentifier(n)) return false;
    const sym = ts.isShorthandPropertyAssignment(n.parent) ? checker.getShorthandAssignmentValueSymbol(n.parent) : checker.getSymbolAtLocation(n);
    const d = sym?.valueDeclaration ?? sym?.declarations?.[0];
    if (!d) return false;
    if (ts.isImportSpecifier(d) || ts.isNamespaceImport(d) || ts.isImportClause(d)) {
      const imp=ts.findAncestor(d,ts.isImportDeclaration);
      if (imp && /^(?:node:)?assert(?:\/strict)?$/.test(imp.moduleSpecifier.text)) return !ts.isImportSpecifier(d) || assertionMethods.has((d.propertyName || d.name).text);
    }
    if (ts.isBindingElement(d) && ts.isVariableDeclaration(d.parent.parent)) {
      const owner = d.parent.parent.initializer;
      return owner && (contextAssert(owner) || assertionMethods.has((d.propertyName || d.name).getText()) && assertionValue(owner,seen));
    }
    return ts.isVariableDeclaration(d) && assertionValue(d.initializer,seen);
  }
  for (const sf of program.getSourceFiles().filter(local)) {
    assert(!sf.parseDiagnostics.length, `malformed JavaScript: ${sf.fileName}`);
    descendants(sf, n => {
      const called = ts.isCallExpression(n) && assertionValue(n.expression);
      const receiver = !called && (ts.isIdentifier(n) || ts.isPropertyAccessExpression(n) || ts.isElementAccessExpression(n)) && handoff(n);
      if (called || receiver && assertionValue(n)) {
        const coords = position(sf,n.getStart()), id = key(coords), call = (called ? n.expression : n).getText();
        assert(!sites.has(id) || sites.get(id).call === call, `conflicting assertion at ${id}`);
        sites.set(id,{coords,call,receiver}); assertions.set(n,id);
      }
      if (!ts.isCallExpression(n) || children.has(sf.fileName)) return;
      const e = n.expression.getText();
      if (e === 'test' || e === 't.test') {
        const loc = position(sf,ts.isPropertyAccessExpression(n.expression) ? n.expression.name.getStart() : n.getStart());
        registrations.set(key(loc),n);
      }
      if (/^(?:test\.)?(?:before|after|beforeEach|afterEach)$/.test(e) && !ts.findAncestor(n,isFunction)) {
        if (!hooks.has(sf)) hooks.set(sf,[]);
        hooks.get(sf).push(n);
      }
    });
  }
  const pairs = new Map(), unresolved = new Map(), reached = new Set(), callbackAssertions = new Map();
  for (const row of cases) {
    const caseKey = [row.path,row.line,row.column,row.name];
    const registration = registrations.get(key(caseKey.slice(0,3)));
    assert(registration, `missing registration: ${key(caseKey)}`);
    checked.add(registration);
    const visited = new Set(), bound = new Map(), pending = new Map(), launchSites = new Map();
    function bind(d,args) {
      for (const [i,param] of d.parameters.entries()) {
        if (!bound.has(param)) bound.set(param,new Set());
        bound.get(param).add(args?.[i] || param.initializer);
      }
    }
    function functionBody(fn,via) {
      for (const parameter of fn.parameters) scan(parameter,via);
      scan(fn.body,via);
    }
    function follow(decl, via) {
      if (!decl || !local(decl.getSourceFile()) || visited.has(decl)) return;
      visited.add(decl);
      if (isFunction(decl)) functionBody(decl,via);
      else if (ts.isVariableDeclaration(decl) && decl.initializer) scan(decl.initializer,via);
    }
    let childLaunch = null;
    function opaque(n, text, rule = null) {
      const spelling = text.replaceAll('\\','\\\\').replaceAll('\t','\\t').replaceAll('\r','\\r').replaceAll('\n','\\n');
      const value = [...caseKey,...position(n.getSourceFile(),n.getStart()),spelling];
      if (rule) resolvedCalls.set(key(value),[...value,rule]);
      else unresolved.set(key(value),value);
    }
    function traceLaunch(n) {
      const launch = launches(n,checker,program,plugin,value,bound,programs);
      if (!launch) return;
      launchSites.set(n,launch);
      for (const script of launch.entries) if (local(script) && !visited.has(script)) {
        visited.add(script);
        const prior=childLaunch;
        childLaunch=`embedded:node-entry:${position(n.getSourceFile(),n.getStart()).join(':')}`;
        scan(script,childLaunch); childLaunch=prior;
      }
    }
    function scan(n, via) {
      if (ts.isFunctionDeclaration(n)) return; // declaration alone does not invoke it
      const sf = n.getSourceFile(), projected = children.get(sf.fileName);
      const id = assertions.get(n);
      if (id) {
        const site = sites.get(id), values = [...caseKey,...site.coords], k = key(values);
        const route = site.receiver ? `callback:${flat(site.receiver)}` : projected ? `embedded:${projected.how}` : childLaunch || via;
        if (!pairs.has(k) || via === 'direct') pairs.set(k,[...values,site.call,route]);
        reached.add(id);
      }
      const anchor = `${sf.fileName}:${n.getStart()}`;
      if (unknown.has(anchor)) opaque(n, `child-input:${unknown.get(anchor)}`);
      const child = programs.get(anchor);
      if (child && !visited.has(child)) {
        visited.add(child);
        scan(program.getSourceFile(child.virtual), `embedded:${child.how}`);
      }
      if (ts.isCallExpression(n) || ts.isNewExpression(n)) {
        const callee = n.expression, text = callee.getText();
        const d = checker.getResolvedSignature(n)?.declaration;
        if (d && isFunction(d) && local(d.getSourceFile())) bind(d,n.arguments);
        const spec = isRequire(n) ? n.arguments[0] : null;
        const unknownImport = isRequire(n) && (!spec || !literal(spec) || (!isBuiltin(spec.text) && !ts.resolveModuleName(spec.text,sf.fileName,options,host).resolvedModule));
        if ((!d || unknownImport) && !id) {
          if (!callRules.has(n)) {
            const defs = functions(callee);
            callRules.set(n,defs?.length === 1 ? defs[0] : classify(n));
          }
          const rule = callRules.get(n);
          if (rule && typeof rule !== 'string') follow(rule,`callback:${flat(text)}`);
          const spelling = projected ? projected.sf.text.slice(projected.offsets[callee.getStart()],projected.offsets[callee.end-1]+1) : text;
          if (rule) opaque(n,spelling,typeof rule !== 'string' ? 'resolved-function' : rule);
          else pending.set(n,spelling);
        }
        traceLaunch(n);
        if (d && isFunction(d)) follow(d,`helper:${flat(d.name?.getText() || text)}`);
      }
      if (ts.isIdentifier(n) && !(ts.isPropertyAccessExpression(n.parent) && n.parent.name === n)) {
        const sym = ts.isShorthandPropertyAssignment(n.parent) ? checker.getShorthandAssignmentValueSymbol(n.parent) : checker.getSymbolAtLocation(n);
        const d = sym?.valueDeclaration ?? sym?.declarations?.[0];
        if (d && (ts.isBindingElement(d) || ts.isImportSpecifier(d)) && n.parent.arguments?.includes(n)) {
          for (const signature of checker.getTypeAtLocation(n).getCallSignatures()) follow(signature.declaration,`callback:${n.text}`);
        }
        if (d && d.name !== n && (isFunction(d) || ts.isVariableDeclaration(d))) {
          const kind = n.parent.expression === n && ts.isCallExpression(n.parent) ? 'helper' : 'callback';
          follow(d,`${kind}:${n.text}`);
        }
      }
      if (isFunction(n)) functionBody(n, `callback:${flat(ts.isCallExpression(n.parent) ? n.parent.expression.getText() : n.name?.getText() || 'function')}`);
      else ts.forEachChild(n, child => { scan(child,via); });
    }
    const body = registration.arguments.at(-1);
    assert(isFunction(body), `unrecognized test callback: ${key(caseKey)}`);
    visited.add(body); functionBody(body,'direct');
    for (const hook of hooks.get(registration.getSourceFile()) || []) scan(hook,`callback:${hook.expression.getText()}`);
    for (const [n,spelling] of pending) {
      // Knowing that require itself is native does not resolve its module.
      const targets = isRequire(n) ? null : boundFunctions(n.expression,bound);
      if (targets) for (const target of targets) {
        bind(target,n.arguments); follow(target,`callback:${flat(spelling)}`);
        if (target.body && local(target.getSourceFile())) descendants(target.body,child => {
          if (isFunction(child)) return false;
          const site=sites.get(assertions.get(child));
          if (site) {
            const witness=[...caseKey,...position(n.getSourceFile(),n.getStart()),spelling,...site.coords];
            callbackAssertions.set(key(witness),witness);
          }
        });
      }
      opaque(n,spelling,targets ? 'case-bound-callback' : null);
    }
    // A later call to a shared launcher may supply a different environment.
    // Reinspect each launch with the accumulated bindings for this case.
    let previousSize;
    do {
      previousSize=visited.size;
      for (const n of launchSites.keys()) traceLaunch(n);
    } while (visited.size!==previousSize);
    for (const [n,launch] of launchSites) if (launch.unknown) opaque(n,`child-launch:${launch.api}`);
  }
  assert.equal(checked.size,registrations.size,'registration absent from census');
  return {pairs,unresolved:[...unresolved.values()],resolvedCalls:[...resolvedCalls.values()],callbackAssertions:[...callbackAssertions.values()],productFiles,sites,
    embeddedInputs:[...programs].map(([anchor,c]) => [...position(c.sf,Number(anchor.slice(anchor.lastIndexOf(':')+1))),c.how]),
    unreached:[...sites.keys()].filter(k => !reached.has(k))};
}
module.exports = {analyze,key};
