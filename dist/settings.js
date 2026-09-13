'use strict';
const settingsState={model:null,section:'accounts',epoch:0,loading:0,saving:false};
function modelSettingsError(error){$('agentSettingsError').textContent=error ? error.message||String(error) : '';}
function renderModelSettings(result){
  settingsState.model=result;const c=result.value;
  $('agentProtocol').value=c.protocol;$('agentEndpoint').value=c.endpoint;$('agentModel').value=c.model;
  $('agentRounds').value=c.max_rounds;$('agentTimeout').value=c.timeout_seconds;
  $('agentKey').value='';$('agentClearKey').checked=false;
  $('agentKeyState').textContent=c.has_api_key?'已保存 Key；留空保留。浏览器不会读取已保存的 Key。':'尚未保存 Key；无需认证的本地服务可留空。';
  $('agentSettingsStatus').textContent='模型配置已载入';
}
function modelSettingsControls(){
  $('agentSaveSettings').disabled=!settingsState.model||!!settingsState.loading||settingsState.saving;
  $('agentReloadSettings').disabled=!!settingsState.loading||settingsState.saving;
}
async function loadModelSettings(){
  if(settingsState.saving||!platform.user||platform.user.role!=='admin')return;
  const epoch=settingsState.epoch,sequence=++settingsState.loading;modelSettingsError('');modelSettingsControls();
  try{const result=await api('/api/agent/settings');if(epoch===settingsState.epoch && sequence===settingsState.loading)renderModelSettings(result);}
  catch(error){if(epoch===settingsState.epoch)modelSettingsError(error);}
  finally{if(epoch===settingsState.epoch && sequence===settingsState.loading){settingsState.loading=0;modelSettingsControls();}}
}
function showSettingsSection(section){
  if(!platform.user||!['accounts','model'].includes(section))return;
  if(section==='model' && platform.user.role!=='admin')return;
  settingsState.section=section;
  $('settings-accounts').hidden=section!=='accounts';$('settings-model').hidden=section!=='model';
  $('settingsAccountsTab').setAttribute('aria-current',section==='accounts'?'page':'false');
  $('settingsModelTab').setAttribute('aria-current',section==='model'?'page':'false');
  if(section==='model' && !settingsState.model && !settingsState.loading)loadModelSettings();
  if(section==='accounts' && platform.user.role==='admin')loadAccounts().catch(error=>{if(platform.user)message(error.message);});
}
window.SettingsUI={
  reset(){
    Object.assign(settingsState,{model:null,section:'accounts',epoch:settingsState.epoch+1,loading:0,saving:false});
    $('agentSettingsForm').reset();$('agentKey').value='';$('agentClearKey').checked=false;
    $('agentSettingsStatus').textContent='';$('agentKeyState').textContent='Key 只在服务器保存，不会回传到浏览器。';modelSettingsError('');modelSettingsControls();
    $('settingsUsername').textContent='';$('settingsRole').textContent='';$('usersBody').innerHTML='';$('auditList').innerHTML='';$('createUserForm').reset();
    $('settings-model').hidden=true;
  },
  open(){
    if(!platform.user)return;
    $('settingsUsername').textContent=platform.user.username;$('settingsRole').textContent=platform.user.role==='admin'?'管理员':'只读';
    showSettingsSection(settingsState.section);
  }
};
$('settingsAccountsTab').addEventListener('click',()=>showSettingsSection('accounts'));
$('settingsModelTab').addEventListener('click',()=>showSettingsSection('model'));
$('agentReloadSettings').addEventListener('click',loadModelSettings);
$('agentSettingsForm').addEventListener('submit',async e=>{
  e.preventDefault();if(!platform.user||platform.user.role!=='admin'||settingsState.saving||settingsState.loading)return;
  const epoch=settingsState.epoch;settingsState.saving=true;modelSettingsError('');modelSettingsControls();
  try{
    if(!settingsState.model)throw Error('请先载入模型配置');
    const value={protocol:$('agentProtocol').value,endpoint:$('agentEndpoint').value.trim(),model:$('agentModel').value.trim(),api_key:$('agentKey').value,clear_api_key:$('agentClearKey').checked,max_rounds:Number($('agentRounds').value),timeout_seconds:Number($('agentTimeout').value)};
    const result=await api('/api/agent/settings',{method:'PUT',body:JSON.stringify({revision:settingsState.model.revision,value})});
    if(epoch!==settingsState.epoch)return;
    renderModelSettings(result);$('agentSettingsStatus').textContent='模型配置已保存';
  }catch(error){if(epoch===settingsState.epoch)modelSettingsError(error);}
  finally{if(epoch===settingsState.epoch){settingsState.saving=false;modelSettingsControls();}}
});
