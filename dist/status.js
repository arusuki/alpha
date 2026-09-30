'use strict';
(()=>{
  const $=id=>document.getElementById(id);
  const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const memberID=location.pathname.split('/')[2];
  const endpoint='/api/status/'+encodeURIComponent(memberID);
  const storageKey='alpha.member-token.'+memberID;
  const state={token:'',data:null,busy:false,epoch:0,timer:null,request:null};
  const labels={unallocated:'尚无容器',pending:'等待创建',running:'正在创建',ready:'已分配容器',failed:'创建失败',deleting:'正在回收',deleted:'尚无容器'};
  function stored(value){try{if(value===undefined)return sessionStorage.getItem(storageKey)||'';if(value)sessionStorage.setItem(storageKey,value);else sessionStorage.removeItem(storageKey);}catch{}return '';}
  function controls(){
    $('tokenSubmit').disabled=state.busy;
    $('refreshStatus').disabled=state.busy;
    document.querySelectorAll('[data-apply]').forEach(button=>{button.disabled=state.busy||button.dataset.available!=='true';});
  }
  function clearSession(message=''){
    state.epoch++;state.request?.abort();state.request=null;
    clearTimeout(state.timer);state.token='';state.data=null;state.busy=false;state.applying=null;stored('');
    $('resourceToken').value='';$('tokenPanel').hidden=false;$('statusContent').hidden=true;
    $('statusNodes').replaceChildren();$('memberIdentity').textContent='使用者 ID · '+memberID;
    $('statusError').textContent=message;$('statusMessage').textContent='';controls();
  }
  async function request(path,options={}){
    const controller=new AbortController();state.request=controller;
    try{
      const response=await fetch(path,{...options,cache:'no-store',credentials:'omit',signal:controller.signal,
        headers:{'Authorization':'Bearer '+state.token,...(options.body?{'Content-Type':'application/json'}:{})}});
      let value;try{value=await response.json();}catch{throw Error('服务返回的数据无效，请刷新重试');}
      if(!response.ok){const error=Error(value.error||'请求失败');error.status=response.status;throw error;}
      return value;
    }finally{if(state.request===controller)state.request=null;}
  }
  function containers(node){
    const rows=[...node.containers];
    if(node.container_id&&!rows.some(c=>c.id===node.container_id))rows.unshift({id:node.container_id,name:node.name,state:'',managed:true});
    return rows;
  }
  function render(){
    const data=state.data;
    $('memberIdentity').textContent=data.username+' · '+data.member_id;
    $('tokenPanel').hidden=true;$('statusContent').hidden=false;
    $('totalNodes').textContent=data.nodes.length;
    $('onlineNodes').textContent=data.nodes.filter(n=>n.online).length;
    $('allocatedNodes').textContent=data.nodes.filter(n=>containers(n).length>0||n.state==='ready').length;
    $('checkedAt').textContent='更新于 '+new Date(data.checked_at*1000).toLocaleTimeString();
    const focused=document.activeElement?.dataset?.apply;
    $('statusNodes').innerHTML=data.nodes.map((n,index)=>{
      const own=containers(n),hasContainer=own.length>0||n.state==='ready';
      const canApply=!hasContainer&&['unallocated','failed','deleted'].includes(n.state);
      const applying=n.node_id===state.applying;
      const allocation=applying?'正在创建':hasContainer?'已分配容器':labels[n.state]||n.state;
      return `<article class="node" data-node="${esc(n.node_id)}">
        <div class="node-top"><span class="node-index">NODE / ${String(index+1).padStart(2,'0')}</span><span class="connection ${n.online?'':'offline'}">${n.online?'在线':'离线 / 不可用'}</span></div>
        <div><h3>${esc(n.node_name)}</h3><p class="address">${esc(n.url)}</p></div>
        <dl class="details"><div><dt>主机</dt><dd>${esc(n.host||'未知')}</dd></div><div><dt>节点容器总数</dt><dd>${n.online?esc(n.container_count):'未知'}</dd></div><div><dt>扫描任务</dt><dd>${n.online?(n.scanning?'进行中':'空闲'):'未知'}</dd></div><div><dt>最近采集</dt><dd>${esc(n.observed_at||'尚无采集记录')}</dd></div></dl>
        ${n.connection_error?`<p class="node-note">${esc(n.connection_error)}</p>`:''}
        <div class="allocation"><div class="allocation-heading"><strong>${esc(allocation)}</strong>${canApply?`<button class="apply" data-apply="${esc(n.node_id)}" data-available="${n.online}" aria-label="${esc((n.state==='failed'?'重试':'申请')+' '+n.node_name+' 的容器')}" ${state.busy||!n.online?'disabled':''}><span aria-hidden="true">＋</span>${applying?'创建中…':n.state==='failed'?'重试':'申请'}</button>`:''}</div>
          ${own.length?`<ul class="container-list">${own.map(c=>`<li><code>${esc(c.name||c.id)}</code><small>${esc(c.state||'已登记 · 运行状态待采集')}</small></li>`).join('')}</ul>`:''}
          ${n.port?`<p class="node-note">SSH · root @ ${esc(n.ssh_host||n.host||'待配置地址')}:${esc(n.port)}</p>`:''}
          ${!hasContainer&&!n.online?'<p class="node-note">节点恢复在线后可申请。</p>':''}
          ${['pending','running'].includes(n.state)?'<p class="node-note">申请处理中，状态会自动更新。</p>':''}
          ${n.error?`<p class="error">${esc(n.error)}</p>`:''}
        </div></article>`;
    }).join('')||'<p class="empty">暂时没有节点，管理员添加后会在这里显示。</p>';
    if(focused)Array.from(document.querySelectorAll('[data-apply]')).find(el=>el.dataset.apply===focused)?.focus();
    controls();
  }
  function schedule(){clearTimeout(state.timer);if(state.token&&!document.hidden)state.timer=setTimeout(()=>refresh(),10000);}
  function failed(error,checkIdentity=true){
    if(error.status===401||checkIdentity&&error.status===403){clearSession(error.message);return;}
    $('statusError').textContent=error.message+(state.data?'。当前显示上次查询结果。':'');
  }
  async function refresh(){
    if(state.busy||!state.token)return;
    const epoch=state.epoch;state.busy=true;controls();
    try{
      const data=await request(endpoint);
      if(epoch!==state.epoch)return;
      state.data=data;stored(state.token);$('resourceToken').value='';$('statusError').textContent='';render();
    }catch(error){if(epoch===state.epoch)failed(error);}
    finally{if(epoch===state.epoch){state.busy=false;controls();schedule();}}
  }
  async function apply(nodeID){
    if(state.busy)return;
    const node=state.data?.nodes.find(n=>n.node_id===nodeID);
    if(!node||!node.online)return;
    const epoch=state.epoch;state.busy=true;state.applying=nodeID;clearTimeout(state.timer);
    $('statusError').textContent='';$('statusMessage').textContent='正在为 '+node.node_name+' 创建容器…';render();
    try{
      const result=await request(endpoint+'/containers',{method:'POST',body:JSON.stringify({node_id:nodeID})});
      if(epoch!==state.epoch)return;
      const allocation=result.nodes.find(n=>n.node_id===nodeID);
      if(!allocation||allocation.state!=='ready')throw Error('容器尚未就绪，请刷新查看申请结果');
      Object.assign(node,allocation);
      $('statusMessage').textContent=node.node_name+' 创建成功 · '+allocation.name;
    }catch(error){
      if(epoch!==state.epoch)return;
      $('statusMessage').textContent='';failed(error,false);
      if(state.data){node.state='failed';node.error=error.message;}
    }finally{
      if(epoch===state.epoch){state.busy=false;state.applying=null;if(state.data)render();else controls();schedule();}
    }
  }
  $('tokenForm').addEventListener('submit',event=>{event.preventDefault();if(state.busy)return;state.token=$('resourceToken').value.trim();refresh();});
  $('refreshStatus').addEventListener('click',()=>refresh());
  $('forgetToken').addEventListener('click',()=>{clearSession();$('resourceToken').focus();});
  $('statusNodes').addEventListener('click',event=>{const button=event.target.closest('[data-apply]');if(button&&!button.disabled)apply(button.dataset.apply);});
  document.addEventListener('visibilitychange',()=>{if(document.hidden)clearTimeout(state.timer);else refresh();});
  window.addEventListener('pagehide',()=>{state.epoch++;clearTimeout(state.timer);state.request?.abort();});
  window.addEventListener('pageshow',event=>{if(event.persisted){state.busy=false;state.applying=null;refresh();}});
  $('memberIdentity').textContent='使用者 ID · '+memberID;
  state.token=stored();if(state.token)refresh();
})();
