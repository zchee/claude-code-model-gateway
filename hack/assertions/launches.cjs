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
    if (!n || seen.has(n)) return [];
    if (ts.isCallExpression(n)) {
      const fn=checker.getResolvedSignature(n)?.declaration;
      const body=fn?.body;
      if (body && !ts.isBlock(body)) return expand(body,new Set(seen).add(n));
      if (body?.statements?.length===1 && ts.isReturnStatement(body.statements[0])) return expand(body.statements[0].expression,new Set(seen).add(n));
    }
    if (!ts.isIdentifier(n)) return [n];
    const sym=checker.getSymbolAtLocation(n), d=sym?.valueDeclaration??sym?.declarations?.[0];
    if (!d) return [n];
    const next=new Set(seen).add(n);
    if (ts.isVariableDeclaration(d) && d.initializer) return expand(d.initializer,next);
    if (bound.has(d)) return [...bound.get(d)].flatMap(a=>expand(a,next));
    return [n];
  }
  const options=api==='fork'?args.slice(1).find(n=>ts.isObjectLiteralExpression(n)):args.at(-1);
  const option = k => options && ts.isObjectLiteralExpression(options) && options.properties.find(p=>p.name?.getText()===k)?.initializer;
  const cwd=option('cwd');
  function entry(file) {
    if (!file) { unknown=true; return; }
    if (isBuiltin(file)) return;
    if (file.startsWith('file:')) { try { file=fileURLToPath(file); } catch { unknown=true; return; } }
    if (!path.isAbsolute(file)) {
      const dir=cwd?value(cwd):plugin;
      if (!dir || !path.isAbsolute(dir)) { unknown=true; return; }
      file=path.resolve(dir,file);
    }
    const sf=program.getSourceFile(file);
    if (!sf || !sf.fileName.startsWith(plugin+'/')) { unknown=true; return; }
    entries.add(sf);
  }
  function argv(n,flagsOnly=false) {
    const alternatives=expand(n);
    if (!alternatives.length) unknown=true;
    for (const arr of alternatives) {
      if (!ts.isArrayLiteralExpression(arr)) { unknown=true; continue; }
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
    entry(value(args[0]));
    const flags=option('execArgv'); if(flags) argv(flags,true);
    if (options?.properties?.some(ts.isSpreadAssignment) || (args[2] && !ts.isObjectLiteralExpression(args[2]))) unknown=true;
    if (args.length>1 && !ts.isArrayLiteralExpression(args[1]) && !ts.isObjectLiteralExpression(args[1])) unknown=true;
  } else if (api==='exec' || api==='execSync') unknown=true;
  else {
    const i=api==='spawnGatewayProcess'||api==='spawnDetached'?1:0;
    const binary=expand(args[i]);
    if (!binary.length || !binary.every(n=>n.getText()==='process.execPath' || /^(?:node|node\.exe)$/.test(path.basename(value(n)||'')))) unknown=true;
    else argv(args[i+1]);
  }
  const env=option('env');
  if (env && ts.isObjectLiteralExpression(env)) for (const field of env.properties) {
    if (field.name?.getText().replace(/^['"]|['"]$/g,'')!=='NODE_OPTIONS') continue;
    const text=value(field.initializer);
    if (text===null) { unknown=true; continue; }
    for (const match of text.matchAll(/(?:^|\s)(?:--require|--import|-r)(?:=|\s+)(?:"([^"]+)"|'([^']+)'|(\S+))/g)) entry(match[1]||match[2]||match[3]);
  }
  return {entries,unknown,api};
}
module.exports={launches};
