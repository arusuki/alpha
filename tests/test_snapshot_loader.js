'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const Usage=require('../dist/usage.js'),viewFixture=require('./snapshot_view_fixture');
let source,status=200,requested;
const context=vm.createContext({Usage,DOMException,fetch:async(url,options)=>{requested={url,options};return {ok:status===200,status,json:async()=>JSON.parse(source)};}});
vm.runInContext(fs.readFileSync('dist/snapshot-loader.js','utf8'),context);
const read=options=>{context.options=options;return vm.runInContext("SnapshotLoader.read('/api/jobs/test/view',options)",context);};
(async()=>{
 const data=JSON.parse(fs.readFileSync('tests/fixtures/snapshot.json','utf8')),expected=Usage.build(data);
 const view=viewFixture(data);source=JSON.stringify(view);
 const controller=new AbortController(),progress=[];
 const result=await read({signal:controller.signal,onProgress:p=>progress.push(p)});
 assert.equal(requested.url,'/api/jobs/test/view');assert.strictEqual(requested.options.signal,controller.signal);
 assert.equal(requested.options.credentials,'same-origin');
 for(const key of ['exclusive','shared','crossOwner','unrelated','attributionLimited'])assert.equal(result.usage[key],expected[key]);
 for(const [id,row] of expected.containers)assert.deepEqual(JSON.parse(JSON.stringify(result.usage.containers.get(id))),row);
 for(const [path,node] of result.usage.nodes){
  const want=expected.inspect(path),actual=result.usage.inspect(path);
  assert.equal(actual.known,want.known);assert.equal(actual.partial,want.partial);
  assert.deepEqual(JSON.parse(JSON.stringify(result.usage.host.get(path))),expected.host.get(path));
  assert.equal(node.children.length,view.nodes.find(n=>n.path===path).children.length);
 }
 assert(result.usage.remote);assert(result.data.tree.loaded);
 assert.equal(progress[0].stage,'download');
 for(const row of view.nodes.filter(n=>!n.loaded))assert.equal(row.children.length,0);
 const target=data.containers[0].upper_path;
 source=JSON.stringify(viewFixture(data,target));
 assert((await read()).usage.inspect(target).node.loaded);
 // The browser must trust server totals, without recomputing a partial tree.
 const supplied=viewFixture(data);supplied.usage.exclusive=987654321;source=JSON.stringify(supplied);
 assert.equal((await read()).usage.exclusive,987654321);
 controller.abort();await assert.rejects(read({signal:controller.signal}),e=>e.name==='AbortError');
 status=403;source=JSON.stringify({error:'forbidden'});await assert.rejects(read(),e=>e.status===403&&e.message==='forbidden');status=200;
 source='{bad';await assert.rejects(read(),SyntaxError);
 for(const bad of [{...view,view_version:2},{...view,nodes:[]},{...view,nodes:[...view.nodes,view.nodes[0]]}]) {source=JSON.stringify(bad);await assert.rejects(read(),/数据/);}
 console.log('Server display loader checks passed: server accounting, partial nodes, references, cancellation and errors.');
})().catch(error=>{console.error(error);process.exitCode=1;});
