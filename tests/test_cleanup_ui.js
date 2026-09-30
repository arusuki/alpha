// Scope switching with one report per scope, including prepared cleanup records.
'use strict';
const assert=require('assert'),fs=require('fs');
const html=fs.readFileSync('dist/index.html','utf8'),elements=new Map(),calls=[];
function element(id){
  assert(html.includes(`id="${id}"`),`Missing element ${id}`);
  if(!elements.has(id))elements.set(id,{value:'',textContent:'',disabled:false,listeners:{},
    set innerHTML(value){this.content=value;if(id==='cleanupReport'||id==='cleanupHostReport')this.value=value.match(/value="([^"]*)"/)[1];},
    get innerHTML(){return this.content||'';},
    addEventListener(name,fn){this.listeners[name]=fn;},setAttribute(name,value){this[name]=value;},replaceChildren(){},
    click(){if(!this.disabled)return this.listeners.click();}});
  return elements.get(id);
}
const reports=['host','container'].map((scope,i)=>({report_id:i+1,report_scope:scope,cleanup_id:scope,cleanup_status:'ready',title:scope,snapshot_id:'scan'}));
const results=Object.fromEntries(reports.map(report=>[report.cleanup_id,{cleanup:{id:report.cleanup_id,status:'ready',snapshot_id:'scan'},entries:[{id:report.cleanup_id,path:'/srv/'+report.cleanup_id,category:1,summary:'cache',status:'pending',detail:JSON.stringify({bytes:4096})}]}]));
let hold=null;
const env={$:element,document:{querySelectorAll:()=>[]},window:{},platform:{user:{role:'admin'}},esc:String,fmt:String,dateTime:String,setTimeout:()=>1,clearTimeout(){},api:async(path,options)=>{
  calls.push({path,options});
  if(path==='/api/agent/cleanup-reports')return {reports:reports.map(row=>({...row}))};
  if(path.startsWith('/api/agent/cleanups/')){if(hold){const wait=hold;hold=null;await wait;}return results[path.split('/').pop()];}

  throw Error('Unexpected API '+path);
}};
const ui=new Function(...Object.keys(env),fs.readFileSync('dist/cleanup.js','utf8')+'\nreturn {view:cleanupView,load:loadCleanupReports,apply:cleanupApply};')(...Object.values(env));
(async()=>{
  await ui.load();
  assert.equal(ui.view.reportScope,'container');
  assert(element('cleanupPrepare').disabled);
  assert(!element('cleanupHostPrepare').disabled,'inactive report must remain accessible');
  assert.equal(element('cleanupHostReport').value,'1');
  assert.equal(element('cleanupReport').value,'2');
  await element('cleanupHostPrepare').click();
  assert.equal(ui.view.reportScope,'host');
  assert.equal(ui.view.entries[0].id,'host');
  assert(element('cleanupHostPrepare').disabled);
  await element('cleanupPrepare').click();
  assert.equal(ui.view.entries[0].id,'container');
  assert(!calls.some(call=>call.options?.method==='POST'),'viewing existing entries must not prepare again');

  // Fresh reads update the cached status before switching away.
  ui.view.reports[1].cleanup_status='running';
  ui.apply(results.container);
  assert.equal(ui.view.reports[1].cleanup_status,'ready');

  // Loading a scope blocks other actions until its entries have arrived.
  let release;
  hold=new Promise(resolve=>{release=resolve;});
  const switching=element('cleanupHostPrepare').click();
  assert(element('cleanupPrepare').disabled);
  assert(element('cleanupHostPrepare').disabled);
  await element('cleanupPrepare').click();
  release();await switching;
  assert.equal(ui.view.reportScope,'host');
  assert.equal(ui.view.entries[0].id,'host');
  assert(!calls.some(call=>call.options?.method==='POST'));
  console.log('Cleanup UI checks passed: prepared scopes, cached state, loading guard.');
})().catch(error=>{throw error;});
