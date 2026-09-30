'use strict';
const settingsState={model:null,control:null,epoch:0,loading:0,saving:false,controlLoading:0,controlSaving:false};
function controlSettingsControls(){
  $('controlSaveSettings').disabled=!settingsState.control||!!settingsState.controlLoading||settingsState.controlSaving;
  $('controlReloadSettings').disabled=!!settingsState.controlLoading||settingsState.controlSaving;
}
function renderControlSettings(value){
  settingsState.control=value;$('controlInternalIP').value=value.internal_ip;
  $('controlWebScheme').value=value.web_scheme;$('controlWebPort').value=value.web_port;
  $('controlSettingsStatus').textContent='总控配置已载入';
}
async function loadControlSettings(){
  if(settingsState.controlSaving||!platform.user||platform.user.role!=='admin')return;
  const epoch=settingsState.epoch,sequence=++settingsState.controlLoading;
  $('controlSettingsError').textContent='';controlSettingsControls();
  try{const value=await api('/api/control/settings');if(epoch===settingsState.epoch&&sequence===settingsState.controlLoading)renderControlSettings(value);}
  catch(error){if(epoch===settingsState.epoch)$('controlSettingsError').textContent=error.message;}
  finally{if(epoch===settingsState.epoch&&sequence===settingsState.controlLoading){settingsState.controlLoading=0;controlSettingsControls();}}
}
function modelSettingsError(error){$('agentSettingsError').textContent=error ? error.message||String(error) : '';}
function renderModelSettings(result){
  settingsState.model=result;const c=result.value;
  $('agentProtocol').value=c.protocol;$('agentEndpoint').value=c.endpoint;$('agentModel').value=c.model;
  $('agentReasoningSummary').checked=!!c.reasoning_summary;
  $('agentTimeout').value=c.timeout_seconds;
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
window.SettingsUI={
  reset(){
    Object.assign(settingsState,{model:null,control:null,epoch:settingsState.epoch+1,loading:0,saving:false,controlLoading:0,controlSaving:false});
    $('controlSettingsForm').reset();$('controlSettingsError').textContent='';$('controlSettingsStatus').textContent='';controlSettingsControls();
    $('agentSettingsForm').reset();$('agentKey').value='';$('agentClearKey').checked=false;
    $('agentSettingsStatus').textContent='';$('agentKeyState').textContent='Key 只在服务器保存，不会回传到浏览器。';modelSettingsError('');modelSettingsControls();
    $('settingsUsername').textContent='';$('settingsRole').textContent='';$('usersBody').innerHTML='';$('auditList').innerHTML='';$('createUserForm').reset();
  },
  open(){
    if(!platform.user)return;
    $('settingsUsername').textContent=platform.user.username;$('settingsRole').textContent=platform.user.role==='admin'?'管理员':'只读';
    if(platform.user.role==='admin')loadAccounts().catch(error=>{if(platform.user)message(error.message);});
    if(platform.user.role==='admin')loadControlSettings();
  },
  openModel(){
    if(platform.user?.role==='admin' && !settingsState.model && !settingsState.loading)loadModelSettings();
  }
};
$('controlReloadSettings').addEventListener('click',loadControlSettings);
$('controlSettingsForm').addEventListener('submit',async event=>{
  event.preventDefault();if(platform.user?.role!=='admin'||!settingsState.control||settingsState.controlLoading||settingsState.controlSaving)return;
  const epoch=settingsState.epoch;settingsState.controlSaving=true;$('controlSettingsError').textContent='';controlSettingsControls();
  try{
    const value=await api('/api/control/settings',{method:'PUT',body:JSON.stringify({revision:settingsState.control.revision,internal_ip:$('controlInternalIP').value.trim(),web_scheme:$('controlWebScheme').value,web_port:Number($('controlWebPort').value)})});
    if(epoch!==settingsState.epoch)return;
    renderControlSettings(value);$('controlSettingsStatus').textContent='总控配置已保存';
  }catch(error){if(epoch===settingsState.epoch)$('controlSettingsError').textContent=error.message;}
  finally{if(epoch===settingsState.epoch){settingsState.controlSaving=false;controlSettingsControls();}}
});
$('agentReloadSettings').addEventListener('click',loadModelSettings);
$('agentSettingsForm').addEventListener('submit',async e=>{
  e.preventDefault();if(!platform.user||platform.user.role!=='admin'||settingsState.saving||settingsState.loading)return;
  const epoch=settingsState.epoch;settingsState.saving=true;modelSettingsError('');modelSettingsControls();
  try{
    if(!settingsState.model)throw Error('请先载入模型配置');
    const value={protocol:$('agentProtocol').value,endpoint:$('agentEndpoint').value.trim(),model:$('agentModel').value.trim(),api_key:$('agentKey').value,clear_api_key:$('agentClearKey').checked,timeout_seconds:Number($('agentTimeout').value),reasoning_summary:$('agentReasoningSummary').checked};
    const result=await api('/api/agent/settings',{method:'PUT',body:JSON.stringify({revision:settingsState.model.revision,value})});
    if(epoch!==settingsState.epoch)return;
    renderModelSettings(result);$('agentSettingsStatus').textContent='模型配置已保存';
  }catch(error){if(epoch===settingsState.epoch)modelSettingsError(error);}
  finally{if(epoch===settingsState.epoch){settingsState.saving=false;modelSettingsControls();}}
});
