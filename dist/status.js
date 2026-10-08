'use strict';
(()=>{
  const $=id=>document.getElementById(id);
  const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const username=decodeURIComponent(location.pathname.split('/')[2]);
  const endpoint='/api/status/'+encodeURIComponent(username);
  const storageKey='alpha.member-session.'+username;
  const state={token:'',data:null,busy:false,epoch:0,timer:null,request:null,keysDirty:false,keysSaving:false,gpuOpen:false,gpuNode:null};
  const labels={unallocated:'尚无容器',pending:'等待分配',running:'正在分配',ready:'已分配容器',failed:'分配失败',deleting:'正在回收',deleted:'尚无容器'};
  function stored(value){try{if(value===undefined)return sessionStorage.getItem(storageKey)||'';if(value)sessionStorage.setItem(storageKey,value);else sessionStorage.removeItem(storageKey);}catch{}return '';}
  function controls(){
    document.querySelectorAll('[data-choice]').forEach(input=>{input.disabled=state.busy||input.dataset.unavailable==='true';});
    $('loginSubmit').disabled=state.busy;$('keysSubmit').disabled=state.busy;$('memberKeys').disabled=state.keysSaving;
    $('refreshStatus').disabled=state.busy;$('passwordSubmit').disabled=state.busy;
    document.querySelectorAll('[data-apply]').forEach(button=>{button.disabled=state.busy||button.dataset.available!=='true';});
  }
  function clearSession(message=''){
    state.epoch++;state.request?.abort();state.request=null;
    state.gpuOpen=false;state.gpuNode=null;gpuUI.reset();$('gpuNode').replaceChildren();
    $('gpuPanel').hidden=true;$('toggleGPU').setAttribute('aria-expanded','false');$('toggleGPU').innerHTML='查看节点 GPU 使用情况 <span aria-hidden="true">→</span>';
    clearTimeout(state.timer);state.token='';state.data=null;state.busy=false;state.applying=null;stored('');
    state.keysDirty=false;state.keysSaving=false;$('keysForm').reset();$('keysError').textContent='';$('keysMessage').textContent='';$('keysSyncState').textContent='';
    $('loginPassword').value='';$('passwordForm').reset();$('passwordError').textContent='';$('loginPanel').hidden=false;$('statusContent').hidden=true;
    $('statusNodes').replaceChildren();$('sshConfig').textContent='';$('sshCommands').replaceChildren();$('controlStatusAddress').textContent='';$('memberIdentity').textContent='使用者 · '+username;
    $('statusError').textContent=message;$('statusMessage').textContent='';$('sshCopyStatus').textContent='';controls();
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
  const gpuUI=GPUUI.create($('statusGPUView'),{
    active:()=>!!state.token&&state.gpuOpen&&!!state.gpuNode&&!document.hidden,
    request:async(path,options)=>{
      const epoch=state.epoch;
      const query=new URLSearchParams(path.split('?')[1]);query.set('node_id',state.gpuNode);
      const response=await fetch(endpoint+'/gpu?'+query,{...options,cache:'no-store',credentials:'omit',headers:{Authorization:'Bearer '+state.token}});
      let data;try{data=await response.json();}catch{throw Error('服务返回的数据无效，请刷新重试');}
      if(!response.ok){if(epoch===state.epoch&&(response.status===401||response.status===403))clearSession(data.error);throw Error(data.error||'GPU 查询失败');}
      return data;
    }
  });
  function renderGPUNodes(){
    const previous=state.gpuNode,nodes=state.data.nodes;
    if(!nodes.some(n=>n.node_id===state.gpuNode))state.gpuNode=nodes.find(n=>n.online)?.node_id||nodes[0]?.node_id||null;
    $('gpuNode').innerHTML=nodes.map(n=>`<option value="${esc(n.node_id)}">${esc(n.node_name)}${n.online?'':'（离线）'}</option>`).join('')||'<option value="">暂无计算节点</option>';
    $('gpuNode').value=state.gpuNode||'';$('gpuNode').disabled=!nodes.length;
    if(previous!==state.gpuNode){gpuUI.reset();if(state.gpuOpen)gpuUI.open();}
  }
  function containers(node){
    const rows=[...node.containers];
    if(node.container_id&&!rows.some(c=>c.id===node.container_id))rows.unshift({id:node.container_id,name:node.name,state:'',managed:true});
    return rows;
  }
  function renderGuide(data){
    const control=data.control,access=data.access;
    $('controlStatusAddress').textContent=control.status_url?'我的总控入口 · '+control.status_url:'尚未分配 share node，请联系管理员。';
    $('sshAccessState').textContent=access.key_state==='ready'?'当前公钥已同步到跳板。':'跳板公钥尚未就绪，请联系管理员。'+(access.error||'');
    const config=access.share_host?['Host alpha-jump','  HostName '+access.share_host,'  User alpha-jump','  Port '+access.share_ssh_port,'  # 自定义私钥路径时，取消下面两行注释并修改路径','  # IdentityFile ~/.ssh/id_ed25519','  # IdentitiesOnly yes']:[];
    const connections=[];
    for(const node of data.nodes){
      if(!access.share_host||!node.container_id||!node.port||node.state!=='ready')continue;
      const alias='alpha-'+username+'-'+node.node_id.slice(0,8);
      config.push('','Host '+alias,'  HostName '+node.internal_ip,'  User root','  Port '+node.port,'  # 自定义私钥路径时，取消下面两行注释并修改路径','  # IdentityFile ~/.ssh/id_ed25519','  # IdentitiesOnly yes','  ProxyJump alpha-jump');
      connections.push({node,alias});
    }
    $('sshConfig').textContent=config.join('\n')+'\n';
    $('sshCommands').innerHTML=connections.map(({node,alias})=>`<p>${esc(node.node_name)} · <code>ssh ${esc(alias)}</code>${node.online?'':' <span class="muted">（节点离线，恢复后连接）</span>'}</p>`).join('');
    $('sshGuideNote').textContent=connections.length?'跳板地址为分配到的 share node，计算节点内网 IP 由管理员维护，容器端口来自你的分配记录。管理员另外分配的容器如未显示端口，请联系管理员获取连接信息。':'尚无容器时，在上方在线节点申请，成功后这里会自动补齐节点 IP、容器端口和连接命令。已有管理员分配的容器但未显示端口时，请联系管理员获取连接信息。';
  }
  function containerChoices(node){
    const candidates=node.candidates||[];
    const option=(value,title,detail,claimed=false)=>`<label class="container-option"><input type="radio" name="container-${esc(node.node_id)}" data-choice="${esc(node.node_id)}" value="${esc(value)}" data-unavailable="${claimed||!node.online}" ${state.busy||claimed||!node.online?'disabled':''}><span class="option-content"><span class="option-title">${esc(title)}</span><span class="option-detail">${esc(detail)}</span></span>${claimed?'<span class="claimed-badge">已领养</span>':''}</label>`;
    return `<fieldset class="container-picker"><legend>选择容器</legend>
      ${option('create','新建一个','使用默认环境，创建专属容器')}
      <p class="candidate-heading">领养已有容器 · ${candidates.filter(c=>!c.claimed).length} 个可选</p>
      <p class="choice-hint">保留原数据和密码</p>
      <div class="candidate-list">${candidates.map(c=>option(c.id,c.name,`${c.id.slice(0,12)}${(c.claimed_by||c.owner)?' · '+(c.claimed_by||c.owner):''}`,c.claimed)).join('')||`<p class="choice-empty">${node.candidates_error?'暂时无法读取已有容器':'暂无已有容器，可以选择新建'}</p>`}</div>
    </fieldset>`;
  }
  function render(){
    const data=state.data;
    $('memberIdentity').textContent=data.username+' · '+data.member_id;
    $('loginPanel').hidden=true;$('statusContent').hidden=false;
    $('totalNodes').textContent=data.nodes.length;
    $('onlineNodes').textContent=data.nodes.filter(n=>n.online).length;
    $('allocatedNodes').textContent=data.nodes.filter(n=>containers(n).length>0||n.state==='ready').length;
    $('checkedAt').textContent='更新于 '+new Date(data.checked_at*1000).toLocaleTimeString();
    renderGuide(data);renderGPUNodes();
    if(!state.keysDirty)$('memberKeys').value=data.ssh_public_key;
    const keyLabels={pending:'等待下发',failed:'下发失败',ready:'已同步'};
    $('keysSyncState').textContent=['跳板 · '+(keyLabels[data.access.key_state]||'等待下发')+(data.access.error?' · '+data.access.error:''),...data.nodes.filter(n=>n.key_state).map(n=>n.node_name+' · '+(keyLabels[n.key_state]||n.key_state)+(n.key_error?' · '+n.key_error:''))].join('\n');
    const active=document.activeElement;
    const focused={apply:active?.dataset?.apply,choice:active?.dataset?.choice,value:active?.value};
    const choices=new Map(Array.from(document.querySelectorAll('[data-choice]:checked')).map(el=>[el.dataset.choice,el.value]));
    $('statusNodes').innerHTML=data.nodes.map((n,index)=>{
      const own=containers(n),hasContainer=own.length>0||n.state==='ready';
      const canApply=n.state!=='ready'&&['unallocated','failed','deleted'].includes(n.state);
      const applying=n.node_id===state.applying;
      const allocation=applying?'正在分配':hasContainer&&!n.mode&&n.state==='unallocated'?'已有容器，待领养':hasContainer?'已分配容器':labels[n.state]||n.state;
      return `<article class="node" data-node="${esc(n.node_id)}">
        <div class="node-top"><span class="node-index">NODE / ${String(index+1).padStart(2,'0')}</span><span class="connection ${n.online?'':'offline'}">${n.online?'在线':'离线 / 不可用'}</span></div>
        <div><h3>${esc(n.node_name)}</h3><p class="address">内网 IP · ${esc(n.internal_ip)}</p></div>
        <dl class="details"><div><dt>主机</dt><dd>${esc(n.host||'未知')}</dd></div><div><dt>节点容器总数</dt><dd>${n.online?esc(n.container_count):'未知'}</dd></div><div><dt>扫描任务</dt><dd>${n.online?(n.scanning?'进行中':'空闲'):'未知'}</dd></div><div><dt>最近采集</dt><dd>${esc(n.observed_at||'尚无采集记录')}</dd></div></dl>
        ${n.connection_error?`<p class="node-note">${esc(n.connection_error)}</p>`:''}
        <div class="allocation"><div class="allocation-heading"><strong>${esc(allocation)}</strong></div>
          ${canApply&&!n.mode?containerChoices(n):''}
          ${canApply&&n.mode?`<p class="node-note">原选择：${n.mode==='adopt'?'领养 · '+esc(n.target_id):'新建一个'}；重试将继续原分配。</p>`:''}
          ${n.candidates_error?`<p class="node-note">已有容器列表暂不可用：${esc(n.candidates_error)}</p>`:''}
          ${own.length?`<ul class="container-list">${own.map(c=>`<li><code>${esc(c.name||c.id)}</code><small>${esc(c.state||'已登记 · 运行状态待采集')}</small></li>`).join('')}</ul>`:''}
          ${n.port?`<p class="node-note">SSH · root @ ${esc(n.internal_ip)} · 端口 ${esc(n.port)}</p>`:''}
          ${!hasContainer&&!n.online?'<p class="node-note">节点恢复在线后可申请。</p>':''}
          ${['pending','running'].includes(n.state)?'<p class="node-note">申请处理中，状态会自动更新。</p>':''}
          ${n.error?`<p class="error">${esc(n.error)}</p>`:''}
          ${canApply?`<button class="apply" data-apply="${esc(n.node_id)}" data-available="${n.online}" aria-label="${esc((n.state==='failed'?'重试':'申请')+' '+n.node_name+' 的容器')}" ${state.busy||!n.online?'disabled':''}>${applying?'正在分配…':!n.online?'节点离线':n.state==='failed'?'重试分配':'申请容器'}<span aria-hidden="true">${applying?'…':'→'}</span></button>`:''}
        </div></article>`;
    }).join('')||'<p class="empty">暂时没有节点，管理员添加后会在这里显示。</p>';
    for(const el of document.querySelectorAll('[data-choice]'))el.checked=el.value===choices.get(el.dataset.choice)&&el.dataset.unavailable!=='true';
    controls();
    if(focused.apply)Array.from(document.querySelectorAll('[data-apply]')).find(el=>el.dataset.apply===focused.apply)?.focus({preventScroll:true});
    if(focused.choice)Array.from(document.querySelectorAll('[data-choice]')).find(el=>el.dataset.choice===focused.choice&&el.value===focused.value)?.focus({preventScroll:true});
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
      state.data=data;stored(state.token);$('loginPassword').value='';$('statusError').textContent='';render();
    }catch(error){if(epoch===state.epoch)failed(error);}
    finally{if(epoch===state.epoch){state.busy=false;controls();schedule();}}
  }
  async function apply(nodeID){
    if(state.busy)return;
    const node=state.data?.nodes.find(n=>n.node_id===nodeID);
    if(!node||!node.online)return;
    const selected=Array.from(document.querySelectorAll('[data-choice]:checked')).find(el=>el.dataset.choice===nodeID)?.value;
    if(!node.mode&&!selected){$('statusError').textContent='请先选择领养已有容器或新建一个';return;}
    const mode=node.mode||(selected==='create'?'create':'adopt');
    const container_id=node.mode?node.target_id:(mode==='adopt'?selected:'');
    const epoch=state.epoch;state.busy=true;state.applying=nodeID;clearTimeout(state.timer);
    $('statusError').textContent='';$('statusMessage').textContent='正在为 '+node.node_name+' 分配容器…';render();
    try{
      const result=await request(endpoint+'/containers',{method:'POST',body:JSON.stringify({node_id:nodeID,mode,container_id})});
      if(epoch!==state.epoch)return;
      const allocation=result.nodes.find(n=>n.node_id===nodeID);
      if(!allocation||allocation.state!=='ready')throw Error('容器尚未就绪，请刷新查看申请结果');
      Object.assign(node,allocation);
      $('statusMessage').textContent=node.node_name+' 分配成功 · '+allocation.name;
    }catch(error){
      if(epoch!==state.epoch)return;
      $('statusMessage').textContent='';failed(error,false);
      if(state.data){node.state='failed';node.error=error.message;}
    }finally{
      if(epoch===state.epoch){state.busy=false;state.applying=null;if(state.data)render();else controls();schedule();}
    }
  }
  $('loginForm').addEventListener('submit',async event=>{
    event.preventDefault();if(state.busy)return;
    const epoch=state.epoch;state.busy=true;controls();
    try{
      const value=await request(endpoint+'/login',{method:'POST',body:JSON.stringify({password:$('loginPassword').value})});
      if(epoch!==state.epoch)return;
      state.token=value.session_token;stored(state.token);$('loginPassword').value='';
    }catch(error){if(epoch===state.epoch)failed(error);}
    finally{if(epoch===state.epoch){state.busy=false;controls();if(state.token)refresh();}}
  });
  $('memberKeys').addEventListener('input',()=>{state.keysDirty=true;$('keysMessage').textContent='';});
  $('keysForm').addEventListener('submit',async event=>{
    event.preventDefault();if(state.busy)return;
    const epoch=state.epoch;state.busy=true;state.keysSaving=true;clearTimeout(state.timer);controls();$('keysError').textContent='';$('keysMessage').textContent='';
    try{
      await request(endpoint+'/keys',{method:'POST',body:JSON.stringify({ssh_public_key:$('memberKeys').value})});
      if(epoch===state.epoch){state.keysDirty=false;$('keysMessage').textContent='公钥已保存，已提交自动下发。下方显示同步结果；失败后处理节点问题并再次保存即可重试。';}
    }catch(error){if(epoch===state.epoch){if(error.status===401||error.status===403)clearSession(error.message);else $('keysError').textContent=error.message;}}
    finally{if(epoch===state.epoch){state.busy=false;state.keysSaving=false;controls();refresh();}}
  });
  $('passwordForm').addEventListener('submit',async event=>{
    event.preventDefault();if(state.busy)return;
    if($('newMemberPassword').value!==$('confirmMemberPassword').value){$('passwordError').textContent='两次输入的密码不一致。';return;}
    const epoch=state.epoch;state.busy=true;clearTimeout(state.timer);controls();$('passwordError').textContent='';
    try{
      await request(endpoint+'/password',{method:'POST',body:JSON.stringify({current_password:$('currentPassword').value,password:$('newMemberPassword').value})});
      if(epoch===state.epoch){clearSession('密码已修改，请使用新密码登录。');$('loginPassword').focus();}
    }catch(error){if(epoch===state.epoch){if(error.status===401)clearSession(error.message);else $('passwordError').textContent=error.message;}}
    finally{if(epoch===state.epoch){state.busy=false;controls();schedule();}}
  });
  $('refreshStatus').addEventListener('click',()=>refresh());
  $('toggleGPU').addEventListener('click',()=>{
    state.gpuOpen=!state.gpuOpen;$('gpuPanel').hidden=!state.gpuOpen;$('toggleGPU').setAttribute('aria-expanded',String(state.gpuOpen));
    $('toggleGPU').innerHTML=state.gpuOpen?'收起 GPU 使用情况 <span aria-hidden="true">↑</span>':'查看节点 GPU 使用情况 <span aria-hidden="true">→</span>';
    if(state.gpuOpen){$('gpuTitle').focus({preventScroll:true});gpuUI.open();}else gpuUI.close();
  });
  $('gpuNode').addEventListener('change',()=>{state.gpuNode=$('gpuNode').value;gpuUI.reset();gpuUI.open();});
  $('copySSHConfig').addEventListener('click',async()=>{
    const epoch=state.epoch;
    try{await AlphaClipboard.writeText($('sshConfig').textContent);if(epoch===state.epoch)$('sshCopyStatus').textContent='SSH 配置已复制，请追加保存到本机 ~/.ssh/config；仅使用自定义私钥路径时需要修改注释项。';}
    catch{if(epoch===state.epoch)$('sshCopyStatus').textContent='请手动复制上方 SSH 配置。';}
  });
  $('logout').addEventListener('click',()=>{
    const token=state.token;clearSession();$('loginPassword').focus();
    fetch(endpoint+'/logout',{method:'POST',credentials:'omit',headers:{Authorization:'Bearer '+token}}).catch(()=>{});
  });
  $('statusNodes').addEventListener('click',event=>{const button=event.target.closest('[data-apply]');if(button&&!button.disabled)apply(button.dataset.apply);});
  document.addEventListener('visibilitychange',()=>{if(document.hidden){clearTimeout(state.timer);gpuUI.close();}else{refresh();if(state.gpuOpen)gpuUI.open();}});
  window.addEventListener('pagehide',()=>{state.epoch++;clearTimeout(state.timer);gpuUI.close();state.request?.abort();});
  window.addEventListener('pageshow',event=>{if(event.persisted){state.busy=false;state.applying=null;refresh();if(state.gpuOpen)gpuUI.open();}});
  $('memberIdentity').textContent='使用者 · '+username;
  state.token=stored();if(state.token)refresh();
})();
