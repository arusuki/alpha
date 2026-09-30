'use strict';
(()=>{
const containerView={busy:false,data:null,action:null,sequence:0};
function containerControls(){
  document.querySelectorAll('#page-containers button, #page-containers input, #page-containers select, #containerActionDialog button, #containerActionDialog input').forEach(el=>el.disabled=containerView.busy);
  $('newContainerProxy').disabled=containerView.busy||$('newContainerNetwork').value!=='bridge';
}
function renderContainers(){
  const data=containerView.data;if(!data)return;
  const admin=platform.user?.role==='admin';
  $('containersError').textContent=data.error||'';
  $('managedContainerRows').innerHTML=data.managed.map(c=>{
    const actions=admin?`${!c.error?(!c.initialized?`<button data-container-action="initialize" data-id="${esc(c.id)}">初始化密码并启动</button>`:`<button data-container-action="${c.state==='running'?'stop':'start'}" data-id="${esc(c.id)}">${c.state==='running'?'停止':'启动'}</button> <button data-container-action="restart" data-id="${esc(c.id)}">重启</button>`)+` <button data-container-action="delete" data-id="${esc(c.id)}">删除</button>`:''} <button data-container-action="release" data-id="${esc(c.id)}">解除接管</button>`:'只读';
    return `<tr><td><strong>${esc(c.name)}</strong><span class="sub">${esc(c.owner||'未归属')} · ${c.origin==='adopt'?'已接管':'平台创建'}</span><small class="process-container-id">${esc(c.id)}</small></td><td>${esc(c.state||'无法确认')}${!c.initialized?'<span class="sub">等待密码初始化</span>':''}${c.error?`<p class="error-text">${esc(c.error)}</p>`:''}</td><td>${esc(c.spec.image)}<span class="sub">${esc(c.spec.network)} · GPU ${esc(c.spec.gpus)} · SSH ${c.spec.port}</span><code>${esc(sshCommand(c.spec.port))}</code></td><td>${actions}</td></tr>`;
  }).join('')||'<tr><td colspan="4" class="empty">尚无管理记录。可以创建容器，或通过命令行导入已有容器。</td></tr>';
  containerControls();
}
function sshCommand(port){const d=containerView.data||{};return `ssh ${d.proxy_jump?'-J '+d.proxy_jump+' ':''}-p ${port} root@${d.ssh_host||'<主机地址>'}`;}
async function refreshContainers(){
  const generation=platform.generation,sequence=++containerView.sequence;
  const data=await api('/api/containers');
  if(generation!==platform.generation||sequence!==containerView.sequence)return;
  containerView.data=data;renderContainers();
}
async function containerTask(fn){
  if(containerView.busy||!platform.user)return;
  const generation=platform.generation;containerView.busy=true;containerControls();$('containersError').textContent='';$('containerActionError').textContent='';
  try{await fn(generation);}catch(error){if(generation===platform.generation){$('containersError').textContent=error.message;if($('containerActionDialog').open)$('containerActionError').textContent=error.message;}}
  finally{if(generation===platform.generation){containerView.busy=false;containerControls();}}
}
function showContainerCredentials(value){
  $('containerCredentials').textContent=`容器：${value.name}\nroot 密码：${value.password}\n${sshCommand(value.port)}`;
  $('containerCredentialsDialog').showModal();
}
function resetContainers(){
  containerView.sequence++;Object.assign(containerView,{busy:false,data:null,action:null});
  for(const id of ['containerActionDialog','containerCredentialsDialog'])if($(id).open)$(id).close();
  $('containerCredentials').textContent='';$('managedContainerRows').innerHTML='';$('containersStatus').textContent='';$('containersError').textContent='';
  $('createContainerForm').reset();$('containerSettingsForm').reset();containerControls();
}
window.ContainersUI={reset:resetContainers,open(){containerTask(async generation=>{
  await refreshContainers();
  if(platform.user?.role==='admin'){
    const c=await api('/api/containers/settings');if(generation!==platform.generation)return;
    for(const [id,key] of Object.entries({containerEndpoint:'endpoint',containerDefaultImage:'image',containerBaseDir:'base_dir',containerStartPort:'start_port',containerSSHHost:'ssh_host',containerProxyJump:'proxy_jump'}))$(id).value=c[key];
  }
});}};
$('containersRefresh').addEventListener('click',()=>containerTask(refreshContainers));
$('containerSettingsForm').addEventListener('submit',event=>{event.preventDefault();containerTask(async generation=>{
  const value={endpoint:$('containerEndpoint').value.trim(),image:$('containerDefaultImage').value.trim(),base_dir:$('containerBaseDir').value.trim(),start_port:Number($('containerStartPort').value),ssh_host:$('containerSSHHost').value.trim(),proxy_jump:$('containerProxyJump').value.trim()};
  await api('/api/containers/settings',{method:'PUT',body:JSON.stringify(value)});if(generation!==platform.generation)return;
  $('containersStatus').textContent='容器配置已保存。';await refreshContainers();
});});
$('newContainerNetwork').addEventListener('change',()=>{if($('newContainerNetwork').value!=='bridge')$('newContainerProxy').checked=false;containerControls();});
$('createContainerForm').addEventListener('submit',event=>{event.preventDefault();containerTask(async generation=>{
  const value={name:$('newContainerName').value.trim(),owner:$('newContainerOwner').value.trim(),image:$('newContainerImage').value.trim(),network:$('newContainerNetwork').value,port:Number($('newContainerPort').value),gpus:$('newContainerGPUs').value,local_proxy:$('newContainerProxy').checked,password:$('newContainerPassword').value};
  $('newContainerPassword').value='';$('containersStatus').textContent='正在创建容器并初始化密码…';
  let result;
  try{result=await api('/api/containers',{method:'POST',body:JSON.stringify(value)});}
  catch(error){
    if(generation===platform.generation){$('containersStatus').textContent='创建未完成，请根据错误提示处理；已登记的容器可重试初始化。';try{await refreshContainers();}catch(_){/* Preserve the original creation failure. */}}
    throw error;
  }
  if(generation!==platform.generation)return;
  $('containersStatus').textContent=`已创建 ${result.name}。`;$('createContainerForm').reset();showContainerCredentials(result);await refreshContainers();
});});
const containerActionNames={start:'启动',stop:'停止',restart:'重启',delete:'删除',release:'解除接管',initialize:'初始化密码并启动'};
$('managedContainerRows').addEventListener('click',event=>{
  const button=event.target.closest('[data-container-action]');if(!button||containerView.busy)return;
  const row=containerView.data?.managed.find(c=>c.id===button.dataset.id);if(!row)return;
  const action=button.dataset.containerAction;containerView.action={row,action};
  $('containerActionTitle').textContent=`${containerActionNames[action]} ${row.name}`;
  $('containerActionHint').textContent=action==='delete'?'仅允许删除已停止的容器。容器可写层（含 /root）将永久删除；宿主机挂载目录与数据卷保留。':action==='release'?'仅解除平台管理记录，容器继续保持当前状态。之后可以通过命令行重新导入。':action==='initialize'?'为尚未完成初始化的容器生成新的随机 root 密码，并启动 SSH。':action==='stop'||action==='restart'?'此操作会中断容器内正在执行的任务和 SSH 连接。':'启动此容器。';
  $('containerConfirmLabel').hidden=!['delete','release'].includes(action);$('containerConfirmName').required=!$('containerConfirmLabel').hidden;$('containerConfirmName').value='';$('containerActionError').textContent='';$('containerActionDialog').showModal();
});
$('containerActionForm').addEventListener('submit',event=>{event.preventDefault();containerTask(async generation=>{
  const {row,action}=containerView.action;
  const result=await api(`/api/containers/${row.id}/${action}`,{method:'POST',body:JSON.stringify({confirm:$('containerConfirmName').value})});
  if(generation!==platform.generation)return;
  $('containerActionDialog').close();if(result.password)showContainerCredentials(result);
  $('containersStatus').textContent=`${row.name}：${containerActionNames[action]}操作完成。`;await refreshContainers();
});});
$('containerActionClose').addEventListener('click',()=>$('containerActionDialog').close());
$('containerActionDialog').addEventListener('cancel',event=>{if(containerView.busy)event.preventDefault();});
$('containerCredentialsClose').addEventListener('click',()=>{$('containerCredentials').textContent='';$('containerCredentialsDialog').close();});
$('containerCredentialsDialog').addEventListener('close',()=>{$('containerCredentials').textContent='';});

})();
