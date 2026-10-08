// Frontend API/state contract tests. Runs without a browser or external packages.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const elements=new Map();
function element(id){
  if(!elements.has(id))elements.set(id,{id,value:'',hidden:false,checked:false,disabled:false,innerHTML:'',textContent:'',dataset:{},style:{},attributes:{},listeners:{},querySelector(){return {focus(){}};},classList:{values:new Set(),toggle(name,on){if(on)this.values.add(name);else this.values.delete(name);},remove(name){this.values.delete(name);}},setAttribute(name,value){this.attributes[name]=value;},removeAttribute(name){delete this.attributes[name];},addEventListener(type,fn){this.listeners[type]=fn;},reset(){},showModal(){this.open=true;},close(){this.open=false;}});
  return elements.get(id);
}
const pageIds=['overview','history','scan-settings','settings','dashboard','processes','agent-settings'];
const pages=pageIds.map(p=>element('page-'+p));
const nav=pageIds.map(p=>{const e=element('nav-'+p);e.dataset.page=p;return e;});
const document={getElementById:element,addEventListener(){},querySelectorAll(selector){if(selector==='.platform-page')return pages;if(selector==='.platform-nav [data-page]')return nav;if(selector==='[data-admin]')return [element('startScan'),element('cancelScan'),nav[2]];return [];}};
class TestAbortController {
  constructor(){this.signal={aborted:false,listeners:[],addEventListener(type,fn){this.listeners.push(fn);},removeEventListener(type,fn){this.listeners=this.listeners.filter(f=>f!==fn);}};}
  abort(){this.signal.aborted=true;this.signal.listeners.slice().forEach(fn=>fn());}
}
const sandbox={console,EventSource:class {addEventListener(){} close(){}},document,window:{location:{pathname:'/nodes/'+ 'e'.repeat(32)+'/'},addEventListener(){},SettingsUI:{reset(){},open(){}}},AbortController:TestAbortController,requestAnimationFrame:fn=>setImmediate(fn),setTimeout:()=>1,clearTimeout(){},fetch:()=>new Promise(()=>{})};
vm.createContext(sandbox);
for(const path of ['dist/usage.js','dist/snapshot.js','dist/app.js','dist/snapshot-loader.js','dist/platform.js'])vm.runInContext(fs.readFileSync(path,'utf8'),sandbox);
assert.equal(vm.runInContext("apiURL('/api/gpu/overview?hours=6&step=60')",sandbox),
  '/api/cluster/nodes/'+'e'.repeat(32)+'/api/gpu/overview?hours=6&step=60',
  'GPU requests must use the selected node proxy and preserve time parameters');
