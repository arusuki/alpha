'use strict';
(()=>{
const state={epoch:0,busy:false,settings:null,data:null,member:null,node:null,devices:[]};
const labels={pending:'待分配',running:'创建中',invited:'等待接受',accepted:'已接受',ready:'已就绪',failed:'失败',creating:'邀请创建中',unknown:'邀请待核对',deleted:'已移除',deleting:'删除中',unallocated:'未申请'};
const label=s=>labels[s]||s;
function controls(){
  $('bastionSettingsFields').disabled=state.busy||!state.settings;
  $('bastionNodeFields').disabled=state.busy;
  for(const root of [$('page-bastion'),$('bastionMemberDialog'),$('bastionNodeDialog')])root.querySelectorAll('button').forEach(e=>{if(e.id!=='bastionMemberClose')e.disabled=state.busy||e.dataset.locked==='true';});
}
function row(title,detail,actions=''){return `<article class="bastion-row"><div><strong>${esc(title)}</strong><div class="bastion-detail">${detail}</div></div><div class="actions">${actions}</div></article>`;}
function statusTag(value){const tone=['accepted','ready'].includes(value)?'success':['failed','unknown','deleting'].includes(value)?'warning':'neutral';return `<span class="resource-tag" data-tone="${tone}">${esc(label(value))}</span>`;}
function poolButtons(r){return `<button data-edit-node="${esc(r.id)}">配置</button><button data-pool="tailscale" data-id="${esc(r.id)}" data-enabled="${r.enabled?'false':'true'}">${r.enabled?'停用':'启用'}</button><button data-remove-pool="tailscale" data-id="${esc(r.id)}" ${r.member_count?'disabled data-locked="true"':''}>移除</button>`;}
function render(){
 const d=state.data;if(!d)return;
 $('bastionShareSummary').textContent=d.tailscale.length?`已配置 ${d.tailscale.length} 个 share node。`:'先在 share node 初始化两个账号，再查询节点并加入分享池。';
 const pool=d.key_pool;
 $('bastionKeyPoolError').textContent=pool.error;
 $('bastionKeySync').dataset.locked=String(!d.tailscale.length);
 $('bastionKeyPool').innerHTML=pool.keys.map(k=>row(k.node_name+' · '+k.fingerprint,`<strong>${k.state==='free'?'free · 未关联用户':'已关联'}</strong>${k.members.length?' · '+k.members.map(m=>esc(m.username)).join('、'):''}<details><summary>查看公钥</summary><code class="pool-public-key">${esc(k.public_key)}</code><small class="sub">条目 ${esc(k.id)}</small></details>`,k.state==='free'?`<button data-clean-key="${esc(k.id)}" data-key-node="${esc(k.node_id)}">清理 free 公钥</button>`:'')).join('')||'<p class="empty">公钥池暂无条目。</p>';
 $('bastionTailscalePool').innerHTML=d.tailscale.map(r=>row(r.name,`<p class="mono">${esc(r.ssh_host)} · SSH ${r.ssh_port} · 总控入口 ${r.status_port}</p><p>总控地址：${esc(r.control_url)}</p><div class="resource-inline"><span class="resource-tag" ${r.enabled?'data-tone="success"':''}>${r.enabled?'可分配':'已停用'}</span><span>${r.member_count} 名使用者</span></div><small class="sub mono">${esc(r.id)}</small>`,poolButtons(r))).join('')||'<p class="empty">尚未选择分享节点，查询节点后可加入分享池。</p>';
 $('bastionAssignments').innerHTML=d.assignments.map(r=>`<article class="bastion-assignment"><div class="bastion-member-name"><strong>${esc(r.username)}</strong>${r.member_status==='deleting'?'<small class="sub">正在删除</small>':''}</div><div class="bastion-member-state"><span>Tailscale 邀请</span>${statusTag(r.invite_state)}</div><div class="bastion-member-state"><span>alpha-jump 公钥</span>${statusTag(r.key_state)}</div><button data-member="${esc(r.member_id)}">查看资源 <span aria-hidden="true">↗</span></button>${r.error?`<p class="error-text">${esc(r.error)}</p>`:''}</article>`).join('')||'<p class="empty">暂无使用者资源。完成注册后，可在这里查看授权和分配情况。</p>';
}
async function task(fn){if(state.busy||platform.user?.role!=='admin')return;const epoch=state.epoch;state.busy=true;controls();$('bastionError').textContent='';$('bastionMemberError').textContent='';$('bastionNodeError').textContent='';try{await fn(epoch);}catch(e){if(epoch===state.epoch)$($('bastionNodeDialog').open?'bastionNodeError':$('bastionMemberDialog').open?'bastionMemberError':'bastionError').textContent=e.message;}finally{if(epoch===state.epoch){state.busy=false;controls();}}}
async function load(epoch,settings=false){const [data,cfg]=await Promise.all([api('/api/bastion/resources'),settings?api('/api/tailscale/settings'):null]);if(epoch!==state.epoch)return;state.data=data;if(cfg){state.settings=cfg;$('bastionTailnet').value=cfg.tailnet;$('bastionTokenStatus').textContent=cfg.has_api_token?'已保存 API Key · 版本 '+cfg.revision:'尚未配置 API Key';}render();}
async function member(epoch,id){const v=await api(`/api/members/${id}/resources`);if(epoch!==state.epoch)return;state.member=v;const a=v.access;$('bastionMemberTitle').textContent=a.username+' · '+(v.status==='deleting'?'正在删除':'资源');
 $('bastionMemberContent').innerHTML=`<p>Tailscale：${esc(a.tailscale_id||'待分配')} · ${esc(label(a.invite_state))}${a.accepted_by?' · '+esc(a.accepted_by):''}</p>${a.invite_url?`<label>分享邀请链接<input readonly value="${esc(a.invite_url)}"></label>`:''}<p>alpha-jump 公钥：${esc(label(a.key_state))}</p>${a.error?`<p class="error-text">${esc(a.error)}</p>`:''}<div class="bastion-list">${v.nodes.map(n=>row(n.node_name,`${esc(label(n.state))}${n.name?' · '+esc(n.name):''}${n.port?' · '+esc(n.internal_ip)+' · 端口 '+n.port:''}${n.error?'<br><span class="error-text">'+esc(n.error)+'</span>':''}`)).join('')||'<p>尚未添加 node。</p>'}</div>`;
 $('bastionResolveForm').hidden=!['unknown','creating'].includes(a.invite_state);$('bastionMemberRetry').hidden=v.status!=='active';
 if(!$('bastionMemberDialog').open)$('bastionMemberDialog').showModal();
}
function closeNode(){state.node=null;$('bastionNodeDialog').close();}
window.BastionUI={open(){return task(e=>load(e,true));},leave(){$('bastionToken').value='';closeNode();},reset(){closeNode();state.epoch++;state.busy=false;state.settings=null;state.data=null;state.member=null;state.devices=[];$('bastionToken').value='';$('bastionMemberDialog').close();for(const id of ['bastionDevices','bastionTailscalePool','bastionAssignments','bastionMemberContent','bastionInviteCandidates','bastionKeyPool'])$(id).innerHTML='';$('bastionError').textContent='';$('bastionStatus').textContent='';$('bastionSettingsForm').reset();controls();}};
function openNode(id){
 if(state.busy||platform.user?.role!=='admin')return;
 const device=state.devices.find(d=>d.nodeId===id),saved=state.data.tailscale.find(n=>n.id===id);
 if(!device&&!saved)return;
 state.node={id,enabled:saved?!!saved.enabled:true};$('bastionNodeError').textContent='';$('bastionNodeTitle').textContent=(saved?'配置':'加入')+' share node · '+(saved?.name||device?.hostname||device?.name);
 const addresses=device?.addresses||[saved.ssh_host];
 $('bastionNodeHost').innerHTML=addresses.map(ip=>`<option value="${esc(ip)}">${esc(ip)}</option>`).join('');
 $('bastionNodeHost').value=saved?.ssh_host||addresses[0];$('bastionNodeSSHPort').value=saved?.ssh_port||22;$('bastionNodeStatusPort').value=saved?.status_port||8765;
 $('bastionNodeDialog').showModal();controls();
}
$('bastionNodeClose').addEventListener('click',closeNode);
$('bastionNodeDialog').addEventListener('close',()=>{state.node=null;});
$('bastionNodeDialog').addEventListener('cancel',event=>{if(state.busy)event.preventDefault();});
$('bastionNodeForm').addEventListener('submit',event=>{event.preventDefault();task(async epoch=>{
 const id=state.node.id,value={enabled:state.node.enabled,ssh_host:$('bastionNodeHost').value,ssh_port:Number($('bastionNodeSSHPort').value),status_port:Number($('bastionNodeStatusPort').value)};
 await api('/api/bastion/tailscale/'+encodeURIComponent(id),{method:'PUT',body:JSON.stringify(value)});
 if(epoch!==state.epoch)return;closeNode();$('bastionStatus').textContent='已校验 alpha-worker 免密登录并保存分享节点。';await load(epoch);
});});
$('bastionKeySync').addEventListener('click',()=>task(async e=>{await api('/api/bastion/keys/sync',{method:'POST',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已补齐用户公钥，free 条目保留。';await load(e);}));
$('bastionKeyPool').addEventListener('click',event=>{const button=event.target.closest('[data-clean-key]');if(!button)return;task(async e=>{await api('/api/bastion/keys/'+encodeURIComponent(button.dataset.keyNode)+'/'+button.dataset.cleanKey,{method:'DELETE',body:'{}'});if(e===state.epoch)$('bastionStatus').textContent='已清理 free 公钥。';await load(e);});});
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
