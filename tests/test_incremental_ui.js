// Folded-leaf interaction and API/publication contract, using the real UI code.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const elements=new Map(),listeners={};
function element(id){
  if(!elements.has(id))elements.set(id,{id,value:'',hidden:false,disabled:false,open:false,innerHTML:'',textContent:'',dataset:{},style:{},listeners:{},querySelector(){return {focus(){}};},classList:{toggle(){},remove(){}},setAttribute(){},removeAttribute(){},addEventListener(type,fn){this.listeners[type]=fn;},showModal(){this.open=true;},close(){this.open=false;}});
  return elements.get(id);
}
const sandbox={console,EventSource:class {addEventListener(){} close(){}},AbortController:class {constructor(){this.signal={aborted:false};}abort(){this.signal.aborted=true;}},requestAnimationFrame:fn=>setImmediate(fn),setTimeout:()=>1,clearTimeout(){},fetch:()=>new Promise(()=>{}),window:{location:{pathname:'/nodes/'+ 'e'.repeat(32)+'/'},addEventListener(){},SettingsUI:{reset(){},open(){}}},document:{getElementById:element,querySelectorAll:()=>[],addEventListener(type,fn){if(!listeners[type])listeners[type]=[];listeners[type].push(fn);}}};
vm.createContext(sandbox);
for(const path of ['dist/usage.js','dist/snapshot.js','dist/app.js','dist/snapshot-loader.js','dist/platform.js'])vm.runInContext(fs.readFileSync(path,'utf8'),sandbox);
const run=code=>vm.runInContext(code,sandbox);
const click=dataset=>listeners.click.forEach(fn=>fn({target:{closest:()=>({dataset})}}));
sandbox.sampleText=fs.readFileSync('tests/fixtures/snapshot.json','utf8');
run(`
apiURL=path=>path;
var sample=JSON.parse(sampleText);sample.containers.forEach(c=>c.label_owner=c.owner);sample.job_id='a'.repeat(32);sample.revision=0;
var currentRoot=Usage.build(sample).inspect(sample.containers[0].upper_path).node;
var leafPath=currentRoot.path+'/cache';
var entry=(path,bytes,extra={})=>({path,name:path.split('/').pop(),kind:'directory',allocated:bytes,apparent:bytes,files:1,errors:0,children:[],...extra});
currentRoot.children=[entry(leafPath,1024,{omitted_entries:10}),entry(currentRoot.path+'/actual-file',512,{kind:'file'})];
platform.page='overview';platform.user={id:'admin',username:'admin',role:'admin'};platform.csrf='csrf';platform.loaded=sample.job_id;platform.followLatest=false;
load(sample,'历史扫描结果');select('container',snapshot.containers[0]);
var baseJob={id:sample.job_id,status:'completed',trigger:'manual',created_by:'admin',created_at:1,snapshot_revision:0,config:{},allocated:sample.tree.allocated};
var state={jobs:[baseJob],directory_jobs:[],directory_jobs:[],latest_id:'b'.repeat(32),active:null,interval_minutes:0};
var calls=[],rejectStart=false,rejectChanges=false,pauseChanges=false,resolveChanges;
function changesFor(data,revision,path) {
 const copy=JSON.parse(JSON.stringify(data)), {tree,...metadata}=copy;
 const replacements=[],ancestors=[];
 function collect(n) {
  if(n.path===path){replacements.push(n);return;}
  if(n.path==='@root' || path.startsWith(n.path.replace(/\\/$/,'')+'/')){
   const {children,...row}=n;ancestors.push(row);children.forEach(collect);
  }
 }
 if(copy.revision>revision)collect(tree);
 return {job_id:copy.job_id,base_revision:revision,revision:copy.revision,metadata,replacements,ancestors};
}
api=async(path,options={})=>{
 calls.push({path,options});
 if(path==='/api/state')return state;
 if(path==='/api/owners'){
  const body=JSON.parse(options.body);
  sample.containers.find(c=>c.id===body.container_id).owner=body.owner;
  return {owners:{[body.container_id]:body.owner}};
 }
 if(path.includes('/changes?revision=')){
  if(rejectChanges)throw Error('读取失败 <script>');
  const patch=changesFor(sample,Number(path.split('revision=')[1]),state.jobs[0].config.incremental_path);
  if(pauseChanges)return new Promise(resolve=>{resolveChanges=()=>resolve(patch);});
  return patch;
 }
 if(path.endsWith('/expand')){
  if(rejectStart)throw Error('扫描记录已更新 <script>');
  const body=JSON.parse(options.body);
  const job={id:'c'.repeat(32),status:'running',trigger:'incremental',created_by:'admin',created_at:2,config:{base_job_id:sample.job_id,base_revision:body.revision,incremental_path:body.path},progress:{entries:12,path:body.path}};
  state={...state,jobs:[job,baseJob],active:job};return job;
 }
 throw Error('unexpected API '+path);
};
SnapshotLoader.read=async(path)=>{calls.push({path});const data=JSON.parse(JSON.stringify(sample));validate(data);return {data,usage:Usage.build(data)};};
`);
(async()=>{
  assert(!element('explorerContent').innerHTML.includes('data-expand-path'),'no general directory toolbar action');
  click({storageEntry:String(run(`explorer.entries.findIndex(e=>e.name==='cache')`))});
  assert.equal(run('explorer.trail.length'),2);
  assert(element('directoryMap').innerHTML.includes('点击扫描并拆分'),'arriving at a folded leaf offers continuation');
  assert(!element('explorerContent').innerHTML.includes('data-expand-path'));
  assert.equal(element('directoryDepth').value,'3','default retains several levels in one traversal');
  assert.equal(element('directoryDepthControl').hidden,false);
  element('directoryDepth').listeners.change({target:{value:'8'}});
  assert.equal(run('platform.expandDepth'),8);
  run('platform.user.role="viewer";refreshDirectoryScan()');
  assert.equal(element('directoryDepthControl').hidden,true);
  assert(!element('directorySelection').innerHTML.includes('data-expand-path'));assert(element('directoryStatus').innerHTML.includes('管理员'));
  run('platform.user.role="admin";refreshDirectoryScan();rejectStart=true');
  await run('expandLeaf(leafPath)');
  assert(element('directoryStatus').innerHTML.includes('点击灰块重试'));
  assert(element('directoryStatus').innerHTML.includes('&lt;script&gt;'));assert(!element('directoryStatus').innerHTML.includes('<script>'));
  run('rejectStart=false');
  await run('expandLeaf(leafPath)');
  const call=run('calls.find(c=>c.path.endsWith("/expand"))');
  assert.equal(call.path,'/api/jobs/'+'a'.repeat(32)+'/expand');
  assert.deepEqual(JSON.parse(call.options.body),{path:run('leafPath'),revision:0,depth:8});
  assert.equal(element('directoryDepth').disabled,true);
  assert(element('directoryStatus').innerHTML.includes('12 项已遍历'));
  assert(element('directoryStatus').innerHTML.includes('data-cancel-expansion'));
  const callCount=run('calls.length');await run('expandLeaf(leafPath)');assert.equal(run('calls.length'),callCount,'duplicate starts are suppressed');
  run(`state.jobs[0].status='failed';state.jobs[0].error='无法读取目录';state.active=null;`);
  await run('syncState()');
  assert(element('directoryStatus').innerHTML.includes('点击灰块重试'));
  assert(element('directoryStatus').innerHTML.includes('无法读取目录'));
  const bytesBefore=run('usage.exclusive+usage.shared+usage.unrelated');
  run('var untouchedBranch=snapshot.tree.children.find(n=>!leafPath.startsWith(n.path+"/"))');
  run(`
    sample=JSON.parse(JSON.stringify(sample));var leaf=Usage.build(sample).nodes.get(leafPath);leaf.children=[entry(leafPath+'/deeper',1536,{omitted_entries:4})];
    var pending=[sample.tree];while(pending.length){const n=pending.pop();if(n.path===leafPath || leafPath.startsWith(n.path+'/') || n.path==='@root'){n.allocated+=1024;n.apparent+=1024;}pending.push(...n.children);}
    leaf.omitted_entries=0;
    sample.revision=1;sample.updated_at=new Date().toISOString();
    state.jobs[0].status='completed';state.jobs[0].error=null;state.jobs[1].snapshot_revision=1;
  `);
  await run('syncState()');
  assert.equal(run('platform.loaded'),'a'.repeat(32),'history selection remains on original record');
  assert.equal(run('platform.followLatest'),false);
  assert.equal(run('snapshot.revision'),1);
  assert.equal(run('explorer.trail[explorer.trail.length-1].path'),run('leafPath'),'updated details preserve drilldown');
  assert.equal(element('directoryDepth').value,'8','saved depth survives published detail refreshes');
  assert(element('containerDialog').open);
  assert(element('directoryMap').innerHTML.includes('deeper'));
  assert(!element('resultLoadingDialog').open,'incremental updates never open the full result loading dialog');
  assert.equal(run('calls.filter(c=>c.path.endsWith("/snapshot")).length'),0,'expansion must never download the entire snapshot');
  assert.equal(run('calls.filter(c=>c.path.includes("/changes?revision=0")).length'),1);
  assert(run('snapshot.tree.children.includes(untouchedBranch)'),'unrelated directory trees are reused');
  assert.equal(run('usage.exclusive+usage.shared+usage.unrelated'),bytesBefore+1024,'global usage is rebuilt with the new tree');
  const reads=run('calls.filter(c=>c.path.includes("/changes?")).length');await run('syncState()');assert.equal(run('calls.filter(c=>c.path.includes("/changes?")).length'),reads,'unchanged revision does not reload');
  click({storageEntry:String(run(`explorer.entries.findIndex(e=>e.name==='deeper')`))});
  assert(element('directoryMap').innerHTML.includes('点击扫描并拆分'),'new deeper leaf can continue again');
  run('showEntry(explorer.entries.findIndex(e=>e.kind==="residual"));usage.nodes.get(explorer.trail[explorer.trail.length-1].path).omitted_entries=0;renderExplorer();showEntry(explorer.entries.findIndex(e=>e.kind==="residual"))');
  assert(!element('directorySelection').innerHTML.includes('data-expand-path'),'directory metadata alone does not offer continuation');
  click({storageCrumb:'0'});
  click({storageEntry:String(run(`explorer.entries.findIndex(e=>e.name==='actual-file')`))});
  assert(!element('directorySelection').innerHTML.includes('data-expand-path'),'real files do not offer continuation');
  // Zero-byte folded entries still provide a route to deeper directories.
  run(`var n=usage.nodes.get(leafPath);n.children=[];n.allocated=0;n.omitted_entries=3;renderExplorer();openDirectoryEntry(explorer.entries.findIndex(e=>e.name==='cache'))`);
  assert(element('mapUnmeasured').innerHTML.includes('点击扫描'));
  assert(element('mapUnmeasured').innerHTML.includes('点击扫描'),'zero-byte pending blocks remain clickable');
  run(`var deferred=usage.nodes.get(leafPath);deferred.omitted_entries=0;deferred.size_unknown=true;deferred.children=[];explorer.trail=explorer.trail.slice(0,1);renderExplorer();`);
  const unknownIndex=run(`explorer.entries.findIndex(e=>e.name==='cache')`);
  assert.equal(run(`explorer.entries[${unknownIndex}].bytes`),null,'unmeasured directory size is unknown, not zero');
  click({storageEntry:String(unknownIndex)});
  assert.equal(run('explorer.trail[explorer.trail.length-1].path'),run('leafPath'),'unknown directories remain navigable');
  assert(element('mapUnmeasured').innerHTML.includes('点击扫描'),'deferred directories can continue without an omitted-entry count');
  assert(element('mapUnmeasured').innerHTML.includes('点击扫描'));
  // The same publication flow supports folded host paths and keeps the host
  // selection open when the original history record gains a new revision.
  run(`
    sample.tree.children.push(entry('/host-cache',4096,{omitted_entries:8}));
    sample.tree.allocated+=4096;sample.tree.apparent+=4096;
    load(sample,'历史扫描结果');select('host');openDirectoryEntry(explorer.entries.findIndex(e=>e.path==='/host-cache'));
  `);
  assert(element('directoryMap').innerHTML.includes('点击扫描并拆分'));
  await run(`expandLeaf('/host-cache')`);
  assert(element('directoryStatus').innerHTML.includes('12 项已遍历'));
  assert.equal(run('state.active.config.incremental_path'),'/host-cache');
  run(`
    sample=JSON.parse(JSON.stringify(sample));var hostLeaf=Usage.build(sample).nodes.get('/host-cache');hostLeaf.children=[entry('/host-cache/content',2048)];hostLeaf.omitted_entries=0;
    sample.revision++;state.jobs[0].status='completed';state.jobs[1].snapshot_revision=sample.revision;state.active=null;
  `);
  await run('syncState()');
  assert.equal(run('selected===HOST'),true);
  assert.equal(run('explorer.trail[explorer.trail.length-1].path'),'/host-cache');
  assert(element('directoryMap').innerHTML.includes('content'));
  assert(element('containerDialog').open);
  // Reading completed analysis can fail independently of the scan. Retry the
  // same patch, preserving the tree and avoiding another scan or full download.
  run(`
    var priorSnapshot=snapshot;sample=JSON.parse(JSON.stringify(sample));sample.revision++;
    Usage.build(sample).nodes.get('/host-cache').omitted_entries=2;
    state.jobs[0].config.base_revision=snapshot.revision;state.jobs[1].snapshot_revision=sample.revision;
    rejectChanges=true;
  `);
  await run('syncState()');
  assert(run('snapshot===priorSnapshot'),'failed patch keeps the current data');
  assert(run('platform.changesError.message.includes("读取失败")'));
  const failedReads=run('calls.length');await run('syncState()');assert.equal(run('calls.length'),failedReads+1,'failed update waits for explicit retry');
  run('rejectChanges=false');
  await run('loadSnapshotChanges()');
  assert.equal(run('snapshot.revision'),run('sample.revision'));
  assert.equal(run('platform.changesError'),null);
  assert.equal(run('calls.filter(c=>c.path.endsWith("/snapshot")).length'),0);
  // A patch captured before a successful owner edit must not overwrite the
  // newly saved owner when its older metadata arrives afterwards.
  run(`pauseChanges=true;var revisionBeforeOwnerEdit=snapshot.revision;var ownerPatch=loadSnapshotChanges();var ownerPatchController=platform.changesLoad.controller;ownerId=snapshot.containers[0].id;$('ownerInput').value='saved-new-owner'`);
  element('ownerForm').listeners.submit({preventDefault(){}});
  await new Promise(setImmediate);
  assert(run('ownerPatchController.signal.aborted'),'saving ownership invalidates an in-flight metadata patch');
  assert.equal(run('snapshot.containers[0].owner'),'saved-new-owner');
  run('resolveChanges()');
  assert.equal(await run('ownerPatch'),false);
  assert.equal(run('snapshot.containers[0].owner'),'saved-new-owner','late metadata cannot undo a successful owner edit');
  assert.equal(run('snapshot.revision'),run('revisionBeforeOwnerEdit'),'aborted metadata does not advance the directory revision');
  // Repeated refreshes share one patch request, and session changes make late
  // responses harmless without replacing the current view.
  run(`pauseChanges=true;var beforeLate=snapshot;var firstPatch=loadSnapshotChanges();var duplicatePatch=loadSnapshotChanges()`);
  assert(run('firstPatch===duplicatePatch'));
  run('platform.generation++;resolveChanges()');
  assert.equal(await run('firstPatch'),false);
  assert(run('snapshot===beforeLate'));
  // Current API keeps the reusable directory worker outside scan history.
  run(`pauseChanges=false;state.directory_jobs=[state.jobs[0]];state.jobs=[baseJob];state.directory_jobs[0].status='running';state.active=state.directory_jobs[0];`);
  await run('syncState()');
  assert.equal(run('platform.history.length'),1,'directory activity must not add scan history');
  assert(!element('jobsBody').innerHTML.includes('目录扫描'));
  assert(element('directoryStatus').innerHTML.includes('data-cancel-expansion'),'separate worker state still supports live progress and stopping');
  run(`state.directory_jobs[0].status='failed';state.directory_jobs[0].error='扫描已停止';state.active=null;`);
  await run('syncState()');
  assert(element('directoryStatus').innerHTML.includes('点击灰块重试'));
  console.log('Incremental UI checks passed: leaf-only actions, permissions, escaped failures, retry, progress, duplicate prevention, revision refresh, preserved navigation and repeated drilldown.');
})().catch(error=>{console.error(error);process.exitCode=1;});
