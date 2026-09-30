'use strict';
(()=>{
const state={epoch:0,busy:false,settings:null,data:null,member:null,install:null,accountAction:null};
const labels={pending:'待分配',running:'创建中',invited:'等待接受',accepted:'已接受',ready:'已就绪',failed:'失败',creating:'邀请创建中',unknown:'邀请待核对',deleted:'已回收',deleting:'回收中',unallocated:'未申请'};
const label=s=>labels[s]||s;
function controls(){
  $('bastionSettingsFields').disabled=state.busy||!state.settings;
  $('bastionInstallSubmit').disabled=state.busy;
  $('bastionSudoPassword').disabled=!!state.install;
  for(const root of [$('page-bastion'),$('bastionMemberDialog')])root.querySelectorAll('button').forEach(e=>{if(e.id!=='bastionMemberClose')e.disabled=state.busy||e.dataset.locked==='true'||(e.dataset.accountAction&&!state.data?.jump_installation.web.available);});
}
function row(title,detail,actions=''){return `<article class="bastion-row"><div><strong>${esc(title)}</strong><div class="bastion-detail">${detail}</div></div><div class="actions">${actions}</div></article>`;}
function poolButtons(type,r){return `<button data-pool="${type}" data-id="${esc(r.id)}" data-enabled="${r.enabled?'false':'true'}">${r.enabled?'停用':'启用'}</button><button data-remove-pool="${type}" data-id="${esc(r.id)}" ${r.member_count?'disabled data-locked="true"':''}>移除</button>`;}
function renderInstallation(){
 const install=state.data?.jump_installation;if(!install)return;
 const account=install.account;
 $('bastionInstall').textContent=account.managed?'重新安装跳板':'添加账号';
 $('bastionInstall').hidden=account.exists&&!account.managed&&!account.removed;
 $('bastionAdopt').hidden=!account.exists||account.managed||account.removed;
 $('bastionRelease').hidden=!account.managed;
 $('bastionRemoveAccount').hidden=!account.managed&&!account.released&&!account.removed;
 $('bastionInstallAvailability').textContent=install.web.available?'':install.web.error;
 $('bastionJumpInstallation').textContent=install.ready?'已安装，可管理 alpha-jump 公钥。':install.error;
}
function render(){
 const d=state.data;if(!d)return;
 renderInstallation();
 const pool=d.key_pool;
 $('bastionKeyPoolError').textContent=pool.error;
 $('bastionKeySync').dataset.locked=String(!d.jump_installation.ready);
 $('bastionKeyPool').innerHTML=pool.keys.map(k=>row(k.fingerprint,`<strong>${k.state==='free'?'free · 未关联用户':'已关联'}</strong>${k.members.length?' · '+k.members.map(m=>esc(m.username)).join('、'):''}<details><summary>查看公钥</summary><code class="pool-public-key">${esc(k.public_key)}</code><small class="sub">条目 ${esc(k.id)}</small></details>`,k.state==='free'?`<button data-clean-key="${esc(k.id)}" ${!d.jump_installation.ready?'disabled data-locked="true"':''}>清理 free 公钥</button>`:'')).join('')||'<p class="empty">公钥池暂无条目。</p>';
 $('bastionTailscalePool').innerHTML=d.tailscale.map(r=>row(r.name,`${esc(r.id)} · ${r.enabled?'可分配':'已停用'} · ${r.member_count} 名使用者`,poolButtons('tailscale',r))).join('')||'<p class="empty">尚未选择分享节点。</p>';
 $('bastionAssignments').innerHTML=d.assignments.map(r=>row(r.username,`${r.member_status==='deleting'?'正在回收 · ':''}Tailscale：${esc(label(r.invite_state))} · alpha-jump 公钥：${esc(label(r.key_state))}${r.error?`<br><span class="error-text">${esc(r.error)}</span>`:''}`,`<button data-member="${esc(r.member_id)}">查看资源</button>`)).join('')||'<p class="empty">暂无使用者资源。</p>';
}
async function task(fn){if(state.busy||platform.user?.role!=='admin')return;const epoch=state.epoch;state.busy=true;controls();$('bastionError').textContent='';$('bastionMemberError').textContent='';try{await fn(epoch);}catch(e){if(epoch===state.epoch)$(state.member?'bastionMemberError':'bastionError').textContent=e.message;}finally{if(epoch===state.epoch){state.busy=false;controls();}}}
async function load(epoch,settings=false){const [data,cfg]=await Promise.all([api('/api/bastion/resources'),settings?api('/api/tailscale/settings'):null]);if(epoch!==state.epoch)return;state.data=data;if(cfg){state.settings=cfg;$('bastionTailnet').value=cfg.tailnet;$('bastionTokenStatus').textContent=cfg.has_api_token?'已保存 API Key · 版本 '+cfg.revision:'尚未配置 API Key';}render();}
async function member(epoch,id){const v=await api(`/api/members/${id}/resources`);if(epoch!==state.epoch)return;state.member=v;const a=v.access;$('bastionMemberTitle').textContent=a.username+' · '+(v.status==='deleting'?'正在回收':'资源');
 $('bastionMemberContent').innerHTML=`<p>Tailscale：${esc(a.tailscale_id||'待分配')} · ${esc(label(a.invite_state))}${a.accepted_by?' · '+esc(a.accepted_by):''}</p>${a.invite_url?`<label>分享邀请链接<input readonly value="${esc(a.invite_url)}"></label>`:''}<p>alpha-jump 公钥：${esc(label(a.key_state))}</p>${a.error?`<p class="error-text">${esc(a.error)}</p>`:''}<div class="bastion-list">${v.nodes.map(n=>row(n.node_name,`${esc(label(n.state))}${n.name?' · '+esc(n.name):''}${n.port?' · '+esc(n.ssh_host)+':'+n.port:''}${n.error?'<br><span class="error-text">'+esc(n.error)+'</span>':''}`)).join('')||'<p>尚未添加 node。</p>'}</div>`;
 $('bastionResolveForm').hidden=!['unknown','creating'].includes(a.invite_state);$('bastionMemberRetry').hidden=v.status!=='active';
 if(!$('bastionMemberDialog').open)$('bastionMemberDialog').showModal();
}
function closeInstall(){ $('bastionSudoPassword').value=''; if(state.install){state.install.abort();state.install=null;} $('bastionInstallDialog').close(); }
window.BastionUI={open(){return task(e=>load(e,true));},leave(){$('bastionToken').value='';closeInstall();},reset(){closeInstall();state.epoch++;state.busy=false;state.settings=null;state.data=null;state.member=null;$('bastionToken').value='';$('bastionMemberDialog').close();for(const id of ['bastionDevices','bastionTailscalePool','bastionAssignments','bastionMemberContent','bastionInviteCandidates','bastionKeyPool'])$(id).innerHTML='';$('bastionError').textContent='';$('bastionStatus').textContent='';$('bastionSettingsForm').reset();controls();}};
const accountActions={
 init:{title:'添加或重装 alpha-jump',button:'安装',description:'添加固定账号或更新公钥读取器，校验并重载 SSH 配置。已有工具会更新，兼容的 data 和成员公钥保留。',success:'跳板安装完成；后续公钥管理无需 sudo。'},
 adopt:{title:'接管已有 alpha-jump',button:'接管',description:'保留已有账号的 UID/GID 和 home，安装或更新公钥读取器，将已有公钥池 data 转交当前 control 和服务用户；保留所有公钥，补齐当前用户缺少的公钥，未关联条目标为 free。SSH 将只接受平台公钥清单；home 中原有 authorized_keys 保留但不再用于登录。原 control 数据目录保留。',success:'已接管 alpha-jump、工具和 data。'},
 release:{title:'取消接管 alpha-jump',button:'取消接管',description:'停止平台添加和撤销公钥。账号、工具、data、SSH 配置和现有公钥授权全部保留；此操作不会撤销已有访问权限。需要管理公钥时可重新接管。',success:'已取消接管；账号、工具、data 和现有授权保留。'},
 delete:{title:'删除 alpha-jump 账号',button:'删除账号',description:'删除固定系统账号，要求先撤销全部跳板公钥。工具、data、home 和 SSH 配置保留。不会强制结束已有进程；删除失败时可核对后重试。',success:'已删除 alpha-jump 账号；工具和 data 保留。'}
};
for(const button of document.querySelectorAll('[data-account-action]'))button.addEventListener('click',()=>{
 if(state.busy||platform.user?.role!=='admin'||!state.data?.jump_installation.web.available)return;
 state.accountAction=button.dataset.accountAction;const action=accountActions[state.accountAction];
 $('bastionInstallTitle').textContent=action.title;$('bastionInstallDescription').textContent=action.description;$('bastionInstallSubmit').textContent=action.button;
 $('bastionAccountConfirmLabel').hidden=state.accountAction!=='delete';$('bastionAccountConfirm').required=state.accountAction==='delete';$('bastionAccountConfirm').value='';
 $('bastionSudoPassword').value='';$('bastionInstallError').textContent='';$('bastionInstallProgress').textContent='';
 $('bastionInstallUser').textContent='服务用户：'+state.data.jump_installation.web.service_user+'；data：'+state.data.jump_installation.data_directory;
 $('bastionInstallDialog').showModal();$('bastionSudoPassword').focus();
});
$('bastionInstallClose').addEventListener('click',closeInstall);
$('bastionInstallDialog').addEventListener('close',()=>{ $('bastionSudoPassword').value='';if(state.install){state.install.abort();state.install=null;} });
$('bastionInstallDialog').addEventListener('cancel',()=>{ $('bastionSudoPassword').value=''; });
$('bastionInstallForm').addEventListener('submit',event=>{
 event.preventDefault();if(state.busy||platform.user?.role!=='admin')return;
 let password=$('bastionSudoPassword').value;$('bastionSudoPassword').value='';
 if(!password||/[\r\n\0]/.test(password)){password='';$('bastionInstallError').textContent='请输入服务用户的 sudo 密码。';return;}
 const action=state.accountAction;
 if(!accountActions[action])return;
 const confirm=$('bastionAccountConfirm').value;
 if(action==='delete'&&confirm!=='alpha-jump'){password='';$('bastionInstallError').textContent='请输入 alpha-jump 确认删除账号。';return;}
 let body=JSON.stringify({action,sudo_password:password,...(action==='delete'?{confirm}:{})});password='';
 task(async epoch=>{
  const controller=new AbortController();state.install=controller;controls();
  $('bastionInstallError').textContent='';$('bastionInstallProgress').textContent='正在认证并执行'+accountActions[action].button+'…';
  try{
   const pending=api('/api/bastion/install',{method:'POST',body,signal:controller.signal});body='';await pending;
   if(epoch!==state.epoch||state.install!==controller)return;
   state.install=null;$('bastionInstallDialog').close();$('bastionStatus').textContent=accountActions[action].success;await load(epoch);
  }catch(e){
   if(epoch===state.epoch&&state.install===controller&&e.name!=='AbortError'){$('bastionInstallError').textContent=e.message;$('bastionInstallProgress').textContent='操作未完成，请刷新状态核对后重新输入密码。';}
  }finally{
   body='';if(epoch===state.epoch){$('bastionSudoPassword').value='';if(state.install===controller)state.install=null;}
  }
 });
});
$('bastionKeySync').addEventListener('click',()=>task(async e=>{await api('/api/bastion/keys/sync',{method:'POST',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已补齐用户公钥，free 条目保留。';await load(e);}));
$('bastionKeyPool').addEventListener('click',event=>{const button=event.target.closest('[data-clean-key]');if(!button)return;task(async e=>{await api('/api/bastion/keys/'+button.dataset.cleanKey,{method:'DELETE',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已清理 free 公钥。';await load(e);});});
$('bastionRefresh').addEventListener('click',()=>task(e=>load(e,true)));
$('bastionSettingsForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{const value={revision:state.settings.revision,tailnet:$('bastionTailnet').value,api_token:$('bastionToken').value};$('bastionToken').value='';await api('/api/tailscale/settings',{method:'PUT',body:JSON.stringify(value)});if(e===state.epoch){$('bastionDevices').innerHTML='';$('bastionStatus').textContent='凭据已保存。';}await load(e,true);});});
$('bastionTest').addEventListener('click',()=>task(async e=>{const r=await api('/api/tailscale/test',{method:'POST',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent=`连接成功，可读取 ${r.device_count} 个节点。`;}));
$('bastionDevicesRefresh').addEventListener('click',()=>task(async e=>{const r=await api('/api/tailscale/devices');if(e!==state.epoch)return;$('bastionDevices').innerHTML=r.devices.map(d=>row(d.hostname||d.name,`${esc(d.nodeId)} · ${esc(d.addresses.join(' / '))}${d.isExternal?' · 外部分享节点':''}`,!d.isExternal&&d.authorized?`<button data-pool="tailscale" data-id="${esc(d.nodeId)}" data-enabled="true">允许分享</button>`:'')).join('')||'<p class="empty">暂无可用节点。</p>';}));
$('page-bastion').addEventListener('click',event=>{const b=event.target.closest('button');if(!b)return;if(b.dataset.member){$('bastionDeleteForm').reset();$('bastionResolveForm').reset();$('bastionInviteCandidates').innerHTML='';return task(e=>member(e,b.dataset.member));}if(b.dataset.pool||b.dataset.removePool)task(async e=>{const type=b.dataset.pool||b.dataset.removePool;await api(`/api/bastion/${type}/${encodeURIComponent(b.dataset.id)}`,{method:b.dataset.pool?'PUT':'DELETE',body:JSON.stringify(b.dataset.pool?{enabled:b.dataset.enabled==='true'}:{})});await load(e);});});
$('bastionMemberClose').addEventListener('click',()=>$('bastionMemberDialog').close());
$('bastionMemberDialog').addEventListener('close',()=>{state.member=null;$('bastionMemberContent').innerHTML='';$('bastionInviteCandidates').innerHTML='';});
$('bastionShareRefresh').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/refresh`,{method:'POST',body:'{}'});await member(e,id);}));
$('bastionMemberRefresh').addEventListener('click',()=>task(e=>member(e,state.member.member_id)));
$('bastionMemberRetry').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/members/${id}/retry`,{method:'POST',body:'{}'});await member(e,id);}));
$('bastionDeleteForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{if($('bastionDeleteConfirm').value!==state.member.access.username)throw Error('请填写完整使用者标识确认删除。');await api(`/api/members/${state.member.member_id}`,{method:'DELETE',body:'{}'});if(e!==state.epoch)return;$('bastionMemberDialog').close();$('bastionStatus').textContent='已开始回收，请刷新资源查看结果。';await load(e);});});
$('bastionResolveForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/resolve-invite`,{method:'POST',body:JSON.stringify({invite_id:$('bastionResolveID').value})});await member(e,id);});});
$('bastionConfirmAbsent').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/resolve-invite`,{method:'POST',body:JSON.stringify({confirm_absent:true})});await member(e,id);}));
$('bastionListInvites').addEventListener('click',()=>task(async e=>{const r=await api(`/api/bastion/tailscale/${encodeURIComponent(state.member.access.tailscale_id)}/invites`);if(e!==state.epoch)return;$('bastionInviteCandidates').innerHTML=r.invites.map(v=>row(v.id,`${esc(v.created)} · ${v.accepted?'已接受':'等待接受'}${v.acceptedBy?.loginName?' · '+esc(v.acceptedBy.loginName):''}`)).join('')||'<p>暂无邀请。</p>';}));
})();
