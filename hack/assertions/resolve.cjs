// Resolve sources of opaque calls, not just the spelling of a method. An
// unknown receiver with a familiar method name must still be reviewed.
'use strict';
const ts = require('typescript');
const fs = require('node:fs');
const path = require('node:path');
const {isBuiltin} = require('node:module');
const literal = n => n && (ts.isStringLiteral(n) || ts.isNoSubstitutionTemplateLiteral(n));
const bodies = n => n && ts.isFunctionLike(n) && n.body;
const prop = n => ts.isPropertyAccessExpression(n) ? n.name.text : '';
const libOrigin = sf => {
  if (!sf?.isDeclarationFile) return null;
  const node = sf.fileName.match(/\/@types\/node\/(?:ts[\d.]+\/)?([^/.]+)/);
  if (node) return 'node:'+node[1];
  if (sf.fileName.includes('/typescript/lib/lib.')) return 'ecmascript';
  return null;
};
function paths(checker) {
  function decl(n) { const s = checker.getSymbolAtLocation(n); return s?.valueDeclaration ?? s?.declarations?.[0]; }
  function value(n,seen = new Set()) {
    if (!n || seen.has(n)) return null;
    const next = new Set(seen).add(n);
    if (literal(n)) return n.text;
    if (ts.isIdentifier(n)) return n.text === '__dirname' ? path.dirname(n.getSourceFile().fileName) : value(decl(n)?.initializer,next);
    if (!ts.isCallExpression(n)) return null;
    const callee = n.expression.getText();
    if (callee === 'require.resolve' && n.arguments.length === 1 && literal(n.arguments[0])) return path.resolve(path.dirname(n.getSourceFile().fileName),n.arguments[0].text);
    if (!['path.join','path.resolve','path.dirname'].includes(callee)) return null;
    const parts = n.arguments.map(arg => value(arg,next));
    if (!parts.length || parts.some(p => p === null)) return null;
    if (callee === 'path.dirname') return path.dirname(parts[0]);
    return callee === 'path.join' ? path.join(...parts) : path.resolve(...parts);
  }
  return {decl,value};
}
function resolver(program,plugin,sources) {
  const checker = program.getTypeChecker(), {decl,value} = paths(checker);
  const product = new Set();
  for (const dir of ['lib','hooks','bin']) {
    const folder = path.join(plugin,dir);
    if (!fs.existsSync(folder)) continue;
    for (const file of fs.readdirSync(folder,{recursive:true}).filter(f => /\.[cm]?js$/.test(f))) {
      const p = path.join(folder,file), text = fs.readFileSync(p,'utf8');
      // Include strings/generated scripts: a changed product that begins to
      // import assertions must not silently retain assertion-free boundaries.
      if (/['"`](?:node:)?assert(?:\/strict)?['"`]|\bassert\s*[.(]/.test(text)) throw Error(`product assertion boundary needs review: ${p}`);
      product.add(p);
    }
  }
  const lock = path.join(plugin,'package-lock.json');
  const packages = fs.existsSync(lock) ? JSON.parse(fs.readFileSync(lock,'utf8')).packages : {};
  function moduleOrigin(n) {
    if (!n || !ts.isCallExpression(n) || n.expression.getText() !== 'require') return null;
    const s = value(n.arguments[0]);
    if (!s) return null;
    if (isBuiltin(s)) return 'node:'+s.replace(/^node:/,'');
    if (/^\.?\.?\//.test(s)) {
      const f = path.resolve(path.dirname(n.getSourceFile().fileName),s);
      if (product.has(f)) return 'product:'+path.relative(plugin,f).split('/')[0];
      if (f.startsWith(plugin+'/') && f.endsWith('.json') && fs.existsSync(f)) return 'json';
      if (f.startsWith(plugin+'/node_modules/')) {
        const name = f.slice((plugin+'/node_modules/').length).split('/')[0];
        if (packages['node_modules/'+name] && f.endsWith('/package.json')) return 'package:'+name;
      }
      return null;
    }
    const name = s.startsWith('@') ? s.split('/').slice(0,2).join('/') : s.split('/')[0];
    return packages['node_modules/'+name] ? 'package:'+name : null;
  }
  const callers = new Map(), assigned = new Map(), calls = [];
  function functions(n,seen=new Set()) {
    if (!n || seen.has(n)) return null;
    const next=new Set(seen).add(n);
    if (bodies(n)) return [n];
    if (!ts.isIdentifier(n)) return null;
    const d=decl(n);
    if (bodies(d)) return [d];
    if (d && ts.isVariableDeclaration(d)) {
      const exprs=[...(d.initializer?[d.initializer]:[]),...(assigned.get(d)||[])];
      const defs=exprs.map(e=>functions(e,next));
      if (defs.length && defs.every(Boolean)) return [...new Set(defs.flat())];
    }
    if (d && ts.isParameter(d)) {
      const i=d.parent.parameters.indexOf(d);
      const refs=(callers.get(d.parent)||[]).map(c=>functions(c.arguments?.[i]??d.initializer,next));
      if (refs.length && refs.every(Boolean)) return [...new Set(refs.flat())];
    }
    return null;
  }
  function index(n) {
    if (ts.isCallExpression(n) || ts.isNewExpression(n)) {
      calls.push(n);
      const d = checker.getResolvedSignature(n)?.declaration;
      if (bodies(d)) { if (!callers.has(d)) callers.set(d,[]); callers.get(d).push(n); }
    }
    if (ts.isBinaryExpression(n) && n.operatorToken.kind === ts.SyntaxKind.EqualsToken && ts.isIdentifier(n.left)) {
      const d = decl(n.left);
      if (d) { if (!assigned.has(d)) assigned.set(d,[]); assigned.get(d).push(n.right); }
    }
    ts.forEachChild(n,index);
  }
  for (const sf of sources) index(sf);
  const forwarded = new Map();
  for (const call of calls) {
    for (const target of functions(call.expression) || []) {
      if (!callers.has(target)) callers.set(target,[]);
      if (!callers.get(target).includes(call)) callers.get(target).push(call);
    }
    if (!ts.isPropertyAccessExpression(call.expression) || call.expression.name.text!=='createServer') continue;
    const obj=call.expression.expression, module=ts.isIdentifier(obj) && moduleOrigin(decl(obj)?.initializer);
    if (!['node:http','node:https','node:net'].includes(module)) continue;
    for (const fn of functions(call.arguments?.at(-1)) || []) for (const param of fn.parameters.slice(0,module==='node:net'?1:2)) forwarded.set(param,module);
  }
  const memo = new Map();
  const dataMethods = new Set(['map','filter','find','flatMap','some','every','includes','startsWith','endsWith','split','join','trim','indexOf','lastIndexOf','toString','replace','replaceAll','slice','push','pop','shift','entries','test','match','has','get','add','then','catch','finally']);
  // Recover returned object fields (including values fulfilled by a Promise).
  // A function may return a project object; do not infer a native receiver
  // from its method's name alone.
  const returnCache=new Map();
  function returns(fn) {
    if (returnCache.has(fn)) return returnCache.get(fn);
    const result=[];
    if (ts.isBlock(fn.body)) {
      const walk=n=>{ if (ts.isReturnStatement(n)) { if(n.expression) result.push(n.expression); } else if(!bodies(n)) ts.forEachChild(n,walk); };
      walk(fn.body);
    } else result.push(fn.body);
    returnCache.set(fn,result);
    return result;
  }
  function fieldValues(n,fields,seen=new Set()) {
    if (!n || seen.has(n) || seen.size>25) return [null];
    const next=new Set(seen).add(n), go=(e,k=fields)=>fieldValues(e,k,next);
    if (ts.isAwaitExpression(n) || ts.isParenthesizedExpression(n)) return go(n.expression);
    if (ts.isPropertyAccessExpression(n)) return go(n.expression,[n.name.text,...fields]);
    if (ts.isIdentifier(n)) {
      const d=decl(n);
      if (d && ts.isVariableDeclaration(d) && d.initializer) return go(d.initializer);
      if (d && ts.isBindingElement(d) && d.parent.parent.initializer) return go(d.parent.parent.initializer,[d.propertyName?.getText() || d.name.getText(),...fields]);
    }
    if (ts.isCallExpression(n)) {
      const d=checker.getResolvedSignature(n)?.declaration;
      if (bodies(d) && sources.includes(d.getSourceFile())) return returns(d).flatMap(x=>go(x));
    }
    if (ts.isNewExpression(n) && n.expression.getText()==='Promise') {
      const executor=n.arguments?.[0], fulfilled=executor?.parameters?.[0];
      if (!fulfilled) return [];
      const values=[];
      const walk=c=>{ if (ts.isCallExpression(c) && ts.isIdentifier(c.expression) && decl(c.expression)===fulfilled && c.arguments.length) values.push(...go(c.arguments[0])); ts.forEachChild(c,walk); };
      walk(executor.body);
      return values;
    }
    if (ts.isObjectLiteralExpression(n) && fields.length) {
      const p=n.properties.find(p=>p.name?.getText()===fields[0]);
      if (p && ts.isPropertyAssignment(p)) return go(p.initializer,fields.slice(1));
      if (p && ts.isShorthandPropertyAssignment(p)) {
        const d=checker.getShorthandAssignmentValueSymbol(p)?.valueDeclaration;
        if (d?.initializer) return go(d.initializer,fields.slice(1));
      }
    }
    return fields.length ? [null] : [n];
  }
  function origin(n,seen = new Set()) {
    if (!n || seen.has(n) || seen.size>35) return null;
    if (memo.has(n)) return memo.get(n);
    const next = new Set(seen).add(n), sub = a => origin(a,next);
    const common = values => values.length && values.every(x => x && x===values[0]) ? values[0] : null;
    let o = null;
    const type = checker.getTypeAtLocation(n);
    if (!(type.flags & (ts.TypeFlags.Any|ts.TypeFlags.Unknown|ts.TypeFlags.Never))) {
      if (type.flags & (ts.TypeFlags.StringLike | ts.TypeFlags.NumberLike | ts.TypeFlags.BooleanLike) || checker.isArrayType(type) || checker.isTupleType(type)) o='ecmascript';
      else o=libOrigin(type.getSymbol()?.declarations?.[0]?.getSourceFile());
    }
    if (ts.isIdentifier(n) && forwarded.has(decl(n))) o=forwarded.get(decl(n));
    if (!o && literal(n)) o='ecmascript';
    if (!o && ts.isAwaitExpression(n)) o=sub(n.expression);
    if (!o && ts.isParenthesizedExpression(n)) o=sub(n.expression);
    if (!o && ts.isConditionalExpression(n)) o=common([sub(n.whenTrue),sub(n.whenFalse)]);
    if (!o && ts.isIdentifier(n)) {
      const d=decl(n);
      if (d && ts.isVariableDeclaration(d)) {
        const vals=[...(d.initializer?[d.initializer]:[]),...(assigned.get(d)||[])];
        o=common(vals.map(sub));
      } else if (d && ts.isBindingElement(d)) {
        const owner=d.parent.parent;
        if (ts.isVariableDeclaration(owner) && owner.initializer) o=sub(owner.initializer);
      } else if (d && ts.isParameter(d)) {
        const fn=d.parent, idx=fn.parameters.indexOf(d), caller=fn.parent;
        if (ts.isCallExpression(caller) && caller.arguments.at(-1)===fn && /^(?:test|t\.test)$/.test(caller.expression.getText()) && idx===0) o='node:test';
        else if (ts.isNewExpression(caller) && caller.expression.getText()==='Promise' && idx<2) o='promise-settle';
        else if (ts.isCallExpression(caller) && caller.arguments.includes(fn) && prop(caller.expression)==='createServer') {
          const mod=sub(caller.expression.expression);
          if (mod==='node:http' || mod==='node:https' || mod==='node:net') o=mod;
        }
        if (!o && callers.has(fn)) o=common(callers.get(fn).map(c => sub(c.arguments?.[idx] ?? d.initializer)));
      }
    }
    if (!o && (ts.isPropertyAccessExpression(n)||ts.isElementAccessExpression(n))) {
      if (!o && ts.isPropertyAccessExpression(n)) o=common(fieldValues(n.expression,[n.name.text]).map(sub));
      const base=sub(n.expression);
      if (!o && (base==='json' || base?.startsWith('product:') || base?.startsWith('package:') || base?.startsWith('node:'))) o=base;
    }
    if (!o && (ts.isCallExpression(n)||ts.isNewExpression(n))) {
      const e=n.expression, d=checker.getResolvedSignature(n)?.declaration;
      o=moduleOrigin(n);
      if (!o && e.getText()==='JSON.parse') o='json';
      if (!o && ts.isNewExpression(n) && e.getText()==='Promise') o='ecmascript';
      if (!o && bodies(d) && sources.includes(d.getSourceFile())) {
        const returns=[];
        const inspect=c=>{ if (ts.isReturnStatement(c)) { if(c.expression) returns.push(c.expression); return; } if (!bodies(c)) ts.forEachChild(c,inspect); };
        if (ts.isBlock(d.body)) inspect(d.body); else returns.push(d.body);
        o=common(returns.map(sub));
      }
      if (!o && ts.isPropertyAccessExpression(e)) {
        const owner=sub(e.expression), method=e.name.text;
        if (owner==='json' && dataMethods.has(method)) o='json';
        if (owner==='ecmascript' && dataMethods.has(method)) o='ecmascript';
        if (owner?.startsWith('package:')) o=owner;
        if (owner?.startsWith('node:') && ['createServer','request','connect','spawn','spawnSync'].includes(method)) o=owner;
      }
      if (!o && ts.isNewExpression(n)) {
        const owner=sub(e); if(owner?.startsWith('package:')) o=owner;
      }
    }
    if (o) memo.set(n,o);
    return o;
  }
  function classify(n) {
    if (n.expression?.getText()==='require') return moduleOrigin(n);
    const e=n.expression;
    if (ts.isPropertyAccessExpression(e)) {
      const src=origin(e.expression);
      if (src==='json' || src==='ecmascript') return dataMethods.has(e.name.text)?'ecmascript':null;
      if (src?.startsWith('node:') || src?.startsWith('product:') || src?.startsWith('package:')) return src;
    }
    if (ts.isIdentifier(e)) {
      const src=origin(e); if(src==='promise-settle') return 'ecmascript:Promise';
      if(src?.startsWith('product:')||src?.startsWith('package:')||src?.startsWith('node:')) return src;
    }
    if (ts.isCallExpression(e)) { const src=origin(e); if(src?.startsWith('package:')) return src; }
    return null;
  }
  function boundFunctions(n,bound,fields=[],seen=new Set()) {
    if (!n || seen.has(n) || seen.size>32) return null;
    const next=new Set(seen).add(n), pick=(v,ks=fields)=>boundFunctions(v,bound,ks,next);
    const all=xs=>xs.length && xs.every(Boolean) ? [...new Set(xs.flat())] : null;
    if (!fields.length && bodies(n)) return [n];
    if (!fields.length && (literal(n)||ts.isNumericLiteral(n)||ts.isArrayLiteralExpression(n)||n.kind===ts.SyntaxKind.NullKeyword)) return [];
    if (!fields.length && ts.isExpression(n)) {
      const signatures=checker.getTypeAtLocation(n).getCallSignatures().map(s=>s.declaration);
      if (signatures.length && signatures.every(d=>d && libOrigin(d.getSourceFile()))) return signatures;
    }
    if (ts.isPropertyAccessExpression(n)) return pick(n.expression,[n.name.text,...fields]);
    if (ts.isParenthesizedExpression(n) || ts.isAwaitExpression(n)) return pick(n.expression);
    if (ts.isIdentifier(n)) {
      const d=decl(n);
      if (bodies(d)) return fields.length ? null : [d];
      return d ? pick(d) : null;
    }
    if (ts.isParameter(n)) return all([...(bound.get(n)||[n.initializer])].map(x=>pick(x)));
    if (ts.isBindingElement(n)) {
      const owner=n.parent.parent, name=n.propertyName?.getText()||n.name.getText();
      if (ts.isParameter(owner) && n.initializer) {
        const args=[...(bound.get(owner)||[owner.initializer])];
        return all(args.map(a=> {
          if (a && ts.isObjectLiteralExpression(a) && a.properties.every(p=>!ts.isSpreadAssignment(p) && p.name?.getText()!==name)) return pick(n.initializer);
          return pick(a,[name,...fields]);
        }));
      }
      return pick(owner,[name,...fields]);
    }
    if (ts.isVariableDeclaration(n)) return all([...(n.initializer?[n.initializer]:[]),...(assigned.get(n)||[])].map(x=>pick(x)));
    if (ts.isArrayLiteralExpression(n) && fields[0]==='[]') return all(n.elements.map(v=>pick(v,fields.slice(1))));
    if (ts.isObjectLiteralExpression(n) && fields.length) {
      for (const p of [...n.properties].reverse()) {
        if (ts.isSpreadAssignment(p)) {
          const result=pick(p.expression); if (result) return result;
          // An unknown spread may replace an earlier field.
          if (!ts.isObjectLiteralExpression(p.expression) || p.expression.properties.some(k=>k.name?.getText()===fields[0])) return null;
        }
        if (p.name?.getText()!==fields[0]) continue;
        if (ts.isMethodDeclaration(p)) return fields.length===1?[p]:null;
        if (ts.isShorthandPropertyAssignment(p)) return pick(checker.getShorthandAssignmentValueSymbol(p)?.valueDeclaration,fields.slice(1));
        return ts.isPropertyAssignment(p)?pick(p.initializer,fields.slice(1)):null;
      }
    }
    if (ts.isCallExpression(n)) {
      if (prop(n.expression)==='shift') return pick(n.expression.expression,['[]',...fields]);
      const d=checker.getResolvedSignature(n)?.declaration;
      if (bodies(d) && sources.includes(d.getSourceFile())) return all(returns(d).map(v=>pick(v)));
      // A product wrapper can return its supplied operation's result. Resolve
      // every return path, instead of assuming a product return is a builtin.
      if (bodies(d) && product.has(d.getSourceFile().fileName) && n.arguments.some(bodies)) {
        const nested=new Map(bound);
        d.parameters.forEach((p,i)=>nested.set(p,new Set([n.arguments[i]??p.initializer])));
        return all(returns(d).map(v=>boundFunctions(v,nested,fields,next)));
      }
      if (prop(n.expression)==='run' && d && libOrigin(d.getSourceFile())==='node:async_hooks') {
        const callbacks=pick(n.arguments[1],[]);
        return callbacks?.every(bodies) ? all(callbacks.flatMap(fn=>returns(fn).map(v=>pick(v)))) : null;
      }
      if (ts.isIdentifier(n.expression) && bound.has(decl(n.expression))) {
        const callbacks=pick(n.expression,[]);
        return callbacks?.every(bodies) ? all(callbacks.flatMap(fn=>returns(fn).map(v=>pick(v)))) : null;
      }
    }
    return null;
  }
  return {classify,functions,boundFunctions,productFiles:product.size,value};
}
module.exports={resolver};
