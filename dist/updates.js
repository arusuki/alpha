'use strict';
(()=>{
let epoch=0,tokenEpoch=0,revision=null,tokenRevision=null,busy=false,refreshing=false,lastState='';
const admin=()=>platform.user?.role==='admin';
const target=()=>$('updateTarget').value;
const endpoint=action=>`/api/updates/${target()}/${action}`;
function fields(disabled){$('updateFields').disabled=disabled;$('updateNow').disabled=disabled;$('updateTarget').disabled=busy;$('updateReload').disabled=busy;}
function renderToken(c){tokenRevision=c.revision;$('updateToken').value='';$('updateClearToken').checked=false;$('updateTokenState').textContent=c.has_token?'已保存统一 Token；留空保留':c.has_environment_token?'使用总控服务环境中的 Token':'尚未配置统一 Token';}
async function loadToken(){
 const seq=++tokenEpoch;tokenRevision=null;$('updateTokenFields').disabled=true;$('updateTokenError').textContent='';
 try{const data=await api('/api/updates/github-token');if(seq!==tokenEpoch)return;renderToken(data);$('updateTokenFields').disabled=false;}
 catch(e){if(seq===tokenEpoch)$('updateTokenError').textContent=e.message;}
}
function renderHealth(h){
 const states={idle:'等待更新',downloading:'正在后台下载并校验，服务正常运行',restarting:'正在停止服务并启动更新器',completed:'上次更新完成',failed:'上次更新失败'};
 $('updateHealth').textContent=`${h.mode} · ${h.version} · ${h.healthy?'在线':'不可用'} · ${states[h.update_state]||h.update_state}`;
 $('updateRelease').textContent=h.release?`最近通知：${h.release.tag} · ${h.release.repo} · ${new Date(h.release.published_at).toLocaleString()}`:'尚未收到 release 通知';
 $('updateFailure').textContent=h.update_state==='failed'?(h.error||h.result?.error||'更新失败，请检查日志'):'';
 $('updateDeliveryError').textContent=h.delivery_error?`版本通知失败：${h.delivery_error}`:'';
 $('updateConnectionError').textContent='';
 const recovery=h.update_state==='failed'?h.recovery:null;
 $('updateRecovery').hidden=!recovery;
 $('updateRecoveryNote').textContent=recovery?.note||'';
 $('updateRetryCommand').value=recovery?.command||'';
 $('updateLogPath').textContent=recovery?`完整日志：${recovery.log_path}`:'';
 $('updatePendingPath').hidden=!recovery?.required;
 $('updatePendingPath').textContent=recovery?.required?`需要先完成人工恢复，请检查：${recovery.pending_path}`:'';
 if(h.update_state!==lastState&&['failed','completed'].includes(h.update_state))$('updateStatus').textContent='';
 if(h.update_state!==lastState&&h.update_state==='completed')$('updateError').textContent='';
 lastState=h.update_state;
 if(h.pending_deliveries)$('updateRelease').textContent+=` · ${h.pending_deliveries} 条通知等待下游确认`;
}
function clearHealth(){
 lastState='';$('updateRecovery').hidden=true;$('updateRetryCommand').value='';
 for(const id of ['updateHealth','updateRelease','updateFailure','updateDeliveryError','updateConnectionError','updateRecoveryNote','updateLogPath','updatePendingPath'])$(id).textContent='';
}
async function refreshHealth(){
 if(!admin()||busy||refreshing||platform.page!=='update-settings'||!target())return;
 const seq=epoch;refreshing=true;
 try{const h=await api(endpoint('health'));if(seq===epoch&&platform.page==='update-settings'){renderHealth(h);if(revision===null)await load();}}
 catch(e){if(seq===epoch&&platform.page==='update-settings')$('updateConnectionError').textContent=`暂时无法读取目标节点的更新状态：${e.message}。已有报告保留，恢复通信后自动刷新。`;}
 finally{refreshing=false;}
}
async function load(){
 if(!admin()||busy||!target())return;
 const seq=++epoch;revision=null;fields(true);clearHealth();$('updateError').textContent='';$('updateStatus').textContent='正在读取…';
 try{
  const data=await api(endpoint('settings'));
  if(seq!==epoch)return;
  revision=data.revision;const c=data.config;
  $('updateCommand').value=c.command;$('updateProxy').value=c.http_proxy;$('updateRepo').value=c.repo;
  $('updatePrerelease').checked=c.prerelease;$('updateAutomatic').checked=c.automatic;
  $('updateSecret').value='';$('updateClearSecret').checked=false;$('updateSecretState').textContent=c.has_webhook_secret?'已保存 Secret；留空保留':'尚未启用 webhook';
  const isRegistry=data.health.mode==='registry';$('updateWebhook').hidden=!isRegistry;
  const option=$('updateTarget').selectedOptions[0];$('updateWebhookURL').value=isRegistry?(option.dataset.url||'')+data.webhook_path:'';
  renderHealth(data.health);$('updateStatus').textContent='设置已载入';fields(false);
 }catch(e){if(seq===epoch){$('updateError').textContent=e.message;$('updateStatus').textContent='';fields(true);}}
}
async function open(){
 if(!admin()||busy)return;
 const seq=++epoch;revision=null;fields(true);
 try{
  const data=await api('/api/updates/targets');if(seq!==epoch)return;
  const selected=target();$('updateTarget').innerHTML='<option value="control">control · 本机总控</option>'+data.nodes.map(n=>`<option value="${esc(n.id)}" data-url="${esc(n.url)}">${esc(n.kind)} · ${esc(n.name)}</option>`).join('');
  if([...$('updateTarget').options].some(o=>o.value===selected))$('updateTarget').value=selected;
  await Promise.all([load(),loadToken()]);
 }catch(e){if(seq===epoch)$('updateError').textContent=e.message;}
}
$('updateSettingsForm').addEventListener('submit',async e=>{
 e.preventDefault();if(!admin()||busy||revision===null)return;
 const seq=epoch;busy=true;fields(true);$('updateError').textContent='';
 const config={command:$('updateCommand').value.trim(),http_proxy:$('updateProxy').value.trim(),repo:$('updateRepo').value.trim(),prerelease:$('updatePrerelease').checked,automatic:$('updateAutomatic').checked,webhook_secret:$('updateSecret').value,clear_webhook_secret:$('updateClearSecret').checked};
 try{const data=await api(endpoint('settings'),{method:'PUT',body:JSON.stringify({revision,config})});if(seq!==epoch)return;revision=data.revision;if(target()==='control')tokenRevision=data.revision;$('updateSecret').value='';$('updateClearSecret').checked=false;$('updateSecretState').textContent=data.config.has_webhook_secret?'已保存 Secret；留空保留':'尚未启用 webhook';$('updateStatus').textContent='更新设置已保存';}
 catch(e){if(seq===epoch)$('updateError').textContent=e.message;}
 finally{if(seq===epoch){busy=false;fields(false);}}
});
$('updateTokenForm').addEventListener('submit',async e=>{
 e.preventDefault();if(!admin()||busy||tokenRevision===null)return;
 const seq=epoch;busy=true;fields(true);$('updateTokenFields').disabled=true;$('updateTokenError').textContent='';
 try{
  const data=await api('/api/updates/github-token',{method:'PUT',body:JSON.stringify({revision:tokenRevision,token:$('updateToken').value,clear:$('updateClearToken').checked})});
  if(seq!==epoch)return;renderToken(data.settings);if(target()==='control')revision=data.settings.revision;
  $('updateTokenStatus').textContent=data.pending_nodes.length?`统一 Token 已保存，${data.pending_nodes.length} 个节点待同步；恢复通信或更新前会再次下发。`:'统一 Token 已保存并下发。';
 }catch(e){if(seq===epoch)$('updateTokenError').textContent=e.message;}
 finally{if(seq===epoch){busy=false;fields(revision===null);$('updateTokenFields').disabled=false;}}
});
$('updateNow').addEventListener('click',async()=>{
 if(!admin()||busy||revision===null)return;
 const seq=epoch;busy=true;fields(true);$('updateError').textContent='';
 try{await api(endpoint('update'),{method:'POST',body:'{}'});if(seq!==epoch)return;lastState='downloading';$('updateStatus').textContent='更新已接受，正在后台下载和校验。准备成功后服务会短暂断开并重启；下载失败不影响服务。状态将自动刷新。';}
 catch(e){if(seq===epoch)$('updateError').textContent=e.message;}
 finally{if(seq===epoch){busy=false;fields(false);await refreshHealth();}}
});
$('updateTarget').addEventListener('change',load);$('updateReload').addEventListener('click',async()=>{await load();await loadToken();});
window.UpdatesUI={open,refreshHealth,reset(){epoch++;tokenEpoch++;revision=null;tokenRevision=null;busy=false;clearHealth();$('updateToken').value='';$('updateClearToken').checked=false;$('updateTokenState').textContent='';$('updateTokenStatus').textContent='';$('updateTokenError').textContent='';$('updateTokenFields').disabled=true;$('updateSecret').value='';$('updateProxy').value='';$('updateStatus').textContent='';$('updateError').textContent='';fields(true);}};
})();
