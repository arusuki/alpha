// Report lifecycle, untrusted Markdown and asynchronous session isolation.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const html=fs.readFileSync('dist/index.html','utf8'),elements=new Map();
function element(id){
  assert(html.includes(`id="${id}"`),`Missing element ${id}`);
  if(!elements.has(id))elements.set(id,{id,value:'',textContent:'',innerHTML:'',hidden:false,disabled:false,open:false,listeners:{},addEventListener(k,fn){this.listeners[k]=fn;},insertAdjacentHTML(_,text){this.innerHTML+=text;},showModal(){this.open=true;},close(){this.open=false;this.listeners.close?.();},focus(){}});
  return elements.get(id);
}
const record='b'.repeat(32),id='a'.repeat(32),calls=[];
let sessions=[],messages=[],hold=null,settings={model:'test',endpoint:'http://model.test'},failure=null,download=null;
const session={id,title:'空间消耗总报告',status:'completed',snapshot_id:record,model:'test',created_at:1,updated_at:2};
const sandbox={console,window:{},$:element,platform:{user:{role:'admin'},loaded:record,followLatest:true},snapshot:{revision:7,host:'host',finished_at:'2026-09-14'},document:{querySelectorAll:()=>[],createElement:()=>({click(){}})},URL:{createObjectURL(blob){download=blob;return 'blob:test';},revokeObjectURL(){}},Blob,setTimeout:()=>1,clearTimeout(){},dateTime:String,showPage(){},esc:value=>String(value).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),api:async(path,options={})=>{
  calls.push({path,options});if(hold&&hold.path===path)return new Promise(resolve=>{hold.resolve=resolve;});
  if(failure&&path===failure.path)throw Error(failure.message);
  if(path==='/api/agent/settings')return {value:settings};
  if(path==='/api/agent/sessions')return {sessions:[...sessions]};
  if(path==='/api/agent/reports'){
    assert.deepEqual(JSON.parse(options.body),{snapshot_id:record,revision:7});sessions=[{...session}];
    messages=[{id:1,role:'user',content:'server report request'},{id:2,role:'tool_call',tool_name:'scan_directory',content:'{"path":"/data/<unsafe>"}'},{id:3,role:'assistant',created_at:3,content:'# 总报告\n\n| 路径 | 大小 |\n| --- | --- |\n| `/data` | **1 GiB** |\n\n<script>alert(1)</script>\n![x](https://example.test/x)'}];
    return {...session};
  }
  if(path.endsWith('/messages')){messages.push({id:messages.length+1,role:'user',content:JSON.parse(options.body).message},{id:messages.length+2,role:'assistant',content:'已进一步分析'});return {...session};}
  if(path.endsWith('/cancel')){session.status='cancelled';return {ok:true};}
  if(path.startsWith('/api/agent/sessions/'+id+'?after=')){
    const after=Number(path.split('after=')[1]),batch=messages.filter(m=>m.id>after).slice(0,2);
    return {session:{...session},messages:batch,next_after:batch.at(-1)?.id||after,has_more:messages.some(m=>m.id>(batch.at(-1)?.id||after)),active_job:null};
  }
  throw Error('Unexpected API '+path);
}};
vm.createContext(sandbox);vm.runInContext(fs.readFileSync('dist/agent.js','utf8'),sandbox);
const run=code=>vm.runInContext(code,sandbox),flush=()=>new Promise(resolve=>setImmediate(resolve));
(async()=>{
  run('agentControls()');assert(!element('generateReport').disabled);
  await run('generateDiskReport()');
  assert.equal(calls.filter(c=>c.path==='/api/agent/reports').length,1);
  assert(element('agentDialog').open);assert(element('agentReports').innerHTML.includes('<table>'));assert(element('agentReports').innerHTML.includes('<strong>1 GiB</strong>'));
  assert(!element('agentReports').innerHTML.includes('<script>'));assert(!element('agentReports').innerHTML.includes('<img'));assert(element('agentReports').innerHTML.includes('&lt;script&gt;'));
  assert(element('agentActivityLog').innerHTML.includes('/data/&lt;unsafe&gt;'));assert(!element('downloadReport').disabled);assert(!element('agentFollowup').hidden);
  const count=run('agentView.messages.length');await run('readAgentSession()');assert.equal(run('agentView.messages.length'),count,'poll must not repeat messages');
  await run('postAgentAction("messages",{message:"继续查看缓存"})');assert(element('agentReports').innerHTML.includes('继续查看缓存'));assert(element('agentReports').innerHTML.includes('已进一步分析'));
  run('downloadAgentReport()');const exported=await download.text();assert(exported.includes('# 总报告'));assert(exported.includes('继续查看缓存'));assert(exported.includes(record));assert(!exported.includes('server report request'));
  const unsafe=run('reportMarkdown("```html\\n<img src=x onerror=alert(1)>\\n```\\n\\n| A | B |\\n| --- | --- |\\n| `/a\\\\|b` | 1 |")');assert(!unsafe.includes('<img'));assert(unsafe.includes('/a|b'));
  sandbox.platform.loaded='c'.repeat(32);run('renderAgentSession()');assert(element('agentReportSource').textContent.includes('不同'));sandbox.platform.loaded=record;
  session.status='running';sessions=[{...session}];const before=calls.filter(c=>c.path==='/api/agent/reports').length;await run('generateDiskReport()');assert.equal(calls.filter(c=>c.path==='/api/agent/reports').length,before,'resume active report instead of creating duplicate');assert(element('sendAgentQuestion').disabled);assert(!element('stopAgent').hidden);
  await run('postAgentAction("cancel",{})');assert(element('stopAgent').hidden);assert(!element('sendAgentQuestion').disabled);
  session.snapshot_id=null;await run('readAgentSession()');assert(element('sendAgentQuestion').disabled);assert(!element('downloadReport').disabled,'report remains exportable after record deletion');session.snapshot_id=record;
  run('window.AgentUI.reset()');assert.equal(element('agentReports').innerHTML,'');assert(!element('agentDialog').open);
  sessions=[];settings.model='';await run('generateDiskReport()');assert(element('agentError').textContent.includes('模型设置'));assert.equal(calls.filter(c=>c.path==='/api/agent/reports').length,before);settings.model='test';
  failure={path:'/api/agent/reports',message:'扫描记录已更新，请刷新'};await run('generateDiskReport()');assert(element('agentError').textContent.includes('扫描记录已更新'));assert(!element('generateReport').disabled);failure=null;
  hold={path:'/api/agent/reports'};const creating=run('generateDiskReport()');await flush();const n=calls.length;await run('generateDiskReport()');assert.equal(calls.length,n,'duplicate clicks must not post twice');run('window.AgentUI.reset()');hold.resolve(session);await creating;hold=null;assert.equal(run('agentView.session'),null,'creation response after logout must be discarded');assert.equal(element('agentReports').innerHTML,'');
  await run('selectAgentSession('+JSON.stringify(session)+')');
  hold={path:`/api/agent/sessions/${id}?after=${run('agentView.cursor')}`};const reading=run('readAgentSession()');run('window.AgentUI.reset()');hold.resolve({session,messages:[{id:50,role:'assistant',content:'private old response'}],next_after:50,has_more:false});await reading;hold=null;assert.equal(element('agentReports').innerHTML,'');
  sandbox.platform.user={role:'viewer'};const total=calls.length;run('agentControls()');await run('generateDiskReport()');await run('openAgentReports()');assert(element('generateReport').disabled);assert.equal(calls.length,total);
  console.log('Agent UI checks passed: selected-record reports, paging, Markdown escaping, followups, exports, progress/cancel, deleted records, configuration errors, duplicate clicks and stale responses after logout.');
})().catch(error=>{console.error(error);process.exitCode=1;});
