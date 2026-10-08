'use strict';
(()=>{
let epoch=0, timer=null, active=false, busy=false, settings=null, nodes=[], statusesPending=false;
const admin=()=>platform.user?.role==='admin';
const target=()=>$('mihomoTarget').value;
const path=(id,action)=>`/api/mihomo/${encodeURIComponent(id)}/${action}`;
const csv=value=>value.split(',').map(s=>s.trim()).filter(Boolean);
function fields(){
 $('mihomoEditor').disabled=busy||!settings;
 $('mihomoConfigFields').disabled=$('mihomoInherit').checked&&target()!=='control';
 $('mihomoTarget').disabled=busy;$('mihomoReload').disabled=busy;
 $('mihomoRefreshAll').disabled=busy;
 const node=nodes.find(n=>n.id===target());
 document.querySelectorAll('[data-mihomo-service]').forEach(b=>b.disabled=busy||!node?.online||(!node.service.digest&&b.dataset.mihomoService!=='stop'));
 $('mihomoRefresh').disabled=busy||!settings;
 document.querySelectorAll('[data-mihomo-group]').forEach(e=>e.disabled=busy||!node?.online);
}
function subscriptionRow(s={name:'',url:'',content:'',prefix:''}){
 const row=document.createElement('div');row.className='mihomo-edit-row';
 row.innerHTML=`<div class="form-grid three"><label>订阅名称<input data-sub="name" value="${esc(s.name)}" required maxlength="80"></label><label>节点名称前缀<input data-sub="prefix" value="${esc(s.prefix)}" maxlength="80" placeholder="可选，用于避免重名"></label><label>来源类型<select data-sub-kind><option value="url">订阅 URL</option><option value="content">内联内容</option></select></label></div><label data-source-url>订阅 URL<input data-sub="url" type="url" value="${esc(s.url)}" autocomplete="off" placeholder="https://…"></label><label data-source-content>订阅内容<textarea data-sub="content" rows="5" spellcheck="false">${esc(s.content)}</textarea></label><button type="button" data-remove-sub class="text-button">移除订阅</button>`;
 row.querySelector('[data-sub-kind]').value=s.content?'content':'url';
 const toggle=()=>{const inline=row.querySelector('[data-sub-kind]').value==='content';row.querySelector('[data-source-url]').hidden=inline;row.querySelector('[data-sub="url"]').disabled=inline;row.querySelector('[data-source-content]').hidden=!inline;row.querySelector('[data-sub="content"]').disabled=!inline;};
 row.querySelector('[data-sub-kind]').addEventListener('change',toggle);toggle();
 row.querySelector('[data-remove-sub]').addEventListener('click',()=>row.remove());
 $('mihomoSubscriptions').append(row);
}
function filterRow(f={name:'',include:'',exclude:'',types:[],sources:[]}){
 const row=document.createElement('div');row.className='mihomo-edit-row';
 row.innerHTML=`<div class="form-grid three"><label>规则名称<input data-filter="name" value="${esc(f.name)}" required maxlength="80" placeholder="例如 美国"></label><label>名称包含（正则）<input data-filter="include" value="${esc(f.include)}" placeholder="美国|US"></label><label>名称排除（正则）<input data-filter="exclude" value="${esc(f.exclude)}" placeholder="测试|过期"></label></div><div class="form-grid"><label>协议类型<input data-filter="types" value="${esc((f.types||[]).join(', '))}" placeholder="例如 ss, vmess, vless"></label><label>订阅来源名称<input data-filter="sources" value="${esc((f.sources||[]).join(', '))}" placeholder="多个名称以逗号分隔"></label></div><button type="button" data-remove-filter class="text-button">移除规则</button>`;
 row.querySelector('[data-remove-filter]').addEventListener('click',()=>row.remove());$('mihomoFilters').append(row);
}
function readConfig(){
 return {binary:$('mihomoBinary').value.trim(),mixed_port:Number($('mihomoPort').value),refresh_minutes:Number($('mihomoInterval').value),filter:$('mihomoGlobalFilter').value.trim(),template:$('mihomoTemplate').value,
 subscriptions:[...$('mihomoSubscriptions').children].map(row=>{const get=k=>row.querySelector(`[data-sub="${k}"]`).value;const inline=row.querySelector('[data-sub-kind]').value==='content';return {name:get('name').trim(),prefix:get('prefix'),url:inline?'':get('url').trim(),content:inline?get('content'):''};}),
 filters:[...$('mihomoFilters').children].map(row=>{const get=k=>row.querySelector(`[data-filter="${k}"]`).value;return {name:get('name').trim(),include:get('include'),exclude:get('exclude'),types:csv(get('types')),sources:csv(get('sources'))};})};
}
function fillConfig(data){
 settings=data;const c=data.config;
 $('mihomoBinary').value=c.binary;$('mihomoPort').value=c.mixed_port;$('mihomoInterval').value=c.refresh_minutes;
 $('mihomoGlobalFilter').value=c.filter;$('mihomoTemplate').value=c.template;
 $('mihomoSubscriptions').replaceChildren();(c.subscriptions||[]).forEach(subscriptionRow);
 $('mihomoFilters').replaceChildren();(c.filters||[]).forEach(filterRow);
 $('mihomoInheritLabel').hidden=target()==='control';$('mihomoInherit').checked=data.inherited;
 $('mihomoConfigTitle').textContent=target()==='control'?'总控默认配置':'此节点代理配置';
 $('mihomoConfigHint').textContent=target()==='control'?'保存后自动下发给继承配置的节点。各节点的启停状态与策略选择保持独立。':'继承时会跟随总控的订阅、过滤规则和模板更新。单独设置后，可勾选继承并保存以恢复总控配置。';
 fields();
}
function renderService(){
 const n=nodes.find(n=>n.id===target());
 if(!n){$('mihomoServiceStatus').textContent='正在读取服务状态…';$('mihomoGroups').replaceChildren();return;}
 const s=n.service;
 $('mihomoServiceStatus').textContent=`${n.role} · ${n.name} · ${!n.online?'节点离线':s.running?(s.ready?'代理运行中':'代理控制接口未就绪'):s.enabled?'代理启动失败或已退出':'代理已停止'}${s.pid?' · PID '+s.pid:''} · ${s.proxy_count||0} 个代理节点`;
 $('mihomoNodeError').textContent=[n.error,n.sync.error,s.error].filter(Boolean).join('；');
 // Do not replace an open selector while the administrator is choosing.
 if(!$('mihomoGroups').contains(document.activeElement)){
  $('mihomoGroups').innerHTML=(s.groups||[]).map((g,i)=>`<div class="mihomo-group"><div><strong>${esc(g.name)}</strong><span class="pill">${esc(g.type)}</span><p>${s.running?'当前':'已保存'}：${esc(g.now||'使用默认候选')}${g.resolved&&g.resolved!==g.now?' → '+esc(g.resolved):''}</p></div>${g.type==='select'?`<label>选择出口<select data-mihomo-group="${i}" aria-label="${esc(g.name)} 选择出口">${!g.now?'<option value="" disabled selected>选择候选节点</option>':''}${g.proxies.map(p=>`<option value="${esc(p)}" ${p===g.now?'selected':''}>${esc(p)}</option>`).join('')}</select></label>`:'<span class="form-note">由 mihomo 自动选择</span>'}</div>`).join('')||'<p class="form-note">尚无已应用的策略组。保存配置并完成下发后，会在这里显示模板中的策略组。</p>';
 }
 fields();
}
function renderNodes(){
 $('mihomoSummary').textContent=`${nodes.filter(n=>n.service.running).length} / ${nodes.length} 个节点正在运行代理 · ${nodes.filter(n=>n.pending).length} 个等待下发`;
 $('mihomoNodes').innerHTML=nodes.map(n=>`<tr><td><strong>${esc(n.name)}</strong><span class="sub">${esc(n.role)}</span></td><td>${n.id==='control'?'总控默认':n.inherited?'继承总控':'单独设置'}</td><td>${!n.online?'节点离线':n.service.running?(n.service.ready?'运行中':'控制接口异常'):n.service.enabled?'启动失败 / 已退出':'已停止'}</td><td>${n.sync.error?'更新失败':n.pending?'等待下发':n.service.digest?'已应用':'未配置'}${n.sync.refreshed_at?`<span class="sub">${esc(dateTime(n.sync.refreshed_at))}</span>`:''}</td><td><button data-mihomo-target="${esc(n.id)}" ${busy?'disabled':''}>管理</button></td></tr>`).join('');
 const ids=[...$('mihomoTarget').options].map(o=>o.value).join(',');
 if(ids!==nodes.map(n=>n.id).join(',')){
  const previous=target();$('mihomoTarget').innerHTML=nodes.map(n=>`<option value="${esc(n.id)}">${esc(n.role)} · ${esc(n.name)}</option>`).join('');
  if(nodes.some(n=>n.id===previous))$('mihomoTarget').value=previous;
  else if(settings){settings=null;loadSettings();}
 }
 renderService();
}
async function refreshStatus(){
 if(!active||!admin()||statusesPending)return;
 const seq=epoch;statusesPending=true;
 try{const data=await api('/api/mihomo/status');if(seq!==epoch||!active)return;nodes=data.nodes;renderNodes();}
 catch(e){if(seq===epoch)$('mihomoSummary').textContent='状态读取失败：'+e.message;}
 finally{if(seq===epoch){statusesPending=false;clearTimeout(timer);if(active)timer=setTimeout(refreshStatus,5000);}}
}
async function loadSettings(){
 if(!admin()||busy)return;const seq=++epoch;statusesPending=false;clearTimeout(timer);settings=null;fields();
 $('mihomoError').textContent='';$('mihomoMessage').textContent='正在读取配置…';$('mihomoPreviewPanel').hidden=true;$('mihomoPreviewYAML').value='';
 const id=target();renderService();
 try{const data=await api(path(id,'settings'));if(seq!==epoch)return;fillConfig(data);$('mihomoMessage').textContent='';}
 catch(e){if(seq===epoch){$('mihomoError').textContent=e.message;$('mihomoMessage').textContent='';}}
 finally{if(seq===epoch)refreshStatus();}
}
async function action(fn){
 if(busy||!admin()||!active)return;const seq=epoch,id=target();busy=true;fields();$('mihomoError').textContent='';
 try{await fn(id,()=>seq===epoch&&active);}
 catch(e){if(seq===epoch)$('mihomoError').textContent=e.message;}
 finally{if(seq===epoch){busy=false;fields();refreshStatus();}}
}
$('mihomoForm').addEventListener('submit',e=>{e.preventDefault();if(!settings)return;action(async(id,current)=>{
 const result=await api(path(id,'settings'),{method:'PUT',body:JSON.stringify({revision:settings.revision,base_revision:settings.base_revision,inherit:id!=='control'&&$('mihomoInherit').checked,config:readConfig()})});
 if(current()){fillConfig(result);$('mihomoMessage').textContent='配置已保存，正在后台生成并下发；请查看上方下发状态。';}
});});
$('mihomoPreview').addEventListener('click',()=>action(async(id,current)=>{
 const result=await api(path(id,'preview'),{method:'POST',body:JSON.stringify(readConfig())});
 if(current()){$('mihomoPreviewPanel').hidden=false;$('mihomoPreviewPanel').open=true;$('mihomoPreviewSummary').textContent=`${result.proxy_count} 个代理节点 · ${result.groups.length} 个策略组`;$('mihomoPreviewYAML').value=result.yaml;}
}));
$('mihomoRefreshAll').addEventListener('click',()=>action(async(_,current)=>{await api('/api/mihomo/refresh',{method:'POST',body:'{}'});if(current())$('mihomoMessage').textContent='已安排刷新全部订阅；失败的节点会显示错误并保留原配置。';}));
$('mihomoRefresh').addEventListener('click',()=>action(async(id,current)=>{await api(path(id,'refresh'),{method:'POST',body:'{}'});if(current())$('mihomoMessage').textContent='已安排刷新此节点订阅。';}));
document.querySelectorAll('[data-mihomo-service]').forEach(b=>b.addEventListener('click',()=>action(async(id,current)=>{
 await api(path(id,'service'),{method:'POST',body:JSON.stringify({action:b.dataset.mihomoService})});if(current())$('mihomoMessage').textContent='服务操作已完成。';
})));
$('mihomoGroups').addEventListener('change',e=>{const select=e.target.closest('[data-mihomo-group]');if(!select)return;
 const n=nodes.find(n=>n.id===target()),g=n?.service.groups[Number(select.dataset.mihomoGroup)];if(!g||!select.value)return;
 const name=select.value;select.blur();action(async(id,current)=>{await api(path(id,'selection'),{method:'PUT',body:JSON.stringify({group:g.name,name})});if(current())$('mihomoMessage').textContent=`已保存 ${g.name} → ${name}`;});
});
$('mihomoNodes').addEventListener('click',e=>{const b=e.target.closest('[data-mihomo-target]');if(b&&!busy){$('mihomoTarget').value=b.dataset.mihomoTarget;loadSettings();$('mihomoTarget').scrollIntoView({block:'center',behavior:'smooth'});}});
$('mihomoTarget').addEventListener('change',loadSettings);$('mihomoReload').addEventListener('click',loadSettings);
$('mihomoInherit').addEventListener('change',()=>{
 if(!$('mihomoInherit').checked){fields();return;}
 action(async(_,current)=>{const root=await api(path('control','settings'));if(current())fillConfig({...settings,inherited:true,base_revision:root.revision,config:root.config});});
});
$('mihomoAddSubscription').addEventListener('click',()=>subscriptionRow());$('mihomoAddFilter').addEventListener('click',()=>filterRow());
$('mihomoDefaultTemplate').addEventListener('click',()=>{if(settings)$('mihomoTemplate').value=settings.default_template;});
function leave(){active=false;epoch++;clearTimeout(timer);busy=false;statusesPending=false;}
window.MihomoUI={open(){if(!admin())return;active=true;loadSettings();},leave,reset(){leave();settings=null;nodes=[];$('mihomoSubscriptions').replaceChildren();$('mihomoFilters').replaceChildren();$('mihomoTemplate').value='';$('mihomoPreviewYAML').value='';$('mihomoPreviewPanel').hidden=true;$('mihomoGroups').replaceChildren();$('mihomoNodes').replaceChildren();$('mihomoError').textContent='';$('mihomoNodeError').textContent='';$('mihomoMessage').textContent='';fields();}};
})();