sandbox.sampleText=fs.readFileSync('tests/fixtures/snapshot.json','utf8');
vm.runInContext(`
apiURL=path=>path;
var sample=JSON.parse(sampleText);sample.containers.forEach(c=>c.label_owner=c.owner);sample.job_id='a'.repeat(32);
var calls=[];
var job={id:'a'.repeat(32),created_at:100,status:'completed',trigger:'manual',created_by:'admin',snapshot_revision:0,allocated:123,progress:{phase:'completed',entries:12},finished_at:101};
var config={revision:1,value:{root:['/srv'],exclude:[],no_docker:true,include_docker_root:false,max_depth:5,max_nodes:50000,docker_timeout:120,owner_label:'project-alpha.owner',interval_minutes:0,scan_backend:'auto',scan_mode:'normal',schedule_mode:'off',schedule_times:[],schedule_weekdays:[0,1,2,3,4,5,6],schedule_timezone:'UTC',retain_records:0}};
var responses={'/api/state':{jobs:[job],directory_jobs:[],directory_jobs:[],latest_id:job.id,active:null,interval_minutes:0},['/api/jobs/'+job.id+'/view']:sample};
`,sandbox);
vm.runInContext(`
responses['/api/settings']=config;
api=async function(path,options={}){
  calls.push({path,options});
  if(path==='/api/settings' && options.method==='PUT'){config={revision:2,value:JSON.parse(options.body).value};responses[path]=config;return config;}
  if(path==='/api/owners'){sample.containers[0].owner='alice';return {owners:{[sample.containers[0].id]:'alice'}};}
  if(path==='/api/logout')return {ok:true};
  if(!(path in responses))throw Error('Unexpected API '+path);
  return responses[path];
};
SnapshotLoader.read=async function(path){const data=await api(path.split('?')[0]);validate(data);return {data,usage:Usage.build(data)};};`,sandbox);
const run=code=>vm.runInContext(code,sandbox);
const flush=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
  run('showAuth(true)');assert(element('authTitle').textContent.includes('初始化'));assert(element('console').hidden);
  await run('enter({user:{id:"admin-id",username:"admin",role:"admin"},csrf:"test"})');
  assert.equal(run('platform.page'),'dashboard');assert.equal(run('platform.loaded'),null);
  assert(!run('calls.some(c=>c.path.endsWith("/view"))'),'login must not download a snapshot');
  run('showPage("overview")');await run('syncState()');if(run('platform.resultLoad'))await run('platform.resultLoad.promise');
  assert(!element('console').hidden);assert(!element('resultContent').hidden);assert.equal(run('platform.loaded'),'a'.repeat(32));
  run("snapshot.filesystems.push({device:'virtual',mount:'/run',fs:'tmpfs',total:50*1024**3,used:0,available:50*1024**3,scanned:0,unexplained:0});renderScanSummary(0)");
  assert.equal(element('taskCapacityNote').textContent,`${run('snapshot.filesystems[0].mount')} 可用 ${run('fmt(snapshot.filesystems[0].available)')}`);
  run('snapshot.filesystems.pop();renderScanSummary(0)');
  run(`var savedDisks=snapshot.filesystems,savedDocker=snapshot.docker;
snapshot.filesystems=[{device:'root',mount:'/',fs:'ext4',total:1000,used:900,available:50},{device:'data',mount:'/data',fs:'ext4',total:2000,used:100,available:1800}];
snapshot.docker={root:'/var/lib/docker',root_canonical:'/var/lib/docker'};renderScanSummary(0);`);
  assert.equal(element('taskTarget').textContent,'Docker 所在文件系统 / · 可用 50 B');
  assert(element('taskCapacityNote').textContent.includes('/ 可用 50 B · /data 可用 1.76 KiB'));
  assert(element('taskCapacityNote').textContent.includes('空闲空间不可互用'));
  run('snapshot.filesystems=savedDisks;snapshot.docker=savedDocker;renderScanSummary(0)');
  assert(element('jobsBody').innerHTML.includes('已完成'));
  run('showPage("scan-settings")');await flush();assert.equal(element('cfgRoots').value,'/srv');
  assert.equal(element('cfgScanBackend').value,'auto','saved scan backend must be displayed');
  assert.equal(element('cfgScanMode').value,'normal','saved scan mode must be displayed');
  element('cfgRoots').value='/data/models\n/data/datasets';element('cfgDocker').checked=false;
  element('settingsForm').listeners.submit({preventDefault(){}});await flush();
  assert.equal(run('platform.config.revision'),2);assert.equal(run('platform.config.value.root.length'),2);
  assert.equal(run('platform.config.value.scan_mode'),'normal');
  assert.equal(run('calls.filter(c=>c.options.method==="PUT")[0].path'),'/api/settings');
  element('cfgDocker').checked=true;element('cfgScanBackend').value='docker';
  element('cfgScanMode').value='fast';
  element('settingsForm').listeners.submit({preventDefault(){}});await flush();
  assert.equal(run('platform.config.value.scan_backend'),'docker');assert.equal(run('platform.config.value.no_docker'),false);
  assert.equal(run('platform.config.value.scan_mode'),'fast');
  element('cfgScanMode').value='normal';await run('loadSettings()');
  assert.equal(element('cfgScanMode').value,'fast','saved scan mode must survive reload');
  element('cfgScheduleMode').value='calendar';run('scanScheduleControls()');
  assert(element('cfgInterval').disabled);assert(!element('cfgCalendarFields').hidden);
  element('cfgScheduleTimes').value='02:00\n14:30';element('cfgScheduleTimezone').value='Asia/Shanghai';element('cfgRetainRecords').value='3';
  element('cfgWeekday0').checked=false;
  element('settingsForm').listeners.submit({preventDefault(){}});await flush();await run('loadSettings()');
  assert.equal(run('platform.config.value.schedule_mode'),'calendar');assert.equal(run('platform.config.value.retain_records'),3);
  assert.equal(element('cfgScheduleTimes').value,'02:00\n14:30');assert(!element('cfgWeekday0').checked);
  run('platform.schedule=platform.config.value;renderTask(0)');assert(element('scheduleStatus').textContent.includes('Asia/Shanghai'));
  run('platform.followLatest=false;platform.loaded="historical"');await run('syncState()');assert.equal(run('platform.loaded'),'historical');
  run('platform.active={id:"a".repeat(32),status:"running"};controls()');assert(element('startScan').disabled);assert(!element('cancelScan').hidden);
  run(`platform.active={id:'progress',status:'running',trigger:'agent-full',created_at:Date.now()/1000-65,progress:{phase:'host',entries:1000,allocated:256*1024**3,capacity_known:true,capacity_total:1024**4,capacity_used:768*1024**3,containers_total:4,containers_done:1,containers_remaining:3,current_containers:[],path:'/srv/data'}};renderTask(0)`);
  assert.equal(element('taskPhase').textContent,'扫描 host');
  assert.equal(element('taskFill').style.width,'25%');assert.equal(element('taskUsedFill').style.width,'75%');
  assert.equal(element('taskMeter').attributes['aria-valuenow'],'25');
  run(`platform.active.progress.capacity_filesystems=[{device:'root',mount:'/',fs:'ext4',available:50},{device:'data',mount:'/data',fs:'ext4',available:1800}];renderTask(0)`);
  assert(element('taskCapacityNote').textContent.includes('/ 可用 50 B · /data 可用 1.76 KiB'));
  assert(element('taskContainers').textContent.includes('剩余 3 个'));assert(element('taskProgress').textContent.includes('1 分 5 秒'));
  run(`platform.active.progress={...platform.active.progress,phase:'container',allocated:512*1024**3,current_containers:[{id:'a',name:'alice-train'}]};renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert(element('taskTarget').textContent.includes('alice-train'));
  run(`platform.active.progress.current_containers.push({id:'b',name:'bob-notebook'});renderTask(0)`);
  assert(element('taskTarget').textContent.includes('共享存储'));assert(element('taskTarget').textContent.includes('bob-notebook'));
  run(`platform.active.progress.phase='saving';platform.active.progress.current_containers=[];platform.active.progress.containers_done=4;renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert.equal(element('taskPhase').textContent,'保存结果');
  run(`platform.active.status='cancelling';renderTask(0)`);
  assert.equal(element('taskPhase').textContent,'正在取消');assert(!element('taskMonitor').classList.values.has('busy'));
  run(`platform.active.status='failed';platform.active.error='无法读取目录';renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert.equal(element('taskPath').textContent,'无法读取目录');
  run(`platform.active.status='completed';renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%','completion must preserve measured capacity ratio');
  assert.equal(element('taskPhase').textContent,'已完成');
  run(`platform.active={id:'new-job',status:'running',created_at:Date.now()/1000,progress:{}}`);
  run(`platform.active.progress={phase:'discovering',preparation_done:3,preparation_total:null,containers_total:5,containers_discovered:0,path:'正在获取 Docker 数据卷列表'};renderTask(0)`);
  assert.equal(element('taskFill').style.width,'0%');assert(!('aria-valuenow' in element('taskMeter').attributes));
  assert(element('taskMeter').classList.values.has('indeterminate'));assert.equal(element('taskScanned').textContent,'3 项');
  run(`platform.active.progress={phase:'discovering',preparation_done:4,preparation_total:10,containers_total:5,containers_discovered:0,current_containers:[{id:'abc',name:'alice-train'}],path:'正在读取容器 alice-train 的配置与可写层大小（1 / 5）'};renderTask(0)`);
  assert.equal(element('taskFill').style.width,'40%');assert(!element('taskMeter').classList.values.has('indeterminate'));
  assert(element('taskTarget').textContent.includes('容器 alice-train'));assert.equal(element('taskTarget').title,'alice-train');
  assert(!element('taskTarget').textContent.includes('abc'));assert(!element('taskPath').textContent.includes('abc'));
  assert.equal(element('taskPercent').textContent,'本阶段 40%');assert(element('taskCapacityNote').textContent.includes('不代表耗时比例'));
  assert(element('taskProgress').textContent.includes('尚未开始文件遍历'));assert(!element('taskMeter').attributes['aria-valuetext'].includes('已扫描'));
  run(`platform.active.progress.preparation_done=5;platform.active.progress.containers_discovered=1;renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert(element('taskContainers').textContent.includes('1 / 5'));
  run(`platform.active.status='cancelling';renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert.equal(element('taskPhase').textContent,'正在取消');
  assert(!element('taskMonitor').classList.values.has('busy'));
  run(`platform.active.status='failed';platform.active.error='Docker 查询超时';renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert.equal(element('taskPath').textContent,'Docker 查询超时');
  run(`platform.active.status='running';delete platform.active.error;platform.active.progress={phase:'preparing',preparation_done:1,preparation_total:2};renderTask(0)`);
  assert.equal(element('taskFill').style.width,'50%');assert.equal(element('taskPhase').textContent,'准备扫描环境');
  run(`platform.active.progress={...platform.active.progress,phase:'host',allocated:25,capacity_total:100,capacity_used:75};renderTask(0)`);
  assert.equal(element('taskCountLabel').textContent,'累计已扫描');assert.equal(element('taskFill').style.width,'25%');
  assert.equal(element('taskUsedFill').style.width,'75%');assert.equal(element('taskPercent').textContent,'占总容量 25%');
  assert.equal(element('taskMeter').attributes['aria-label'],'已扫描空间占文件系统总容量');
  run(`platform.active={id:'queued-job',status:'queued',created_at:Date.now()/1000,progress:{}};renderTask(0)`);
  assert(element('taskMeter').classList.values.has('indeterminate'));assert.equal(element('taskScanned').textContent,'0 项');
  run(`platform.active.status='running'`);
  run(`platform.active.progress={phase:'scanning',allocated:123};renderTask(0)`);
  assert.equal(element('taskPercent').textContent,'容量未知');assert(!element('taskPercent').textContent.includes('NaN'));
  run(`platform.active.progress={phase:'host',allocated:120,capacity_total:100,containers_total:0};renderTask(0)`);
  assert.equal(element('taskFill').style.width,'100%');assert.equal(element('taskMeter').attributes['aria-valuenow'],'100');
  assert(element('taskPercent').textContent.includes('120%'));assert(element('taskContainers').textContent.includes('无容器'));
  run('platform.active=null;select("container",snapshot.containers[0])');assert(element('detail').innerHTML.includes('data-edit-owner'));
  run('platform.loaded=snapshot.job_id;ownerId=snapshot.containers[0].id');element('ownerInput').value='alice';
  element('ownerForm').listeners.submit({preventDefault(){}});await flush();
  assert(run('usage.owners.has("alice")'));assert(element('detail').innerHTML.includes('alice'));
  run('showPage("overview")');await run('syncState()');
  await run('loadJob(job.id,true)');
  // Deletion requires an explicit confirmation and preserves results on failure.
  run('renderHistory()');assert(element('jobsBody').innerHTML.includes('data-delete-job'));
  run('openDeleteJob(job.id)');assert(element('deleteJobDialog').open);
  const beforeDelete=run('calls.length');element('closeDeleteJob').listeners.click();
  assert(!element('deleteJobDialog').open);assert.equal(run('calls.length'),beforeDelete);
  run(`var originalAPI=api;api=async(path,options={})=>{if(options.method==='DELETE')throw Error('记录仍在使用');return originalAPI(path,options);};openDeleteJob(job.id)`);
  await run('deleteJob()');assert(element('deleteJobDialog').open);assert.equal(element('deleteJobError').textContent,'记录仍在使用');
  assert.equal(run('platform.loaded'),run('job.id'));assert(!element('confirmDeleteJob').disabled);
  run(`
    var olderJob={...job,id:'b'.repeat(32),created_at:90};
    responses['/api/jobs/'+olderJob.id+'/view']={...sample,job_id:olderJob.id};
    api=async(path,options={})=>{
      if(options.method==='DELETE'){
        calls.push({path,options});
        var id=path.split('/').pop();
        responses['/api/state']={jobs:id===job.id?[olderJob]:[],directory_jobs:[],latest_id:id===job.id?olderJob.id:null,active:null,interval_minutes:0};
        return {deleted_ids:[id],cleanup_pending:false};
      }
      return originalAPI(path,options);
    };
  `);
  await run('deleteJob()');assert(!element('deleteJobDialog').open);
  assert.equal(run('platform.loaded'),run('olderJob.id'));assert.equal(run('platform.history.length'),1);
  assert(!element('jobsBody').innerHTML.includes('data-delete-job="'+run('job.id')+'"'));
  run('openDeleteJob(olderJob.id)');await run('deleteJob()');
  assert.equal(run('platform.loaded'),null);assert.equal(run('snapshot'),null);
  assert(element('resultContent').hidden);assert(!element('firstScan').hidden);
  assert(element('jobsBody').innerHTML.includes('还没有扫描记录'));assert(element('latestResult').hidden);
  run(`responses['/api/state']={jobs:[job],directory_jobs:[],directory_jobs:[],latest_id:job.id,active:null,interval_minutes:0}`);
  await run('syncState()');assert.equal(run('platform.history.length'),0,'stale polling must not restore deleted records');
  run('api=originalAPI;responses["/api/state"]={jobs:[job],directory_jobs:[],directory_jobs:[],latest_id:job.id,active:null,interval_minutes:0}');await run('enter({user:{id:"admin-id",username:"admin",role:"admin"},csrf:"test"})');
  run('showPage("overview")');await run('syncState()');if(run('platform.resultLoad'))await run('platform.resultLoad.promise');
  run('platform.user.role="viewer";renderHistory()');assert(!element('jobsBody').innerHTML.includes('data-delete-job'));
  run('platform.user.role="viewer";select("container",snapshot.containers[0]);showPage("overview");showPage("scan-settings")');assert.equal(run('platform.page'),'overview');assert(!element('detail').innerHTML.includes('data-edit-owner'));
  run('platform.nodeID="";showPage("settings")');assert.equal(run('platform.page'),'settings');assert(element('scanActions').hidden);assert.equal(element('pageTitle').textContent,'设置');
  run('showPage("agent-settings")');assert.equal(run('platform.page'),'settings');
  run('platform.nodeID="e".repeat(32);showPage("overview")');assert(!element('scanActions').hidden);assert.equal(element('pageTitle').textContent,'空间用量');
  run(`
    var previousSnapshot=snapshot,previousLoaded=platform.loaded;
    var pendingReads=[];
    SnapshotLoader.read=function(path,options){return new Promise((resolve,reject)=>pendingReads.push({path,options,resolve,reject}));};
    showPage('history');
    var cancelledRead=loadJob('cancel-test',true);
    pendingReads[0].options.onProgress({stage:'download',done:0,total:null,bytes:123456,detail:'读取展示数据'});
  `);
  assert(element('resultLoadingDialog').open);assert.equal(element('resultLoadingPhase').textContent,'读取展示数据');
  assert(!('aria-valuenow' in element('resultLoadingMeter').attributes));
  run(`pendingReads[0].options.onProgress({stage:'aggregate',done:200,total:1000,unit:'节点',detail:'汇总空间用量'})`);
  assert.equal(element('resultLoadingPercent').textContent,'20%');assert(element('resultLoadingCount').textContent.includes('200 节点 / 1,000 节点'));
  element('cancelResultLoading').listeners.click();
  assert(run('pendingReads[0].options.signal.aborted'));
  run(`pendingReads[0].resolve({data:sample,usage:Usage.build(sample)})`);
  assert.equal(await run('cancelledRead'),false);
  assert(run('snapshot===previousSnapshot && platform.loaded===previousLoaded'));assert.equal(run('platform.page'),'history');
  assert(!element('resultLoadingDialog').open);
  run(`responses['/api/state'].latest_id='cancel-test';platform.followLatest=true;`);
  await run('syncState()');assert.equal(run('pendingReads.length'),1,'poll must not restart a cancelled result');
  run(`var staleRead=loadJob('stale-test',true);var newerRead=loadJob('newer-test',true);pendingReads[1].resolve({data:sample,usage:Usage.build(sample)});`);
  assert.equal(await run('staleRead'),false);assert(element('resultLoadingDialog').open);
  run(`pendingReads[2].resolve({data:sample,usage:Usage.build(sample)});`);
  assert.equal(await run('newerRead'),true);assert.equal(run('platform.loaded'),'newer-test');assert(!element('resultLoadingDialog').open);
  run(`var failedRead=loadJob('failed-test',true);pendingReads[3].reject(Error('无效的 JSON'));`);
  await assert.rejects(run('failedRead'),/无效的 JSON/);assert.equal(run('platform.loaded'),'newer-test');assert(!element('resultLoadingDialog').open);
  run(`var logoutRead=loadJob('logout-test',true);`);
  run('showAuth(false)');assert(element('console').hidden);assert.equal(run('platform.csrf'),'');
  run(`pendingReads[4].resolve({data:sample,usage:Usage.build(sample)});`);
  assert.equal(await run('logoutRead'),false);assert.equal(run('platform.loaded'),'newer-test');
  run(`platform.user={username:'admin',role:'admin'};platform.page='history';platform.history=[job];platform.loaded=job.id;platform.followLatest=false;platform.deletedIDs=new Set();responses['/api/state']={jobs:[],directory_jobs:[],latest_id:null,active:null,interval_minutes:0,schedule_mode:'off',history_floor:null};`);
  await run('syncState()');assert.equal(run('platform.history.length'),0);assert.equal(run('platform.loaded'),null);assert.equal(run('snapshot'),null);
  console.log('Platform frontend checks passed: setup/login state, backend results, settings save, historical selection, active-job controls, viewer restrictions and logout.');
  console.log('Result loading checks passed: phase/count display, cancellation, preserved results, retry suppression, stale completion, errors and logout.');
})().catch(error=>{console.error(error);process.exitCode=1;});
