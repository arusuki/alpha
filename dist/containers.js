'use strict';
(()=>{
const containerView={busy:false,data:null,action:null,sequence:0,permissions:null,permissionAction:null};
function containerControls(){
  document.querySelectorAll('#page-containers button, #page-containers input, #page-containers select, #containerActionDialog button, #containerActionDialog input, #containerPermissionDialog button, #containerPermissionDialog input').forEach(el=>el.disabled=containerView.busy);
  $('newContainerProxy').disabled=containerView.busy||$('newContainerNetwork').value!=='bridge';
  const p=containerView.permissions;
  $('containerGroupRepair').disabled=containerView.busy||!p||!!p.sudo_error||p.uid===0||p.group_member;
  $('containerDirectoryRepair').disabled=containerView.busy||!p||!!p.sudo_error||p.directory_writable;
}
function renderPermissions(){
  const p=containerView.permissions;if(!p)return;
  $('containerPermissionAccount').textContent=`worker 运行账号：${p.username}（UID ${p.uid}）。检查使用节点已保存的配置；修改目录后请先保存。`;
  $('containerGroupStatus').textContent=p.uid===0?'当前以 root 运行，无需加入 docker 组；建议使用普通服务账号。':p.group_error||(!p.group_member?'账号尚未加入 docker 组。':p.restart_required?'账号已加入 docker 组，当前进程尚未生效；请重启 worker 后重新检查。':'账号已加入 docker 组，当前进程已生效。');
  $('containerDirectoryStatus').textContent=`${p.base_dir}：${p.directory_writable?'可写，已验证创建和删除临时目录。':p.directory_error}`;
  $('containerDockerStatus').textContent=`Docker ${p.endpoint}：${p.docker_available?'连接正常':p.docker_error}。Docker 组成员身份与实际连接结果分别检查。`;
  $('containerSudoStatus').textContent=p.sudo_error||'修复只作用于本节点运行账号及数据根目录；加组后需重启 worker。';
  containerControls();
}
async function refreshPermissions(){
  if(platform.user?.role!=='admin')return;
  const generation=platform.generation;
  containerView.permissions=null;containerControls();
  $('containerGroupStatus').textContent='正在检查…';$('containerDirectoryStatus').textContent='正在检查…';
  let p;
  try{p=await api('/api/containers/permissions');}
  catch(error){if(generation===platform.generation){$('containerGroupStatus').textContent='检查未完成';$('containerDirectoryStatus').textContent='检查未完成';$('containerPermissionError').textContent=error.message;}throw error;}
  if(generation!==platform.generation)return;
  containerView.permissions=p;renderPermissions();
}
function renderContainers(){
  const data=containerView.data;if(!data)return;
  const admin=platform.user?.role==='admin';
  $('containersError').textContent=data.error||'';
  $('managedContainerRows').innerHTML=data.managed.map(c=>{
    const actions=admin?`${!c.error?(!c.initialized?`<button data-container-action="initialize" data-id="${esc(c.id)}">初始化密码并启动</button>`:`<button data-container-action="${c.state==='running'?'stop':'start'}" data-id="${esc(c.id)}">${c.state==='running'?'停止':'启动'}</button> <button data-container-action="restart" data-id="${esc(c.id)}">重启</button>`)+` <button data-container-action="delete" data-id="${esc(c.id)}">删除</button>`:''} <button data-container-action="release" data-id="${esc(c.id)}">解除接管</button>`:'只读';
    return `<tr><td><strong class="managed-container-owner">${esc(c.owner||'未归属')}</strong><span class="sub">${c.origin==='adopt'?'已接管':'平台创建'}</span><small class="process-container-id">名称：${esc(c.name)}</small><small class="process-container-id">ID：${esc(c.id)}</small></td><td>${esc(c.state||'无法确认')}${!c.initialized?'<span class="sub">等待密码初始化</span>':''}${c.error?`<p class="error-text">${esc(c.error)}</p>`:''}</td><td>${esc(c.spec.image)}<span class="sub">${esc(c.spec.network)} · GPU ${esc(c.spec.gpus)} · SSH ${c.spec.port}</span><code>${esc(sshCommand(c.spec.port))}</code></td><td>${actions}</td></tr>`;
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
  const generation=platform.generation;containerView.busy=true;containerControls();$('containersError').textContent='';$('containerActionError').textContent='';$('containerPermissionError').textContent='';$('containerPermissionDialogError').textContent='';
  try{await fn(generation);}catch(error){if(generation===platform.generation){$('containersError').textContent=error.message;if($('containerActionDialog').open)$('containerActionError').textContent=error.message;if($('containerPermissionDialog').open)$('containerPermissionDialogError').textContent=error.message;}}
  finally{if(generation===platform.generation){containerView.busy=false;containerControls();}}
}
function showContainerCredentials(value){
  $('containerCredentials').textContent=`容器：${value.name}\nroot 密码：${value.password}\n${sshCommand(value.port)}${value.warning?"\n\n"+value.warning:""}`;
  $('containerCredentialsDialog').showModal();
}
function resetContainers(){
  containerView.sequence++;Object.assign(containerView,{busy:false,data:null,action:null,permissions:null,permissionAction:null});
  for(const id of ['containerActionDialog','containerCredentialsDialog','containerPermissionDialog'])if($(id).open)$(id).close();
  $('containerSudoPassword').value='';$('containerPermissionDialogError').textContent='';$('containerPermissionError').textContent='';
  $('containerPermissionAccount').textContent='检查运行 worker 的系统账号及已保存的数据目录。';
  $('containerGroupStatus').textContent='等待检查';$('containerDirectoryStatus').textContent='等待检查';$('containerDockerStatus').textContent='';$('containerSudoStatus').textContent='';
  $('containerCredentials').textContent='';$('managedContainerRows').innerHTML='';$('containersStatus').textContent='';$('containersError').textContent='';
  $('createContainerForm').reset();$('containerSettingsForm').reset();containerControls();
}
window.ContainersUI={reset:resetContainers,open(){containerTask(async generation=>{
  await refreshContainers();
  if(generation!==platform.generation)return;
  if(platform.user?.role==='admin'){
    const c=await api('/api/containers/settings');if(generation!==platform.generation)return;
    for(const [id,key] of Object.entries({containerEndpoint:'endpoint',containerDefaultImage:'image',containerBaseDir:'base_dir',containerStartPort:'start_port',containerSSHHost:'ssh_host',containerProxyJump:'proxy_jump'}))$(id).value=c[key];
    await refreshPermissions();
  }
});}};
$('containersRefresh').addEventListener('click',()=>containerTask(async generation=>{await refreshContainers();if(generation===platform.generation)await refreshPermissions();}));
$('containerPermissionsRefresh').addEventListener('click',()=>containerTask(refreshPermissions));
$('containerSettingsForm').addEventListener('submit',event=>{event.preventDefault();containerTask(async generation=>{
  const value={endpoint:$('containerEndpoint').value.trim(),image:$('containerDefaultImage').value.trim(),base_dir:$('containerBaseDir').value.trim(),start_port:Number($('containerStartPort').value),ssh_host:$('containerSSHHost').value.trim(),proxy_jump:$('containerProxyJump').value.trim()};
  await api('/api/containers/settings',{method:'PUT',body:JSON.stringify(value)});if(generation!==platform.generation)return;
  containerView.permissions=null;containerControls();$('containersStatus').textContent='容器配置已保存。';await refreshPermissions();if(generation===platform.generation)await refreshContainers();
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
  $('containersStatus').textContent=`已创建 ${result.name}。${result.warning||""}`;$('createContainerForm').reset();showContainerCredentials(result);await refreshContainers();
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
  $('containersStatus').textContent=`${row.name}：${containerActionNames[action]}操作完成。${result.warning||""}`;await refreshContainers();
});});
$('containerActionClose').addEventListener('click',()=>$('containerActionDialog').close());
$('containerActionDialog').addEventListener('cancel',event=>{if(containerView.busy)event.preventDefault();});
$('containerCredentialsClose').addEventListener('click',()=>{$('containerCredentials').textContent='';$('containerCredentialsDialog').close();});
$('containerCredentialsDialog').addEventListener('close',()=>{$('containerCredentials').textContent='';});

function openPermissionRepair(action){
  const p=containerView.permissions;if(containerView.busy||!p||platform.user?.role!=='admin')return;
  if(p.sudo_error||(action==='docker_group'?(p.uid===0||p.group_member):p.directory_writable))return;
  containerView.permissionAction={action,base_dir:p.base_dir,endpoint:p.endpoint};
  $('containerPermissionTitle').textContent=action==='docker_group'?'加入 Docker 组':'设置数据目录 ACL';
  $('containerPermissionHint').textContent=action==='docker_group'?`使用 ${p.username} 的 sudo 权限将此账号加入 docker 组（不存在时创建）。完成后请重启 worker。`:`使用 ${p.username} 的 sudo 权限为 ${p.base_dir} 添加该账号的读、写和进入目录权限。目录不存在时创建；父目录须已存在。不递归修改已有数据。`;
  $('containerSudoPassword').value='';$('containerPermissionDialogError').textContent='';$('containerPermissionDialog').showModal();$('containerSudoPassword').focus();
}
$('containerGroupRepair').addEventListener('click',()=>openPermissionRepair('docker_group'));
$('containerDirectoryRepair').addEventListener('click',()=>openPermissionRepair('directory'));
$('containerPermissionForm').addEventListener('submit',event=>{event.preventDefault();containerTask(async generation=>{
  if(!containerView.permissionAction)return;
  const request={method:'POST',body:JSON.stringify({...containerView.permissionAction,sudo_password:$('containerSudoPassword').value})};
  $('containerSudoPassword').value='';
  let p;
  try{p=await api('/api/containers/permissions',request);}
  finally{request.body='';}
  if(generation!==platform.generation)return;
  containerView.permissions=p;renderPermissions();$('containerPermissionDialog').close();
  $('containersStatus').textContent=p.restart_required?'权限操作已完成。请重启 worker 使 Docker 组权限生效，再重新检查。':'权限操作已完成，已重新检查当前状态。';
  await refreshContainers();
});});
$('containerPermissionClose').addEventListener('click',()=>{if(!containerView.busy){$('containerSudoPassword').value='';containerView.permissionAction=null;$('containerPermissionDialog').close();}});
$('containerPermissionDialog').addEventListener('cancel',event=>{if(containerView.busy)event.preventDefault();else $('containerSudoPassword').value='';});
$('containerPermissionDialog').addEventListener('close',()=>{if(!$('containerPermissionDialog').open){$('containerSudoPassword').value='';containerView.permissionAction=null;}});

})();
