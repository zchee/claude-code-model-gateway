// Resolve Node entry and preload arguments without invoking a child or loader.
'use strict';
const ts = require('typescript');
const path = require('node:path');
const {isBuiltin} = require('node:module');
const {fileURLToPath} = require('node:url');

function launches(call,checker,program,plugin,value,bound,programs) {
  const signature=checker.getResolvedSignature(call)?.declaration;
  const source=signature?.getSourceFile().fileName || '';
  let api=call.expression.getText().split('.').at(-1);
  if (source.includes('/@types/node/child_process.d.ts') && signature.name) api=signature.name.getText();
  const wrapped = ['spawnGatewayProcess','spawnGatewayProcessSync','spawnDetached'].includes(api);
  if (!wrapped && !['spawn','spawnSync','exec','execSync','execFile','execFileSync','fork'].includes(api)) return null;
  // Do not treat arbitrary `.fork`/`.exec` methods (e.g. RegExp.exec) as Node primitives.
  if (!wrapped && signature && !source.includes('/@types/node/child_process.d.ts') && !/^node:child_process$/.test(source)) return null;
  const args=call.arguments || [], entries=new Set();
  let unknown=false;
  function expand(n,seen = new Set()) {
    if (!n || seen.has(n)) return [null];
    if (ts.isParenthesizedExpression(n)) return expand(n.expression,new Set(seen).add(n));
    if (ts.isConditionalExpression(n)) return [n.whenTrue,n.whenFalse].flatMap(v=>expand(v,new Set(seen).add(n)));
    if (ts.isCallExpression(n)) {
      const fn=checker.getResolvedSignature(n)?.declaration;
      const body=fn?.body;
      if (body && !ts.isBlock(body)) return expand(body,new Set(seen).add(n));
      if (body?.statements?.length===1 && ts.isReturnStatement(body.statements[0])) return expand(body.statements[0].expression,new Set(seen).add(n));
    }
    if (!ts.isIdentifier(n)) return [n];
    const sym=ts.isShorthandPropertyAssignment(n.parent) ? checker.getShorthandAssignmentValueSymbol(n.parent) : checker.getSymbolAtLocation(n);
    const d=sym?.valueDeclaration??sym?.declarations?.[0];
    if (!d) return [n];
    const next=new Set(seen).add(n);
    if (ts.isVariableDeclaration(d) && d.initializer) return expand(d.initializer,next);
    if (bound.has(d)) return [...bound.get(d)].flatMap(a=>expand(a,next));
    return [n];
  }
  // undefined means the property is absent; null means its value is opaque.
  // Inspect spreads in assignment order: a later explicit key replaces even
  // an unknown spread. Otherwise retain both the unknown and earlier values.
  function properties(input,key,seen=new Set()) {
    if (input===undefined) return [undefined];
    if (!input || seen.has(input)) return [null];
    const next=new Set(seen).add(input);
    return expand(input).flatMap(obj=>{
      if (!obj || !ts.isObjectLiteralExpression(obj)) return [null];
      let result=[undefined];
      for (const p of obj.properties) {
        if (ts.isSpreadAssignment(p)) {
          const spread=properties(p.expression,key,next);
          result=spread.flatMap(v=>v===undefined ? result : v===null ? [null,...result] : [v]);
        } else if (p.name && ts.isComputedPropertyName(p.name) && value(p.name.expression)===null) {
          result.push(null);
        } else if ((p.name?.text ?? (p.name && ts.isComputedPropertyName(p.name) && value(p.name.expression)))===key) {
          result=[ts.isShorthandPropertyAssignment(p) ? p.name : p.initializer??null];
        }
      }
      return [...new Set(result)];
    });
  }
  const executableIndex=api==='spawnGatewayProcess'||api==='spawnDetached'?1:0;
  const optional=args[executableIndex+1];
  // fork/spawn overload their second argument: an array is argv, an object
  // is options. An ambiguous argument cannot prove the absence of a preload.
  const options=args[executableIndex+2]!==undefined ? [args[executableIndex+2]] : optional ? expand(optional).map(n=>n && ts.isArrayLiteralExpression(n)?undefined:n) : [undefined];
  const option=key=>options.flatMap(obj=>properties(obj,key));
  function strings(n,seen=new Set()) {
    if (!n || seen.has(n)) return [null];
    const next=new Set(seen).add(n);
    return expand(n).flatMap(v=>{
      if (!v) return [null];
      const fixed=value(v); if (fixed!==null) return [fixed];
      if (ts.isCallExpression(v) && v.expression.getText()==='JSON.stringify' && v.arguments.length===1) return strings(v.arguments[0],next).map(x=>x===null?null:JSON.stringify(x));
      if (ts.isBinaryExpression(v) && v.operatorToken.kind===ts.SyntaxKind.PlusToken) {
        const left=strings(v.left,next),right=strings(v.right,next);
        if (left.length*right.length>128) return [null];
        return left.flatMap(a=>right.map(b=>a===null||b===null?null:a+b));
      }
      if (ts.isTemplateExpression(v)) {
        let parts=[v.head.text];
        for (const span of v.templateSpans) {
          const subs=strings(span.expression,next);
          if (parts.length*subs.length>128) return [null];
          parts=parts.flatMap(prefix=>subs.map(x=>prefix===null||x===null?null:prefix+x+span.literal.text));
        }
        return parts;
      }
      return [null];
    });
  }
  function entry(file) {
    if (!file) { unknown=true; return; }
    if (isBuiltin(file)) return;
    if (file.startsWith('file:')) { try { file=fileURLToPath(file); } catch { unknown=true; return; } }
    if (!path.isAbsolute(file)) {
      for (const cwd of option('cwd')) for (const dir of cwd===undefined?[plugin]:strings(cwd)) {
        if (!dir || !path.isAbsolute(dir)) unknown=true;
        else entry(path.resolve(dir,file));
      }
      return;
    }
    const sf=program.getSourceFile(file);
    if (!sf || !sf.fileName.startsWith(plugin+'/')) { unknown=true; return; }
    entries.add(sf);
  }
  function argv(n,flagsOnly=false) {
    const alternatives=expand(n);
    if (!alternatives.length) unknown=true;
    for (const arr of alternatives) {
      if (!arr || !ts.isArrayLiteralExpression(arr)) { unknown=true; continue; }
      let script=false;
      for(let i=0;i<arr.elements.length;i++) {
        const arg=arr.elements[i], word=value(arg);
        if (word===null) { unknown=true; break; }
        if (['--require','-r','--import'].includes(word)) { entry(value(arr.elements[++i])); continue; }
        const pre=word.match(/^(?:--require=|--import=|-r(.+))(.+)?$/);
        if (pre) { entry(pre[1]||pre[2]); continue; }
        if (['-e','--eval','-p','--print'].includes(word)) {
          const code=arr.elements[++i];
          if (!code || (!programs.has(`${code.getSourceFile().fileName}:${code.getStart()}`) && value(code)!=='')) unknown=true;
          script=true; continue;
        }
        if (word.startsWith('-') && word!=='--') {
          if (!/^--(?:test(?:-reporter=\S+)?|no-warnings|input-type=\S+|unhandled-rejections=\S+)$/.test(word)) unknown=true;
          continue;
        }
        if (word==='--') continue;
        if (!flagsOnly && !script) { entry(word); script=true; }
        break;
      }
      if (!flagsOnly && !script) unknown=true;
    }
  }
  if (api==='fork') {
    for (const file of strings(args[0])) entry(file);
    for (const flags of option('execArgv')) if (flags!==undefined) argv(flags,true);
  } else if (api==='exec' || api==='execSync') unknown=true;
  else {
    const binary=expand(args[executableIndex]);
    if (!binary.every(n=>n && (n.getText()==='process.execPath' || /^(?:node|node\.exe)$/.test(path.basename(value(n)||''))))) unknown=true;
    if (binary.some(n=>n && (n.getText()==='process.execPath' || /^(?:node|node\.exe)$/.test(path.basename(value(n)||''))))) argv(optional);
  }
  const environments=api==='spawnDetached' ? [args[3]] : option('env');
  for (const env of environments) for (const setting of properties(env,'NODE_OPTIONS')) {
    if (setting===undefined) continue;
    for (const text of strings(setting)) {
      if (text===null) { unknown=true; continue; }
      // Accept plain or double-quoted option words. Unsupported quoting or
      // escaped characters stay opaque rather than silently dropping a flag.
      const words=[];
      let word='',quoted=false;
      for (const ch of text) {
        if (ch==='"') quoted=!quoted;
        else if (ch==='\\' || ch==="'") { unknown=true; word+=ch; }
        else if (/\s/.test(ch) && !quoted) { if(word) words.push(word); word=''; }
        else word+=ch;
      }
      if(quoted) unknown=true;
      if(word) words.push(word);
      for (let i=0;i<words.length;i++) {
        const flag=words[i];
        if (['--require','-r','--import'].includes(flag)) entry(words[++i]);
        else if (/^(?:--require=|--import=|-r.)/.test(flag)) entry(flag.replace(/^(?:--require=|--import=|-r)/,''));
        else if (!/^--(?:no-warnings|unhandled-rejections=\S+)$/.test(flag)) unknown=true;
      }
    }
  }
  return {entries,unknown,api};
}
module.exports={launches};
