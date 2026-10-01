// Settings behavior and navigation contracts, without external packages.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const html=fs.readFileSync('dist/index.html','utf8');
assert(html.includes('src="/settings.js"'));
assert(html.includes('data-page="scan-settings"'));
assert(html.includes('data-page="settings"'));
const elements=new Map();
function element(id){
  assert(html.includes(`id="${id}"`),`Missing DOM element ${id}`);
  if(!elements.has(id))elements.set(id,{value:'',checked:false,disabled:false,hidden:false,textContent:'',innerHTML:'',listeners:{},setAttribute(){},addEventListener(type,fn){this.listeners[type]=fn;},reset(){}});
  return elements.get(id);
}
let config={revision:1,value:{protocol:'responses',endpoint:'https://model.example/v1',model:'test-model',timeout_seconds:180,has_api_key:true}};
const requests=[];let accountLoads=0,failSave=false,pendingLoad=null,pendingSave=null;
const sandbox={console,window:{},$:element,platform:{user:{username:'admin',role:'admin'}},loadAccounts:async()=>{accountLoads++;},message(){},api:async(path,options={})=>{
  requests.push({path,options});
  assert.equal(path,'/api/agent/settings');
  if(options.method==='PUT'){
    if(pendingSave)return new Promise(resolve=>{pendingSave.resolve=resolve;});
    if(failSave)throw Error('配置已更新，请重新载入');
    const body=JSON.parse(options.body);assert.equal(body.revision,config.revision);
    const {api_key,clear_api_key,...value}=body.value;
    config={revision:config.revision+1,value:{...value,has_api_key:clear_api_key?false:!!api_key||config.value.has_api_key}};
  }else if(pendingLoad)return new Promise(resolve=>{pendingLoad.resolve=resolve;});
  return config;
}};
vm.createContext(sandbox);vm.runInContext(fs.readFileSync('dist/settings.js','utf8'),sandbox);
const run=code=>vm.runInContext(code,sandbox),flush=()=>new Promise(resolve=>setImmediate(resolve));
const submit=()=>element('agentSettingsForm').listeners.submit({preventDefault(){}});
(async()=>{
  run('window.SettingsUI.reset();window.SettingsUI.open()');await flush();
  assert.equal(accountLoads,1);assert.equal(element('settingsUsername').textContent,'admin');assert.equal(requests.length,0);
  run('window.SettingsUI.openModel()');await flush();assert.equal(element('agentModel').value,'test-model');assert(element('agentKeyState').textContent.includes('已保存'));assert.equal(element('agentKey').value,'');assert(!element('agentSaveSettings').disabled);
  element('agentReasoningSummary').checked=true;element('agentKey').value='new-secret';element('agentProtocol').value='completions';await submit();
  assert.equal(JSON.parse(requests[requests.length-1].options.body).value.api_key,'new-secret');assert.equal(config.value.protocol,'completions');assert.equal(element('agentKey').value,'');assert.equal(run('settingsState.model.revision'),2);
  await submit();assert(config.value.has_api_key,'blank Key must preserve stored credentials');
  assert.equal(config.value.reasoning_summary,true);
  element('agentClearKey').checked=true;await submit();assert(!config.value.has_api_key);assert(!element('agentClearKey').checked);
  failSave=true;await submit();assert(element('agentSettingsError').textContent.includes('重新载入'));assert(!element('agentSaveSettings').disabled);failSave=false;
  pendingLoad={};const loading=run('loadModelSettings()');assert(element('agentSaveSettings').disabled);run('window.SettingsUI.reset()');pendingLoad.resolve(config);await loading;pendingLoad=null;assert.equal(run('settingsState.model'),null);assert.equal(element('agentKey').value,'');assert.equal(element('agentSettingsStatus').textContent,'');
  await run('loadModelSettings()');pendingSave={};element('agentKey').value='temporary-key';const saving=submit();assert(element('agentSaveSettings').disabled);const before=requests.length;await submit();assert.equal(requests.length,before,'double submission must not write twice');run('window.SettingsUI.reset()');pendingSave.resolve(config);await saving;pendingSave=null;assert.equal(run('settingsState.model'),null);assert.equal(element('agentKey').value,'');assert.equal(element('agentSettingsStatus').textContent,'');
  sandbox.platform.user={username:'reader',role:'viewer'};const count=requests.length,accounts=accountLoads;run('window.SettingsUI.open();window.SettingsUI.openModel()');await submit();assert.equal(requests.length,count);assert.equal(accountLoads,accounts);assert.equal(element('settingsRole').textContent,'只读');
  console.log('Settings checks passed: account/model separation, write-only Key, preserve/clear, save errors, viewer restrictions, duplicate submission and stale responses after logout.');
})().catch(error=>{console.error(error);process.exitCode=1;});
