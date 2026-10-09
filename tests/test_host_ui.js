// Host navigation uses the real explorer, including snapshot refresh and roles.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const elements=new Map(),listeners={};
function element(id) {
  if(!elements.has(id))elements.set(id,{innerHTML:'',textContent:'',value:'',open:false,listeners:{},attributes:{},querySelector(){return {focus(){}};},classList:{toggle(){}},addEventListener(type,fn){this.listeners[type]=fn;},setAttribute(key,value){this.attributes[key]=value;},showModal(){this.open=true;},close(){this.open=false;}});
  return elements.get(id);
}
const sandbox={console,setTimeout,clearTimeout,window:{addEventListener(){},platform:{user:{role:'admin'},jobs:[]}},document:{getElementById:element,querySelectorAll:()=>[],addEventListener(type,fn){listeners[type]=fn;}}};
sandbox.renderTask=()=>{};
sandbox.platform=sandbox.window.platform || {user:null,jobs:[]};
vm.createContext(sandbox);
for(const path of ['dist/usage.js','dist/snapshot.js','dist/disk-capacity.js','dist/app.js'])vm.runInContext(fs.readFileSync(path,'utf8'),sandbox);
const run=code=>vm.runInContext(code,sandbox);
const click=dataset=>listeners.click({target:{closest:()=>({dataset})}});
sandbox.sampleText=fs.readFileSync('tests/fixtures/snapshot.json','utf8');
run(`
var sample=JSON.parse(sampleText);sample.job_id='host-job';
var entry=(path,bytes,children=[],extra={})=>({path,name:path.split('/').pop(),kind:'directory',allocated:bytes,apparent:bytes,files:0,errors:0,children,...extra});
sample.tree=entry('@root',4096,[entry('/',4096,[
  entry('/host',2048,[entry('/host/cache',1024,[],{omitted_entries:5}),entry('/host/file<script>',512,[],{kind:'file'})]),
  entry('/container',1024),entry('/denied',0,[],{kind:'unreadable',errors:1})
])],{kind:'root'});
sample.containers=[{...sample.containers[0],upper_path:'/container',log_path:null,mounts:[]}];sample.resources=[{path:'/container',kinds:['writable'],containers:[sample.containers[0].id]}];
load(sample,'test');
`);
assert(element('storageOverview').innerHTML.includes('data-host'));
assert(element('storageOverview').innerHTML.includes('Host · 宿主机占用'));
click({host:''});
assert(element('containerDialog').open);
assert(element('detailTitle').textContent.startsWith('Host ·'));
assert(!element('detail').innerHTML.includes('data-edit-owner'));
assert(!element('detail').innerHTML.includes('SizeRw'));
assert(element('storageSources').innerHTML.includes('全部已扫描目录'));
assert(element('explorerContent').innerHTML.includes('3 KiB'));
assert(!element('explorerContent').innerHTML.includes('@root'));
const open=name=>click({storageEntry:String(run(`explorer.entries.findIndex(e=>e.name===${JSON.stringify(name)})`))});
open('/');
assert(element('explorerContent').innerHTML.includes('宿主机：/'));
assert(!element('directoryMap').innerHTML.includes('>container</span>'));
open('denied');assert(element('directorySelection').innerHTML.includes('大小未知'));
open('host');
assert(element('explorerContent').innerHTML.includes('宿主机：/host'));
assert(!element('directoryMap').innerHTML.includes('<script>'));
assert(element('directoryMap').innerHTML.includes('&lt;script&gt;'));
open('file<script>');
assert(element('directorySelection').innerHTML.includes('512 B'));
assert.equal(run('explorer.trail.length'),3);
element('mapSearch').listeners.input({target:{value:'file'}});
assert(!element('directoryMap').innerHTML.includes('>cache</span>'));
element('mapSearch').listeners.input({target:{value:''}});
open('cache');
assert(element('directoryMap').innerHTML.includes('点击扫描并拆分'));
run('platform.user.role="viewer";refreshDirectoryScan()');
assert(!element('directorySelection').innerHTML.includes('data-expand-path'));
assert(element('directoryStatus').innerHTML.includes('管理员'));
// A published revision keeps the host scope and physical trail on the same job.
run(`
var updated=JSON.parse(JSON.stringify(sample));var cache=Usage.build(updated).nodes.get('/host/cache');
cache.children=[entry('/host/cache/deeper',512,[],{omitted_entries:3})];cache.omitted_entries=0;
updated.revision=1;load(updated,'updated',Usage.build(updated),true);
`);
assert(element('containerDialog').open);
assert.equal(run('selected===HOST'),true);
assert.equal(run('explorer.trail[explorer.trail.length-1].path'),'/host/cache');
assert(element('directoryMap').innerHTML.includes('>deeper</span>'));
click({storageUp:''});assert(element('explorerContent').innerHTML.includes('宿主机：/host'));
click({storageCrumb:'0'});assert.equal(run('explorer.trail.length'),1);
click({storageSource:'1'});assert(element('explorerContent').innerHTML.includes('4 KiB'));
open('/');assert(element('directoryMap').innerHTML.includes('>container</span>'));
click({container:run('snapshot.containers[0].id')});
assert(element('detail').innerHTML.includes('Docker 逻辑大小'));
assert.equal(run('explorer.trail.length'),1);
click({host:''});
assert.equal(run('explorer.source'),0);
element('containerDialog').close();element('containerDialog').listeners.close();
assert.equal(run('selected'),null);assert.equal(run('explorer'),null);
// No Docker containers and zero host bytes still have an accessible entry.
run(`sample.containers=[];load(sample,'host-only');`);
click({host:''});assert(element('explorerContent').innerHTML.includes('4 KiB'));
run(`sample.tree=entry('@root',0,[],{kind:'root'});load(sample,'empty');`);
assert(element('storageOverview').innerHTML.includes('data-host'));
assert(element('directoryMap').innerHTML.includes('此目录为空'));
run(`var historical={...sample,job_id:'different-job'};load(historical,'history',Usage.build(historical));`);
assert(!element('containerDialog').open);assert.equal(run('selected'),null);
console.log('Host UI checks passed: entry points, scoped totals, drilldown, paths, search, files, unknowns, escaping, roles, refreshed navigation, container switching and host-only/empty snapshots.');
