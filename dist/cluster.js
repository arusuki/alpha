'use strict';
(()=>{
const state={data:null,epoch:0,pending:null,editing:null,removing:null,busy:false,opening:false,unavailableTarget:null,filter:'all',nodesHTML:'',sharing:null,shareSequence:0,reconnecting:new Set(),reconnectErrors:new Map()};
const admin=()=>platform.user?.role==='admin';
const nodeAvailable=n=>n.online&&(n.kind==='registry'||!!n.inventory);
function configure(){
  const central=!platform.nodeID;
  document.body.classList.toggle('control-room',central);
  document.querySelectorAll('[data-control-only]').forEach(e=>e.hidden=!central);
  document.querySelectorAll('.platform-nav [data-page]').forEach(e=>{
    const nodePage=['dashboard','overview','containers','processes'].includes(e.dataset.page);
    e.hidden=(e.hasAttribute('data-control-only')?!central:nodePage?central:!!platform.nodeID)||(e.hasAttribute('data-admin')&&!admin());
  });
  document.querySelectorAll('[data-page="agent-settings"],[data-page="members"],[data-page="bastion"]').forEach(e=>e.hidden=!!platform.nodeID||!admin());
  $('agentGuideStorage').hidden=central;
  $('agentGuideNodes').hidden=!central;
  $('nodeContext').hidden=!platform.nodeID;
  $('newContainerOwner').required=true;
  $('workspaceDate').textContent=new Date().toLocaleDateString('zh-CN',{year:'numeric',month:'long',day:'numeric',weekday:'long'});
  if(platform.nodeID){
    const epoch=state.epoch;
    $('nodeContextName').textContent='正在读取节点…';
    api(`/api/cluster/nodes/${platform.nodeID}`).then(n=>{if(epoch===state.epoch)$('nodeContextName').textContent=n.name;}).catch(e=>{if(epoch===state.epoch)$('nodeContextStatus').textContent=e.message;});
    if(admin())api('/api/members').then(data=>{if(epoch===state.epoch)$('clusterMemberOptions').innerHTML=data.members.map(m=>`<option value="${esc(m.username)}"></option>`).join('');}).catch(e=>{if(epoch===state.epoch)$('nodeContextStatus').textContent='使用者列表读取失败：'+e.message;});
  }
}
function render(){
  const data=state.data;if(!data)return;
  $('clusterOnline').textContent=`${data.online} / ${data.nodes.length}`;
  $('clusterContainers').textContent=data.container_count;
  const owners=data.members.filter(m=>m.count&&m.username).length;
  const unassigned=data.members.find(m=>!m.username)?.count||0;
  const unavailable=data.nodes.filter(n=>!nodeAvailable(n)).length;
  $('clusterOwners').textContent=owners;
  $('clusterUnassigned').textContent=unassigned;
  $('clusterNodeCount').textContent=String(data.nodes.length).padStart(2,'0');
  $('clusterHealth').textContent=!data.nodes.length?'等待第一个节点接入':unavailable?`${unavailable} 个节点暂时不可用`:'所有节点连接正常';
  $('clusterHealth').parentElement.dataset.state=!data.nodes.length?'empty':unavailable?'partial':'online';
  $('clusterChecked').textContent=`检查于 ${dateTime(data.checked_at)}`;
  $('clusterPartial').hidden=!data.partial;
  $('clusterPartial').textContent='部分计算节点离线、协议不一致或详情读取失败，容器统计仅包含已成功读取的节点。';
  renderNodes();
  $('allocationContainerCount').textContent=data.container_count;
  $('allocationOwnerCount').textContent=owners;
  $('allocationUnassignedCount').textContent=unassigned;
  $('allocationNodeCount').textContent=data.nodes.filter(n=>n.kind==='worker'&&nodeAvailable(n)).length;
  $('allocationStatus').dataset.state=data.partial?'partial':'ready';
  $('allocationStatus').textContent=(data.partial?'统计不完整：部分节点不可用。':'计算节点统计完整。')+` ${data.container_count-unassigned} 个有使用者的容器 · ${unassigned} 个未归属容器 · ${dateTime(data.checked_at)}`;
  renderAllocations();
}
function renderNodes(){
  const data=state.data;if(!data)return;
  for(const n of data.nodes)if(nodeAvailable(n))state.reconnectErrors.delete(n.id);
  const query=$('clusterSearch').value.trim().toLowerCase();
  const nodes=data.nodes.filter(n=>(state.filter==='all'||nodeAvailable(n)===(state.filter==='online'))&&(!query||[n.name,n.url,n.internal_ip,n.kind,n.inventory?.host].join(' ').toLowerCase().includes(query)));
  document.querySelectorAll('[data-node-filter]').forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.nodeFilter===state.filter)));
  const html=nodes.map(n=>{
    if(n.kind==='registry')return renderRegistryNode(n,data.nodes.indexOf(n)+1);
    const inventory=n.inventory;
    const owners=inventory?new Set(inventory.containers.map(c=>c.owner).filter(Boolean)).size:0;
    const scanned=inventory?.observed_at?new Date(inventory.observed_at).toLocaleString('zh-CN'):'';
    return `<article class="node-card ${nodeAvailable(n)?'':'node-offline'}"><div class="node-card-heading"><span class="node-index">${String(data.nodes.indexOf(n)+1).padStart(2,'0')} / WORKER</span><span class="node-status"><i></i>${!n.online?'离线':!inventory?'详情不可用':'在线'}</span></div><h3>${esc(n.name)}</h3><p class="mono node-address">${esc(n.url)}</p><p class="mono node-address">内网 IP · ${esc(n.internal_ip)}</p><div class="node-metrics"><div><strong>${inventory?inventory.containers.length:'—'}</strong><span>容器记录</span></div><div><strong>${inventory?owners:'—'}</strong><span>有容器的使用者</span></div><svg class="node-symbol ui-icon" aria-hidden="true"><use href="#icon-storage"/></svg></div>${inventory?`<dl class="node-details"><div><dt>未归属容器</dt><dd>${inventory.containers.filter(c=>!c.owner).length}</dd></div><div><dt>主机</dt><dd>${esc(inventory.host||'尚未获取')}</dd></div><div><dt>扫描任务</dt><dd ${inventory.active?'class="node-scanning"':''}>${inventory.active?'扫描进行中':'当前空闲'}</dd></div></dl><p class="node-observed">${scanned?'最近扫描 · '+esc(scanned):'尚无完成的扫描'}</p>`:`<div class="node-unavailable"><strong>${n.online?'节点在线，详情暂不可用':'暂时无法连接此节点'}</strong><p>${esc(state.reconnectErrors.get(n.id)||n.error)}</p></div>`}<div class="node-actions"><a class="node-open" data-open-node="${n.id}" href="/nodes/${n.id}/" aria-label="进入节点 ${esc(n.name)}">进入节点 <span aria-hidden="true">↗</span></a>${admin()?'<button data-page="update-settings">更新设置</button>':''}${reconnectButton(n)}${admin()?`<button data-edit-node="${n.id}" aria-label="编辑节点 ${esc(n.name)}">编辑</button><button data-remove-node="${n.id}" aria-label="移除节点 ${esc(n.name)}">移除</button>`:''}</div></article>`;
  }).join('')||(!data.nodes.length?`<div class="cluster-empty"><span class="empty-node-symbol" aria-hidden="true">＋</span><h3>${admin()?'连接你的第一个节点':'等待节点接入'}</h3><p>${admin()?'添加主机后，在这里统一查看状态并进入管理。':'管理员添加节点后，这里会显示你的主机。'}</p>${admin()?'<button class="primary" data-add-node>添加节点 ↗</button>':''}</div>`:'<div class="cluster-empty"><h3>没有匹配的节点</h3><p>试试其他名称、主机地址或连接状态。</p><button data-clear-nodes>清除筛选</button></div>');
  const container=$('clusterNodes');
  if(state.nodesHTML===html)return;
  const active=document.activeElement;
  const focus=container.contains(active)?['href','data-share-registry','data-reconnect-node','data-edit-node','data-remove-node','data-clear-nodes','data-add-node'].map(attr=>[attr,active.getAttribute(attr)]).find(([,value])=>value!==null):null;
  container.innerHTML=html;
  state.nodesHTML=html;
  if(focus){const [attr,value]=focus;const target=[...container.querySelectorAll(`[${attr}]`)].find(el=>el.getAttribute(attr)===value);(target||$('clusterSearch')).focus({preventScroll:true});}
}
function renderRegistryNode(n,index){
  const connection=n.connection;
  const error=state.reconnectErrors.get(n.id)||n.error;
  const status=n.online?'已连接':({connecting:'连接中',reconnecting:'重连中',disconnected:'未连接'}[connection?.state]||'未连接');
  return `<article class="node-card ${nodeAvailable(n)?'':'node-offline'}"><div class="node-card-heading"><span class="node-index">${String(index).padStart(2,'0')} / REGISTRY</span><span class="node-status"><i></i>${status}</span></div><h3>${esc(n.name)}</h3><p class="mono node-address">${esc(n.url)}</p><div class="node-metrics"><div><strong>注册入口</strong><span>由总控主动连接</span></div><svg class="node-symbol ui-icon" aria-hidden="true"><use href="#icon-user"/></svg></div><dl class="node-details"><div><dt>连接令牌</dt><dd>•••••• · 已保存</dd></div><div><dt>最近连接</dt><dd>${connection?.connected_at?esc(dateTime(connection.connected_at)):'等待连接'}</dd></div></dl>${error?`<div class="node-unavailable"><p>${esc(error)}</p></div>`:''}<p class="node-observed">${connection?.last_seen?'最近通信 · '+esc(dateTime(connection.last_seen)):'正在等待连接确认'}</p>${admin()?`<div class="node-actions"><button class="node-open" data-share-registry="${n.id}" aria-label="生成 ${esc(n.name)} 的共享注册链接">共享链接 ↗</button>${reconnectButton(n)}<button data-edit-node="${n.id}" aria-label="编辑节点 ${esc(n.name)}">编辑</button><button data-remove-node="${n.id}" aria-label="移除节点 ${esc(n.name)}">移除</button></div>`:''}</article>`;
}
function reconnectButton(n){
  if(!admin()||(nodeAvailable(n)&&!state.reconnecting.has(n.id)))return '';
  const pending=state.reconnecting.has(n.id);
  return `<button data-reconnect-node="${n.id}" aria-label="重连节点 ${esc(n.name)}" ${pending?'disabled aria-busy="true"':''}>${pending?'正在重连…':'重连'}</button>`;
}
async function reconnectNode(id){
  if(!admin()||state.reconnecting.has(id)||!state.data?.nodes.some(n=>n.id===id))return;
  const epoch=state.epoch;state.reconnecting.add(id);state.reconnectErrors.delete(id);renderNodes();
  try{
    await api(`/api/cluster/nodes/${id}/reconnect`,{method:'POST',body:'{}',signal:AbortSignal.timeout(12000)});
    if(epoch!==state.epoch)return;
    if(state.pending)await state.pending;
    if(epoch===state.epoch)await refresh();
  }catch(error){if(epoch===state.epoch)state.reconnectErrors.set(id,error.name==='TimeoutError'?'节点重连超时，请稍后重试。':error.message);}
  finally{if(epoch===state.epoch){state.reconnecting.delete(id);renderNodes();}}
}
function nodeKindControls(){
  const registry=$('nodeKind').value==='registry';
  $('nodeInternalIPLabel').hidden=registry;$('nodeInternalIP').required=!registry;
  $('nodeURL').placeholder=registry?'https://register.example.com':'http://10.0.0.11:8765';
  $('nodeKindHint').textContent=registry?'公网注册入口：总控主动连接，支持多个入口各自设置令牌。公网地址需使用 HTTPS。':'计算节点：提供容器、存储扫描与进程管理。';
}
function clearShareResult(){
  $('registryShareURL').value='';$('registryShareResult').hidden=true;
  $('registryShareCopy').disabled=true;$('registryShareStatus').textContent='';$('registryShareError').textContent='';
}
function resetShare(){
  state.shareSequence++;state.sharing=null;clearShareResult();
  $('registryShareNode').textContent='';$('registryShareHint').textContent='';
  $('registryShareInvitation').innerHTML='';$('registryShareInvitation').disabled=true;
}
async function shareRegistry(id){
  if(!admin())return;
  const node=state.data?.nodes.find(n=>n.id===id&&n.kind==='registry');if(!node)return;
  resetShare();state.sharing=id;
  const sequence=state.shareSequence;
  $('registryShareNode').textContent=`${node.name} · ${node.url}`;
  $('registryShareInvitation').innerHTML='<option value="">正在读取邀请码…</option>';
  $('registryShareDialog').showModal();
  try{
    const options=await window.MembersUI.invitationOptions();if(sequence!==state.shareSequence)return;
    $('registryShareInvitation').innerHTML=options;
    const available=$('registryShareInvitation').options.length>1;
    $('registryShareInvitation').disabled=!available;
    $('registryShareHint').textContent=available?'选择邀请码后自动生成注册链接。':'暂无可用邀请码，点击加号创建。';
  }catch(error){if(sequence===state.shareSequence)$('registryShareError').textContent=error.message;}
}
async function generateShare(){
  clearShareResult();
  const id=state.sharing,invitationID=$('registryShareInvitation').value;
  const sequence=++state.shareSequence;
  if(!id||!invitationID||!admin())return;
  $('registryShareInvitation').disabled=true;$('registryShareStatus').textContent='正在生成注册链接…';
  try{
    const result=await api(`/api/cluster/nodes/${id}/registration-link`,{method:'POST',body:JSON.stringify({invitation_id:invitationID})});
    if(sequence!==state.shareSequence)return;
    $('registryShareURL').value=result.url;$('registryShareResult').hidden=false;$('registryShareCopy').disabled=false;
    $('registryShareStatus').textContent='链接已生成，可复制并发给使用者。';
  }catch(error){if(sequence===state.shareSequence){$('registryShareStatus').textContent='';$('registryShareError').textContent=error.message;}}
  finally{if(sequence===state.shareSequence)$('registryShareInvitation').disabled=false;}
}
function renderAllocations(){
  const data=state.data;if(!data)return;
  const query=$('allocationSearch').value.trim().toLowerCase();
  const expanded=new Map([...$('allocationRows').querySelectorAll('details[data-owner]')].map(el=>[el.dataset.owner,el.open]));
  const rows=data.members.filter(m=>!query||[m.username,...m.nodes.flatMap(n=>[n.name,...n.containers.flatMap(c=>[c.name,c.id])])].join(' ').toLowerCase().includes(query));
  const memberCount=data.members.filter(m=>m.username).length;
  const matchedMembers=rows.filter(m=>m.username).length;
  $('allocationResultCount').textContent=query?`${matchedMembers} / ${memberCount} 名使用者`:`${memberCount} 名使用者`;
  const regular=rows.filter(m=>m.username);
  const unassigned=rows.filter(m=>!m.username);
  const renderUser=m=>`<details class="allocation-user" data-owner="${esc(m.username)}" ${query||(expanded.get(m.username)??false)?'open':''}>
    <summary><span class="allocation-avatar" aria-hidden="true">${esc(Array.from(m.username||'?')[0].toUpperCase())}</span><span class="allocation-identity"><strong>${esc(m.username||'未归属容器')}</strong>${!m.username?'<small>不计入使用者人数</small>':''}</span><span class="allocation-counts">${m.count} 个容器 · ${m.nodes.length} 个节点</span>${admin()&&m.registered&&m.id?`<button class="danger" data-delete-member="${esc(m.id)}">删除使用者</button>`:''}<span class="allocation-chevron" aria-hidden="true">›</span></summary>
    <div class="allocation-content">${m.nodes.map(n=>`<section class="allocation-node"><h3><a data-open-node="${n.id}" href="/nodes/${n.id}/#containers"><svg class="ui-icon" aria-hidden="true"><use href="#icon-storage"/></svg>${esc(n.name)} <span aria-hidden="true">↗</span></a><small>${n.containers.length} 个容器</small></h3><div class="table-scroll"><table><thead><tr><th scope="col">容器 / ID</th><th scope="col">记录来源</th><th scope="col">扫描时状态</th></tr></thead><tbody>${n.containers.map(c=>`<tr><td><strong>${esc(c.name)}</strong><small class="sub mono">${esc(c.id)}</small></td><td><span class="resource-tag">${c.managed?'已接管':'扫描发现'}</span></td><td><span class="resource-tag" ${c.state==='running'?'data-tone="success"':''}>${esc(c.state||'尚无扫描状态')}</span><small class="sub">${esc(c.observed_at?new Date(c.observed_at).toLocaleString('zh-CN'):'')}</small></td></tr>`).join('')}</tbody></table></div></section>`).join('')||`<p class="empty">${data.partial?'已读取的节点中暂无该使用者的容器记录，其他节点尚未确认。':'暂无该使用者的容器记录。'}</p>`}</div></details>`;
  $('allocationRows').innerHTML=[...regular,...unassigned].map(renderUser).join('')||`<div class="cluster-empty"><h3>${query?'没有匹配的使用者或容器':'暂无使用者容器记录'}</h3><p>${query?'试试其他使用者、节点名称或容器 ID。':'节点接入并分配容器后，可在这里查看归属。'}</p></div>`;
}
async function refresh(){
  if(!platform.user||platform.nodeID)return;
  if(state.pending)return state.pending;
  const epoch=state.epoch;
  const pending=(async()=>{try{
    const data=await api('/api/cluster/overview',{signal:AbortSignal.timeout(12000)});if(epoch!==state.epoch)return;
    state.data=data;$('clusterError').textContent='';render();
  }catch(e){if(epoch===state.epoch){$('clusterError').textContent=(state.data?'刷新失败，以下保留上次结果：':'无法读取节点：')+e.message;$('clusterHealth').textContent='集群状态更新失败';$('clusterHealth').parentElement.dataset.state='partial';if(!state.data)$('clusterNodes').innerHTML='<div class="cluster-empty"><h3>暂时无法读取节点</h3><p>请使用「刷新状态」重新连接。</p></div>';$('allocationStatus').dataset.state='partial';$('allocationStatus').textContent=(state.data?'刷新失败，以下为上次结果：':'无法读取统计：')+e.message;}}
  finally{if(epoch===state.epoch){state.pending=null;$('clusterNodes').setAttribute('aria-busy','false');$('clusterRefresh').disabled=false;$('allocationsRefresh').disabled=false;}}})();
  state.pending=pending;$('clusterNodes').setAttribute('aria-busy','true');$('clusterRefresh').disabled=true;$('allocationsRefresh').disabled=true;return pending;
}
function edit(id=null){
  if(!admin()||state.busy)return;
  const n=state.data?.nodes.find(n=>n.id===id);state.editing=id;
  $('nodeForm').reset();$('nodeName').value=n?.name||'';$('nodeURL').value=n?.url||'';$('nodeInternalIP').value=n?.internal_ip||'';
  $('nodeKind').value=n?.kind||'worker';$('nodeKind').disabled=!!id;nodeKindControls();
  $('nodeToken').required=!id;$('nodeDialogTitle').textContent=id?'编辑节点':'添加节点';
  $('nodeTokenHint').textContent=id?'未输入新令牌时保留原值；更换地址时会核对节点身份。':'输入此节点的连接令牌；令牌只保存在总控服务端。';
  $('nodeFormError').textContent='';$('nodeDialog').showModal();
}
function busy(value){state.busy=value;for(const id of ['nodeSave','nodeCancel','nodeRemoveConfirm','nodeRemoveCancel'])$(id).disabled=value;}
function unavailable(error,name='当前节点',target=null){
  state.unavailableTarget=target;
  $('nodeUnavailableDescription').textContent=`${name} 暂时不可用，请检查节点连接后重试。`;
  $('nodeUnavailableError').textContent=error||'';
  $('nodeUnavailableHome').hidden=!platform.nodeID;
  if(!$('nodeUnavailableDialog').open)$('nodeUnavailableDialog').showModal();
}
function connected(){
  $('nodeContextStatus').textContent='';
  if($('nodeUnavailableDialog').open)$('nodeUnavailableDialog').close();
  state.unavailableTarget=null;
}
async function connectNode(node,href){
  if(state.opening)return;
  const epoch=state.epoch;state.opening=true;$('nodeUnavailableRetry').disabled=true;
  try{
    // Offline status is only the last observation; always probe again on demand.
    await api(`/api/cluster/nodes/${node.id}/api/state`,{signal:AbortSignal.timeout(12000)});
    if(epoch===state.epoch)window.location.href=href;
  }catch(error){if(epoch===state.epoch&&platform.user)unavailable(error.name==='TimeoutError'?'节点连接超时，请稍后重试。':error.message,node.name,{node,href});}
  finally{if(epoch===state.epoch){state.opening=false;$('nodeUnavailableRetry').disabled=false;}}
}
async function openNode(event){
  const link=event.target.closest('[data-open-node]');if(!link)return;
  event.preventDefault();
  const node=state.data?.nodes.find(n=>n.id===link.dataset.openNode);if(!node)return;
  link.setAttribute('aria-busy','true');
  try{await connectNode(node,link.href);}finally{link.removeAttribute('aria-busy');}
}
async function retryNode(){
  if(state.opening)return;
  if(!platform.nodeID){
    const target=state.unavailableTarget;
    if(target)await connectNode(target.node,target.href);
    return;
  }
  const epoch=state.epoch;state.opening=true;$('nodeUnavailableRetry').disabled=true;
  try{await syncState();if(epoch===state.epoch)showPage(platform.page,false);}
  catch(error){if(epoch===state.epoch&&platform.user)unavailable(error.message);}
  finally{if(epoch===state.epoch){state.opening=false;$('nodeUnavailableRetry').disabled=false;}}
}
window.ClusterUI={configure,refresh,unavailable,connected,reset(){
  state.epoch++;state.opening=false;state.unavailableTarget=null;$('nodeUnavailableRetry').disabled=false;state.data=null;state.pending=null;state.editing=null;state.removing=null;state.filter='all';state.nodesHTML='';busy(false);
  state.reconnecting.clear();state.reconnectErrors.clear();
  document.body.classList.remove('control-room');
  $('clusterSearch').value='';$('allocationSearch').value='';$('clusterError').textContent='';$('clusterPartial').hidden=true;$('clusterNodeCount').textContent='';
  for(const id of ['clusterOnline','clusterContainers','clusterOwners','allocationContainerCount','allocationOwnerCount','allocationNodeCount'])$(id).textContent='—';
  $('allocationResultCount').textContent='';$('allocationStatus').textContent='';delete $('allocationStatus').dataset.state;
  $('clusterHealth').textContent='正在连接集群…';$('clusterHealth').parentElement.dataset.state='empty';$('clusterChecked').textContent='正在获取节点状态…';
  document.querySelectorAll('[data-node-filter]').forEach(button=>button.setAttribute('aria-pressed',String(button.dataset.nodeFilter==='all')));
  resetShare();
  for(const id of ['nodeDialog','nodeRemoveDialog','nodeUnavailableDialog','registryShareDialog'])if($(id).open)$(id).close();
  $('nodeToken').value='';$('clusterMemberOptions').innerHTML='';$('allocationRows').innerHTML='';$('clusterNodes').innerHTML='<p class="cluster-empty">正在读取节点…</p>';$('nodeContextStatus').textContent='';
}};
$('nodeUnavailableRetry').addEventListener('click',retryNode);
$('nodeUnavailableClose').addEventListener('click',()=>$('nodeUnavailableDialog').close());
$('clusterNodes').addEventListener('click',openNode);
$('allocationRows').addEventListener('click',openNode);
$('allocationRows').addEventListener('click',event=>{
  const button=event.target.closest('[data-delete-member]');if(!button||!admin())return;
  event.preventDefault();event.stopPropagation();
  const member=state.data?.members.find(m=>m.id===button.dataset.deleteMember);if(!member)return;
  window.MembersUI.confirmDelete(member.id,member.username,async()=>{if(state.pending)await state.pending;await refresh();});
});
$('clusterRefresh').addEventListener('click',refresh);$('allocationsRefresh').addEventListener('click',refresh);
$('allocationSearch').addEventListener('input',renderAllocations);
$('clusterSearch').addEventListener('input',renderNodes);
document.querySelectorAll('[data-node-filter]').forEach(button=>button.addEventListener('click',()=>{state.filter=button.dataset.nodeFilter;renderNodes();}));
$('clusterAdd').addEventListener('click',()=>edit());
$('nodeKind').addEventListener('change',nodeKindControls);
$('nodeCancel').addEventListener('click',()=>$('nodeDialog').close());
$('nodeDialog').addEventListener('close',()=>{$('nodeToken').value='';});
for(const id of ['nodeDialog','nodeRemoveDialog'])$(id).addEventListener('cancel',e=>{if(state.busy)e.preventDefault();});
$('clusterNodes').addEventListener('click',e=>{
  if(e.target.closest('[data-add-node]')){edit();return;}
  if(e.target.closest('[data-clear-nodes]')){state.filter='all';$('clusterSearch').value='';renderNodes();$('clusterSearch').focus();return;}
  const share=e.target.closest('[data-share-registry]');if(share){shareRegistry(share.dataset.shareRegistry);return;}
  const reconnect=e.target.closest('[data-reconnect-node]');if(reconnect){reconnectNode(reconnect.dataset.reconnectNode);return;}
  const editButton=e.target.closest('[data-edit-node]');if(editButton){edit(editButton.dataset.editNode);return;}
  const remove=e.target.closest('[data-remove-node]');if(!remove||!admin()||state.busy)return;
  state.removing=state.data.nodes.find(n=>n.id===remove.dataset.removeNode);if(!state.removing)return;
  $('nodeRemoveDescription').textContent=state.removing.kind==='registry'?`移除 ${state.removing.name} 并断开注册连接？此入口将无法办理注册；registry 保存的总控绑定与会话仍会保留。`:`从总控移除 ${state.removing.name}？`;$('nodeRemoveError').textContent='';$('nodeRemoveDialog').showModal();
});
$('registryShareInvitation').addEventListener('change',generateShare);
$('registryShareClose').addEventListener('click',()=>{resetShare();$('registryShareDialog').close();});
$('registryShareDialog').addEventListener('cancel',resetShare);
$('registryShareDialog').addEventListener('close',()=>{if(state.sharing)resetShare();});
$('registryShareCreate').addEventListener('click',()=>{
  resetShare();$('registryShareDialog').close();showPage('members');
  $('memberInvitationForm').scrollIntoView({behavior:'instant',block:'center'});$('memberInvitationLabel').focus({preventScroll:true});
});
$('registryShareCopy').addEventListener('click',async()=>{
  const url=$('registryShareURL').value,sequence=state.shareSequence;if(!url)return;
  try{await navigator.clipboard.writeText(url);if(sequence===state.shareSequence)$('registryShareStatus').textContent='链接已复制。';}
  catch(_){if(sequence===state.shareSequence){$('registryShareURL').select();$('registryShareStatus').textContent='请手动复制所选链接。';}}
});
$('nodeForm').addEventListener('submit',async e=>{
  e.preventDefault();if(state.busy||!admin())return;
  const epoch=state.epoch,id=state.editing;busy(true);$('nodeFormError').textContent='';
  const body=JSON.stringify({kind:$('nodeKind').value,name:$('nodeName').value,url:$('nodeURL').value,internal_ip:$('nodeKind').value==='worker'?$('nodeInternalIP').value.trim():'',token:$('nodeToken').value});$('nodeToken').value='';
  try{await api('/api/cluster/nodes'+(id?'/'+id:''),{method:id?'PUT':'POST',body});if(epoch!==state.epoch)return;$('nodeDialog').close();if(state.pending)await state.pending;await refresh();}
  catch(error){if(epoch===state.epoch)$('nodeFormError').textContent=error.message;}
  finally{if(epoch===state.epoch)busy(false);}
});
$('nodeRemoveCancel').addEventListener('click',()=>$('nodeRemoveDialog').close());
$('nodeRemoveForm').addEventListener('submit',async e=>{
  e.preventDefault();if(state.busy||!admin()||!state.removing)return;
  const epoch=state.epoch;busy(true);
  try{await api(`/api/cluster/nodes/${state.removing.id}`,{method:'DELETE'});if(epoch!==state.epoch)return;$('nodeRemoveDialog').close();if(state.pending)await state.pending;await refresh();}
  catch(error){if(epoch===state.epoch)$('nodeRemoveError').textContent=error.message;}
  finally{if(epoch===state.epoch)busy(false);}
});
})();
