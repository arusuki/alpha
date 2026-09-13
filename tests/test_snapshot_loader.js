// Exercise the actual Worker protocol and bounded transfer without a browser.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm'),util=require('util');
const Usage=require('../dist/usage.js');
class AbortController {
  constructor(){this.signal={aborted:false,listeners:new Set(),addEventListener(type,fn){this.listeners.add(fn);},removeEventListener(type,fn){this.listeners.delete(fn);}};}
  abort(){if(this.signal.aborted)return;this.signal.aborted=true;for(const fn of this.signal.listeners)fn();}
}
class DOMException extends Error {constructor(message,name){super(message);this.name=name;}}
const clone=value=>JSON.parse(JSON.stringify(value));
let source,status=200,knownLength=true,workers=[],stallDownload=false,releaseDownload;
class Worker {
  constructor(){
    workers.push(this);this.terminated=false;this.nodeBatchSizes=[];
    const worker=this;
    this.context=vm.createContext({console,TextDecoder:util.TextDecoder,TextEncoder:util.TextEncoder,performance:{now:()=>Date.now()},self:{},postMessage(message){
      if(message.type==='chunk'&&message.section==='nodes')worker.nodeBatchSizes.push(message.value.length);
      const copy=clone(message);
      setImmediate(()=>{if(!worker.terminated)worker.onmessage({data:copy});});
    },fetch:async()=>({ok:status===200,status,headers:{get:name=>name==='Content-Length'&&knownLength?String(Buffer.byteLength(source)):null},json:async()=>JSON.parse(source),body:{getReader(){
      const bytes=Buffer.from(source);let offset=0;
      return {async read(){
        if(stallDownload){await new Promise(resolve=>{releaseDownload=resolve;});}
        if(offset>=bytes.length)return {done:true};
        // Split UTF-8 names across chunks as real network streams can do.
        const value=bytes.slice(offset,offset+71);offset+=value.length;return {done:false,value};
      }};
    }}})});
    this.context.importScripts=(...paths)=>paths.forEach(path=>vm.runInContext(fs.readFileSync('dist'+path,'utf8'),this.context));
    vm.runInContext(fs.readFileSync('dist/snapshot-worker.js','utf8'),this.context);
  }
  postMessage(message){setImmediate(()=>{if(!this.terminated)this.context.self.onmessage({data:message});});}
  terminate(){this.terminated=true;}
}
const context=vm.createContext({console,Worker,Usage,DOMException,setTimeout,clearTimeout});
vm.runInContext(fs.readFileSync('dist/snapshot-loader.js','utf8'),context);
const read=(options)=>{context.options=options;return vm.runInContext("SnapshotLoader.read('/snapshot',options)",context);};
const tick=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
  const data=JSON.parse(fs.readFileSync('tests/fixtures/snapshot.json','utf8'));
  const target=data.tree.children[0];
  // Over 500 nodes forces multiple main-thread batches; retain child ordering.
  const children=Array.from({length:1201},(_,i)=>({name:'文件-'+i,path:target.path+'/文件-'+i,kind:'file',allocated:1,apparent:1,files:1,errors:0,children:[]}));
  children.push({...children[0],name:'hardlink',path:target.path+'/hardlink',kind:'reference',reference:children[0].path,allocated:0,apparent:0,files:0});
  children.push({...children[0],name:'cycle',path:target.path+'/cycle',kind:'reference',reference:target.path+'/cycle',allocated:0,apparent:0,files:0});
  target.children=children;
  data.containers[0].name='容器中文名称';
  data.containers[0].upper_path=target.path;data.containers[1].upper_path=target.path;
  data.containers[2].upper_path=target.path+'/hardlink';
  source=JSON.stringify(data);
  const controller=new AbortController(),events=[];
  const result=await read({signal:controller.signal,onProgress:p=>events.push(p)});
  assert.deepStrictEqual(clone(result.data),data);
  const expected=Usage.build(data);
  for(const key of ['exclusive','shared','crossOwner','unrelated','attributionLimited'])assert.equal(result.usage[key],expected[key],key);
  for(const [id,row] of expected.containers)assert.deepStrictEqual(clone(result.usage.containers.get(id)),row);
  for(const path of expected.nodes.keys()){
    assert.deepStrictEqual(clone(result.usage.host.get(path)),expected.host.get(path),'host attribution survives Worker transfer');
    const actual=result.usage.inspect(path),want=expected.inspect(path);
    assert.equal(actual.known,want.known);assert.equal(actual.partial,want.partial);
    assert.strictEqual(actual.node,want.node?result.usage.nodes.get(want.node.path):want.node);
  }
  assert.strictEqual(result.data.tree,result.usage.nodes.get('@root'));
  assert.equal(result.usage.inspect('/not-present').known,false);
  assert.equal(result.usage.inspect(target.path+'/cycle').known,false);
  for(const c of result.data.containers)assert.strictEqual(result.usage.containers.get(c.id).container,c);
  assert.deepStrictEqual(events.map(e=>e.stage).filter((stage,i,all)=>i===0||stage!==all[i-1]),['download','parse','validate','index','relations','aggregate','transfer']);
  assert(events.some(p=>p.stage==='parse'&&p.total===null));
  assert(events.some(p=>p.stage==='download'&&p.done===Buffer.byteLength(source)&&p.total===p.done));
  assert(workers[0].nodeBatchSizes.length>1);assert(Math.max(...workers[0].nodeBatchSizes)<=500);assert(workers[0].terminated);
  const transfer=events.filter(p=>p.stage==='transfer');assert.equal(transfer[transfer.length-1].done,transfer[transfer.length-1].total);
  // Download and CPU work are stopped through Worker termination, with no result.
  stallDownload=true;
  const cancelled=new AbortController(),cancelPromise=read({signal:cancelled.signal,onProgress(){}});
  while(!releaseDownload)await tick();
  cancelled.abort();await assert.rejects(cancelPromise,e=>e.name==='AbortError');assert(workers[1].terminated);
  stallDownload=false;releaseDownload();
  source='{broken';await assert.rejects(read({signal:new AbortController().signal,onProgress(){}}),/JSON/);
  for (const version of [1,3]) {
    source=JSON.stringify({...data,schema_version:version});
    await assert.rejects(read({signal:new AbortController().signal,onProgress(){}}),/快照/);
  }
  for (const path of [['revision'],['scan','omitted_references'],['containers',0,'writable_layer'],['filesystems',0,'scanned']]) {
    const invalid=clone(data), key=path[path.length-1];
    delete path.slice(0,-1).reduce((value,part)=>value[part],invalid)[key];
    source=JSON.stringify(invalid);
    await assert.rejects(read({signal:new AbortController().signal,onProgress(){}}),/格式无效/);
  }
  status=401;source=JSON.stringify({error:'会话已过期'});await assert.rejects(read({signal:new AbortController().signal,onProgress(){}}),e=>e.status===401);
  status=200;knownLength=false;source=JSON.stringify(data);
  const unknown=[];await read({signal:new AbortController().signal,onProgress:p=>unknown.push(p)});
  assert(unknown.some(p=>p.stage==='download'&&p.total===null));
  assert(workers.every(w=>w.terminated));
  console.log('Snapshot Worker checks passed: UTF-8 streaming, measured stages, bounded node batches, data/accounting parity, identities, cancellation and parse/auth errors.');
})().catch(error=>{console.error(error);process.exitCode=1;});
