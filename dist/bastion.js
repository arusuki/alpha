'use strict';
(()=>{
const state={epoch:0,busy:false,settings:null,ssh:null,identity:null,selectedIdentity:'',data:null,member:null,node:null,devices:[]};
const labels={pending:'待分配',running:'创建中',invited:'等待接受',accepted:'已接受',ready:'已就绪',failed:'失败',creating:'邀请创建中',unknown:'邀请待核对',deleted:'已移除',deleting:'删除中',unallocated:'未申请'};
const label=s=>labels[s]||s;
function controls(){
  $('bastionSettingsFields').disabled=state.busy||!state.settings;
  $('bastionSSHFields').disabled=state.busy||!state.ssh;
  $('bastionSSHGenerateFields').disabled=state.busy||!state.ssh;
  $('bastionSSHSave').dataset.locked=String(!state.identity||state.identity.identity_file!==state.selectedIdentity||!state.ssh?.identities.includes(state.selectedIdentity)||state.selectedIdentity===state.ssh?.identity_file);
  $('bastionSSHDownload').dataset.locked=String(!state.identity?.public_key);
  $('bastionNodeFields').disabled=state.busy;
  for(const root of [$('page-bastion'),$('bastionMemberDialog'),$('bastionNodeDialog')])root.querySelectorAll('button').forEach(e=>{if(e.id!=='bastionMemberClose')e.disabled=state.busy||e.dataset.locked==='true';});
}
function renderIdentity(identity){
 state.identity=identity?.public_key?identity:null;
 $('bastionSSHPublic').hidden=!state.identity;
 $('bastionSSHPublicKey').textContent=state.identity?.public_key||'';
 $('bastionSSHFingerprint').textContent=state.identity?.fingerprint||'';
}
function identityName(path){return path.split('/').at(-2);}
function renderIdentities(){
 const cfg=state.ssh;
 $('bastionSSHInventory').innerHTML=cfg.identities.map(path=>{
  const active=path===cfg.identity_file,selected=path===state.selectedIdentity;
  return `<article class="bastion-key-card" data-selected="${selected}"><button type="button" class="bastion-key-choice" data-select-identity="${esc(path)}" aria-pressed="${selected}"><span class="bastion-key-indicator" aria-hidden="true">${selected?'✓':''}</span><span class="bastion-key-label"><strong>${esc(identityName(path))}</strong><small>Ed25519 · ${selected?'已选择':'点击选择并查看公钥'}</small></span><span class="resource-tag" ${active?'data-tone="success"':''}>${active?'当前使用':selected?'待启用':'未启用'}</span></button><button type="button" class="bastion-key-delete" data-delete-identity="${esc(path)}" aria-label="删除密钥 ${esc(identityName(path))}" ${active?'disabled data-locked="true" title="请先启用其他密钥"':''}>删除</button></article>`;
 }).join('')||'<div class="empty"><strong>还没有总控密钥</strong><p>在下方创建第一个密钥，再下载公钥到 share node 授权。</p></div>';
}
function renderSSH(cfg){
 state.ssh=cfg;state.selectedIdentity=cfg.identities.includes(cfg.identity_file)?cfg.identity_file:'';
 renderIdentities();
 $('bastionSSHCurrent').textContent=cfg.identity_file?`当前使用：${identityName(cfg.identity_file)} · 版本 ${cfg.revision}`:'尚未启用密钥。选择或创建密钥后，先在 share node 授权公钥，再启用。';
 $('bastionSSHDirectory').textContent='创建 Ed25519 密钥，私钥保存在总控，刷新后仍可选择。';
 $('bastionSSHCreate').open=!cfg.identities.length;
 renderIdentity(state.selectedIdentity?cfg:null);$('bastionSSHStatus').textContent=cfg.error||'';
}
function row(title,detail,actions=''){return `<article class="bastion-row"><div><strong>${esc(title)}</strong><div class="bastion-detail">${detail}</div></div><div class="actions">${actions}</div></article>`;}
function statusTag(value){const tone=['accepted','ready'].includes(value)?'success':['failed','unknown','deleting'].includes(value)?'warning':'neutral';return `<span class="resource-tag" data-tone="${tone}">${esc(label(value))}</span>`;}
function poolButtons(r){return `<button data-edit-node="${esc(r.id)}">配置</button><button data-pool="tailscale" data-id="${esc(r.id)}" data-enabled="${r.enabled?'false':'true'}">${r.enabled?'停用':'启用'}</button><button data-remove-pool="tailscale" data-id="${esc(r.id)}" ${r.member_count?'disabled data-locked="true"':''}>移除</button>`;}
function assignmentNodeLabel(assignment){
 if(!assignment.tailscale_id)return '尚未分配 share node';
 const node=state.data.tailscale.find(n=>n.id===assignment.tailscale_id);
 return node?`Share node · ${node.name} (${node.id})`:`Share node 不可用 · ${assignment.tailscale_id}`;
}
function assignmentsHTML(assignments,showNode=false){return assignments.map(r=>`<article class="bastion-assignment"><div class="bastion-member-name"><strong>${esc(r.username)}</strong>${showNode?`<small class="sub">${esc(assignmentNodeLabel(r))}</small>`:''}${r.member_status==='deleting'?'<small class="sub">正在删除</small>':''}</div><div class="bastion-member-state"><span>Tailscale 邀请</span>${statusTag(r.invite_state)}</div><div class="bastion-member-state"><span>alpha-jump 公钥</span>${statusTag(r.key_state)}</div><button data-member="${esc(r.member_id)}">查看资源 <span aria-hidden="true">↗</span></button>${r.error?`<p class="error-text">${esc(r.error)}</p>`:''}</article>`).join('');}
function keysHTML(keys){return keys.map(k=>row(k.fingerprint,`<strong>${k.state==='free'?'free · 未关联用户':'已关联'}</strong>${k.members.length?' · '+k.members.map(m=>esc(m.username)).join('、'):''}<details><summary>查看公钥</summary><code class="pool-public-key">${esc(k.public_key)}</code><small class="sub">条目 ${esc(k.id)}</small></details>`,k.state==='free'?`<button data-clean-key="${esc(k.id)}" data-key-node="${esc(k.node_id)}">清理 free 公钥</button>`:'')).join('');}
function render(){
 const d=state.data;if(!d)return;
 $('bastionShareSummary').textContent=d.tailscale.length?`已配置 ${d.tailscale.length} 个 share node · ${d.tailscale.filter(n=>n.enabled).length} 个可分配`:'先在 share node 初始化两个账号，再查询节点并加入分享池。';
 const pool=d.key_pool;
 $('bastionKeyPoolError').textContent=pool.error;
 $('bastionKeySync').dataset.locked=String(!d.tailscale.length);
 $('bastionTailscalePool').innerHTML=d.tailscale.map(n=>{
  const keys=pool.keys.filter(k=>k.node_id===n.id),members=d.assignments.filter(m=>m.tailscale_id===n.id);
  return `<article class="management-panel bastion-node" data-share-node="${esc(n.id)}">
   <div class="section-heading"><div class="bastion-node-title"><h2>${esc(n.name)}</h2><span class="resource-tag" ${n.enabled?'data-tone="success"':''}>${n.enabled?'可分配':'已停用'}</span><span class="sub">${n.member_count} 名使用者</span></div><div class="actions">${poolButtons(n)}</div></div>
   <div class="bastion-node-connection"><span class="mono">${esc(n.ssh_host)} · SSH ${n.ssh_port} · 总控入口 ${n.status_port}</span><span>总控地址：${esc(n.control_url)}</span><small class="sub mono">节点 ID · ${esc(n.id)}</small></div>
   <div class="bastion-node-accounts"><div><strong class="mono">alpha-worker</strong><span>总控管理账号</span><p>总控通过 SSH 登录此账号管理公钥；此账号运行网页入口代理。</p></div><div><strong class="mono">alpha-jump</strong><span>成员跳板账号</span><p>使用本节点公钥池授权登录，转发到计算节点。</p></div></div>
   <section class="bastion-node-section"><h3>本节点公钥池 <span class="sub">${keys.length} 条 · ${keys.filter(k=>k.state==='free').length} 条 free</span></h3><p class="resource-description">公钥可关联多名使用者。free 公钥仍可登录，清理后撤销本节点授权。</p><div class="bastion-list bastion-node-keys">${keysHTML(keys)||`<p class="empty">${pool.error?'公钥池查询未完整成功，请查看上方错误后刷新。':'本节点暂无公钥。'}</p>`}</div></section>
   <section class="bastion-node-section"><h3>本节点使用者 <span class="sub">${members.length} 名</span></h3><div class="bastion-list">${assignmentsHTML(members)||'<p class="empty">本节点尚未分配使用者。</p>'}</div></section>
  </article>`;
 }).join('')||'<p class="empty">尚未配置分享节点。展开全局连接设置保存凭据，再查询并添加节点。</p>';
 $('bastionAssignments').innerHTML=assignmentsHTML(d.assignments,true)||'<p class="empty">暂无使用者资源。完成注册后，可在这里查看授权和分配情况。</p>';
}

async function task(fn){if(state.busy||platform.user?.role!=='admin')return;const epoch=state.epoch;state.busy=true;controls();$('bastionError').textContent='';$('bastionMemberError').textContent='';$('bastionNodeError').textContent='';try{await fn(epoch);}catch(e){if(epoch===state.epoch)$($('bastionNodeDialog').open?'bastionNodeError':$('bastionMemberDialog').open?'bastionMemberError':'bastionError').textContent=e.message;}finally{if(epoch===state.epoch){state.busy=false;controls();}}}
async function load(epoch,settings=false){const [data,cfg,ssh]=await Promise.all([api('/api/bastion/resources'),settings?api('/api/tailscale/settings'):null,settings?api('/api/bastion/ssh'):null]);if(epoch!==state.epoch)return;state.data=data;if(cfg){state.settings=cfg;$('bastionTailnet').value=cfg.tailnet;$('bastionTokenStatus').textContent=cfg.has_api_token?'已保存 API Key · 版本 '+cfg.revision:'尚未配置 API Key';}if(ssh)renderSSH(ssh);render();}
async function member(epoch,id){const v=await api(`/api/members/${id}/resources`);if(epoch!==state.epoch)return;state.member=v;const a=v.access;$('bastionMemberTitle').textContent=a.username+' · '+(v.status==='deleting'?'正在删除':'资源');
 $('bastionMemberContent').innerHTML=`<p>Tailscale：${esc(a.tailscale_id||'待分配')} · ${esc(label(a.invite_state))}${a.accepted_by?' · '+esc(a.accepted_by):''}</p>${a.invite_url?`<label>分享邀请链接<input readonly value="${esc(a.invite_url)}"></label>`:''}<p>alpha-jump 公钥：${esc(label(a.key_state))}</p>${a.error?`<p class="error-text">${esc(a.error)}</p>`:''}<div class="bastion-list">${v.nodes.map(n=>row(n.node_name,`${esc(label(n.state))}${n.name?' · '+esc(n.name):''}${n.port?' · '+esc(n.internal_ip)+' · 端口 '+n.port:''}${n.error?'<br><span class="error-text">'+esc(n.error)+'</span>':''}`)).join('')||'<p>尚未添加 node。</p>'}</div>`;
 $('bastionResolveForm').hidden=!['unknown','creating'].includes(a.invite_state);$('bastionMemberRetry').hidden=v.status!=='active';
 if(!$('bastionMemberDialog').open)$('bastionMemberDialog').showModal();
}
function closeNode(){state.node=null;$('bastionNodeDialog').close();}
window.BastionUI={open(){return task(e=>load(e,true));},leave(){$('bastionToken').value='';closeNode();},reset(){closeNode();state.epoch++;state.busy=false;state.settings=null;state.ssh=null;state.selectedIdentity='';state.data=null;state.member=null;state.devices=[];renderIdentity(null);$('bastionSSHForm').reset();$('bastionSSHGenerateForm').reset();for(const id of ['bastionSSHCurrent','bastionSSHDirectory','bastionSSHStatus'])$(id).textContent='';$('bastionSSHCreate').open=false;$('bastionSSHInventory').innerHTML='';$('bastionToken').value='';$('bastionMemberDialog').close();for(const id of ['bastionDevices','bastionTailscalePool','bastionAssignments','bastionMemberContent','bastionInviteCandidates'])$(id).innerHTML='';$('bastionError').textContent='';$('bastionStatus').textContent='';$('bastionKeyPoolError').textContent='';$('bastionShareSummary').textContent='';$('bastionGlobalSettings').open=false;$('bastionSettingsForm').reset();controls();}};
$('bastionSSHInventory').addEventListener('click',event=>{
 const remove=event.target.closest('[data-delete-identity]');
 if(remove){
  if(state.busy||platform.user?.role!=='admin'||!state.ssh)return;
  const path=remove.dataset.deleteIdentity;
  if(path===state.ssh.identity_file)return;
  if(!window.confirm(`删除此密钥的私钥和公钥文件？删除后无法恢复。\n${path}\n此操作不会撤销 share node 上已安装的管理公钥。`))return;
  return task(async epoch=>{
   await api('/api/bastion/ssh/identity',{method:'DELETE',body:JSON.stringify({identity_file:path})});
   if(epoch!==state.epoch)return;
   state.ssh.identities=state.ssh.identities.filter(p=>p!==path);
   if(state.selectedIdentity===path){state.selectedIdentity=state.ssh.identities.includes(state.ssh.identity_file)?state.ssh.identity_file:'';renderIdentity(state.selectedIdentity?state.ssh:null);}
   else if(state.identity?.identity_file===path)renderIdentity(null);
   renderIdentities();$('bastionSSHCreate').open=!state.ssh.identities.length;
   $('bastionSSHStatus').textContent='密钥文件已删除。';
  });
 }
 const button=event.target.closest('[data-select-identity]');if(!button)return;
 task(async epoch=>{
  const path=button.dataset.selectIdentity;
  if(!state.ssh?.identities.includes(path))throw Error('请从列表选择已创建的密钥');
  state.selectedIdentity=path;renderIdentity(null);renderIdentities();controls();
  $('bastionSSHStatus').textContent='正在读取所选密钥的公钥…';
  const identity=await api('/api/bastion/ssh/public-key?identity_file='+encodeURIComponent(path));
  if(epoch!==state.epoch)return;renderIdentity(identity);
  $('bastionSSHStatus').textContent=path===state.ssh.identity_file?'已读取当前使用身份的公钥。':'已选择保存的密钥，尚未启用。请先在 share node 授权，再校验并启用。';
 });
});
$('bastionSSHForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
 if(!state.ssh?.identities.includes(state.selectedIdentity)||state.identity?.identity_file!==state.selectedIdentity)throw Error('请先选择已创建的密钥；没有密钥时请先创建');
 const cfg=await api('/api/bastion/ssh',{method:'PUT',body:JSON.stringify({revision:state.ssh.revision,identity_file:state.selectedIdentity})});
 if(epoch!==state.epoch)return;renderSSH(cfg);$('bastionSSHStatus').textContent='总控密钥已启用。';await load(epoch);
});});
$('bastionSSHGenerateForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
 const identity=await api('/api/bastion/ssh/generate',{method:'POST',body:JSON.stringify({name:$('bastionSSHName').value.trim()})});
 if(epoch!==state.epoch)return;
 state.selectedIdentity=identity.identity_file;renderIdentity(identity);$('bastionSSHCreate').open=false;
 $('bastionSSHStatus').textContent='密钥对已持久保存，尚未启用。刷新后仍可从密钥列表选择。请先下载公钥并在 share node 授权，再校验并启用。';
 state.ssh.identities=[...new Set([...state.ssh.identities,identity.identity_file])].sort();renderIdentities();
});});
$('bastionSSHDownload').addEventListener('click',()=>{
 if(state.busy||platform.user?.role!=='admin'||!state.identity)return;
 const url=URL.createObjectURL(new Blob([state.identity.public_key+'\n'],{type:'text/plain;charset=utf-8'}));
 const link=document.createElement('a');link.href=url;link.download='control-service.pub';document.body.appendChild(link);link.click();link.remove();setTimeout(()=>URL.revokeObjectURL(url),1000);
});
function openNode(id){
 if(state.busy||platform.user?.role!=='admin')return;
 const device=state.devices.find(d=>d.nodeId===id),saved=state.data.tailscale.find(n=>n.id===id);
 if(!device&&!saved)return;
 state.node={id,enabled:saved?!!saved.enabled:true};$('bastionNodeError').textContent='';$('bastionNodeTitle').textContent=(saved?'配置':'加入')+' share node · '+(saved?.name||device?.hostname||device?.name);
 const addresses=device?.addresses||[saved.ssh_host];
 $('bastionNodeHost').innerHTML=addresses.map(ip=>`<option value="${esc(ip)}">${esc(ip)}</option>`).join('');
 $('bastionNodeHost').value=saved?.ssh_host||addresses[0];$('bastionNodeSSHPort').value=saved?.ssh_port||22;
 $('bastionNodeDialog').showModal();controls();
}
$('bastionNodeClose').addEventListener('click',closeNode);
$('bastionNodeDialog').addEventListener('close',()=>{state.node=null;});
$('bastionNodeDialog').addEventListener('cancel',event=>{if(state.busy)event.preventDefault();});
$('bastionNodeForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
 const id=state.node.id,value={enabled:state.node.enabled,ssh_host:$('bastionNodeHost').value,ssh_port:Number($('bastionNodeSSHPort').value)};
 await api('/api/bastion/tailscale/'+encodeURIComponent(id),{method:'PUT',body:JSON.stringify(value)});
 if(epoch!==state.epoch)return;closeNode();$('bastionStatus').textContent='已校验 alpha-worker 免密登录并保存分享节点。';await load(epoch);
});});
$('bastionKeySync').addEventListener('click',()=>task(async e=>{await api('/api/bastion/keys/sync',{method:'POST',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已补齐用户公钥，free 条目保留。';await load(e);}));
$('bastionTailscalePool').addEventListener('click',event=>{const button=event.target.closest('[data-clean-key]');if(!button)return;task(async e=>{await api('/api/bastion/keys/'+encodeURIComponent(button.dataset.keyNode)+'/'+button.dataset.cleanKey,{method:'DELETE',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已清理 free 公钥。';await load(e);});});
$('bastionRefresh').addEventListener('click',()=>task(e=>load(e,true)));
$('bastionSettingsForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{const value={revision:state.settings.revision,tailnet:$('bastionTailnet').value,api_token:$('bastionToken').value};$('bastionToken').value='';await api('/api/tailscale/settings',{method:'PUT',body:JSON.stringify(value)});if(e===state.epoch){state.devices=[];$('bastionDevices').innerHTML='';$('bastionStatus').textContent='凭据已保存。';}await load(e,true);});});
$('bastionTest').addEventListener('click',()=>task(async e=>{const r=await api('/api/tailscale/test',{method:'POST',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent=`连接成功，可读取 ${r.device_count} 个节点。`;}));
$('bastionDevicesRefresh').addEventListener('click',()=>task(async e=>{const r=await api('/api/tailscale/devices');if(e!==state.epoch)return;state.devices=r.devices;$('bastionDevices').innerHTML=r.devices.map(d=>row(d.hostname||d.name,`${esc(d.nodeId)} · ${esc(d.addresses.join(' / '))}${d.isExternal?' · 外部分享节点':''}`,!d.isExternal&&d.authorized?`<button data-add-node="${esc(d.nodeId)}">配置并加入分享池</button>`:'')).join('')||'<p class="empty">暂无可用节点。</p>';}));
$('page-bastion').addEventListener('click',event=>{const b=event.target.closest('button');if(!b)return;if(b.dataset.addNode||b.dataset.editNode)return openNode(b.dataset.addNode||b.dataset.editNode);if(b.dataset.member){$('bastionDeleteForm').reset();$('bastionResolveForm').reset();$('bastionInviteCandidates').innerHTML='';return task(e=>member(e,b.dataset.member));}if(b.dataset.pool||b.dataset.removePool)task(async e=>{const type=b.dataset.pool||b.dataset.removePool;await api(`/api/bastion/${type}/${encodeURIComponent(b.dataset.id)}`,{method:b.dataset.pool?'PUT':'DELETE',body:JSON.stringify(b.dataset.pool?{enabled:b.dataset.enabled==='true'}:{})});await load(e);});});
$('bastionMemberClose').addEventListener('click',()=>$('bastionMemberDialog').close());
$('bastionMemberDialog').addEventListener('close',()=>{state.member=null;$('bastionMemberContent').innerHTML='';$('bastionInviteCandidates').innerHTML='';});
$('bastionShareRefresh').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/refresh`,{method:'POST',body:'{}'});await member(e,id);}));
$('bastionMemberRefresh').addEventListener('click',()=>task(e=>member(e,state.member.member_id)));
$('bastionMemberRetry').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/members/${id}/retry`,{method:'POST',body:'{}'});await member(e,id);}));
$('bastionDeleteForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{if($('bastionDeleteConfirm').value!==state.member.access.username)throw Error('请填写完整使用者标识确认删除。');await api(`/api/members/${state.member.member_id}`,{method:'DELETE',body:'{}'});if(e!==state.epoch)return;$('bastionMemberDialog').close();$('bastionStatus').textContent='使用者已删除，容器保留并标为未归属，等待管理员手动回收。';await load(e);});});
$('bastionResolveForm').addEventListener('submit',event=>{event.preventDefault();task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/resolve-invite`,{method:'POST',body:JSON.stringify({invite_id:$('bastionResolveID').value})});await member(e,id);});});
$('bastionConfirmAbsent').addEventListener('click',()=>task(async e=>{const id=state.member.member_id;await api(`/api/bastion/members/${id}/resolve-invite`,{method:'POST',body:JSON.stringify({confirm_absent:true})});await member(e,id);}));
$('bastionListInvites').addEventListener('click',()=>task(async e=>{const r=await api(`/api/bastion/tailscale/${encodeURIComponent(state.member.access.tailscale_id)}/invites`);if(e!==state.epoch)return;$('bastionInviteCandidates').innerHTML=r.invites.map(v=>row(v.id,`${esc(v.created)} · ${v.accepted?'已接受':'等待接受'}${v.acceptedBy?.loginName?' · '+esc(v.acceptedBy.loginName):''}`)).join('')||'<p>暂无邀请。</p>';}));
})();
