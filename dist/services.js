'use strict';
(()=>{
const path='/api/containers/services';
const definitions={
 'tetragon':{name:'Tetragon',short:'进程观测',icon:'◎',description:'观测宿主机进程与容器事件，为进程监控提供数据。'},
 'dram-bw':{name:'DRAM 带宽',short:'内存带宽采集',icon:'▥',description:'采集宿主机内存带宽，配置采样节奏与硬件后端。'},
 'rootless-docker':{name:'Rootless Docker',short:'容器引擎与挂载',icon:'◇',description:'为指定使用者的容器提供共享 Docker 引擎。'}
};
const labels={running:'运行中',exited:'已停止',created:'尚未启动',missing:'未部署',unknown:'状态未知',restarting:'重启中',paused:'已暂停',dead:'异常',mounted:'已挂载',waiting:'等待容器启动',pending:'待应用'};
let epoch=0,busy=false,data=null,members=[],active='tetragon',drafts=new Map(),membersError='';
function reset(){epoch++;busy=false;data=null;members=[];active='tetragon';drafts.clear();membersError='';$('serviceCards').replaceChildren();$('serviceNav').replaceChildren();$('serviceMounts').replaceChildren();$('serviceMountPanel').hidden=true;$('servicesError').textContent='';$('servicesMessage').textContent='';$('servicesRefresh').disabled=false;}
function capture(){for(const form of $('serviceCards').querySelectorAll('form'))drafts.set(form.dataset.serviceConfig,new FormData(form));}
function select(name){
 active=name;
 for(const card of $('serviceCards').children)card.hidden=card.dataset.service!==active;
 for(const button of $('serviceNav').querySelectorAll('button'))button.setAttribute('aria-current',button.dataset.serviceSelect===active?'page':'false');
 $('serviceMountPanel').hidden=!data||active!=='rootless-docker';
}
function section(title,note,body){return `<fieldset class="service-section"><legend>${title}</legend><p class="form-note">${note}</p>${body}</fieldset>`;}
function rootlessOptions(c){
 const users=[...new Set([...members,...data.candidates.map(x=>x.owner),...c.users])].sort();
 return section('挂载位置','为使用者容器提供 Docker socket；默认位置可直接被 Docker CLI 使用。',`<div class="form-grid"><label>容器内 socket 路径<div class="service-path-input"><input name="socket_path" required value="${esc(c.socket_path)}" placeholder="/var/run/docker.sock"><button type="button" data-default-path="socket_path" title="恢复默认 socket 路径">恢复默认</button></div><small>默认 /var/run/docker.sock，包含文件名。修改后会撤销旧位置的挂载。</small></label><label>节点管理 socket 路径<div class="service-path-input"><input name="control_socket" required value="${esc(c.control_socket)}" placeholder="/run/rootless-docker/control.sock"><button type="button" data-default-path="control_socket" title="恢复默认管理路径">恢复默认</button></div><small>默认 /run/rootless-docker/control.sock，节点服务账号须有访问权限。</small></label></div>`)
 +section('使用者授权','选择此节点上可以使用 Docker 引擎的使用者。未创建或未启动的容器会在就绪后应用挂载。',`<div class="service-access-note">所选使用者共享同一个引擎，可访问其中全部容器、镜像和卷。</div>${membersError?`<p class="error-text">${esc(membersError)}；以下保留本节点容器和已选使用者。</p>`:''}<div class="service-users-heading"><span data-user-count></span><div><button type="button" data-select-users="all">全选</button><button type="button" data-select-users="none">清空选择</button></div></div><div class="service-users">${users.map(owner=>{const targets=data.candidates.filter(x=>x.owner===owner);return `<label class="service-user"><input type="checkbox" name="users" value="${esc(owner)}" ${c.users.includes(owner)?'checked':''}><span><strong>${esc(owner)}</strong><small>${targets.length?targets.map(x=>esc(x.name)+(x.initialized?'':' · 待初始化')).join('、'):'此节点暂无已接管容器'}</small></span></label>`;}).join('')||'<p class="form-note">暂无使用者，请先登记使用者或接管容器。</p>'}</div>`);
}
function dramOptions(c){
 const d=c.dram;
 return section('采集参数','保存后将重启正在运行的采集服务；循环重启时会先停止再应用。已停止的服务在下次启动时使用新配置。',`<div class="form-grid"><label>采样间隔（微秒）<input name="interval_us" type="number" min="100" max="10000000" step="1" required value="${d.interval_us}"><small data-frequency></small></label><label>采集后端<select name="backend">${[['amd-rome','AMD Rome · EPYC 7002'],['auto','自动检测硬件'],['mock','模拟数据 · 仅用于测试']].map(([v,l])=>`<option value="${v}" ${d.backend===v?'selected':''}>${l}</option>`).join('')}</select><small>默认 AMD Rome；不支持的硬件会明确报错。</small></label><label>参考峰值带宽（GB/s）<input name="peak_gbps" type="number" min="0" max="1000000" step="any" required value="${d.peak_gbps}"><small>默认 0：仅报告带宽。设置参考峰值后可计算利用率。</small></label></div><div class="service-access-note">默认间隔 100,000 μs（约 10 Hz）。AMD Rome 建议使用至少 100 ms 的间隔，较短间隔可能产生无效样本。数据表示宿主机总带宽。</div>`);
}
function updateHints(){
 const root=$('serviceCards').querySelector('[data-service="rootless-docker"]');
 if(root)root.querySelector('[data-user-count]').textContent=`已选择 ${root.querySelectorAll('input[name="users"]:checked').length} 位使用者`;
 const dram=$('serviceCards').querySelector('[data-service="dram-bw"]');
 if(dram){const interval=Number(dram.querySelector('[name="interval_us"]').value);dram.querySelector('[data-frequency]').textContent=interval>=100&&interval<=10000000?`约 ${(1000000/interval).toLocaleString('zh-CN',{maximumFractionDigits:2})} Hz · 每轮采集结束后的等待时间，实际频率受扫描耗时影响。`:'请输入 100–10,000,000 微秒。';}
}
function render(){
 if(!data)return;
 $('serviceNav').innerHTML=data.services.map(s=>{const d=definitions[s.name];return `<button type="button" data-service-select="${s.name}" aria-controls="service-${s.name}"><span class="service-nav-icon" aria-hidden="true">${d.icon}</span><span><strong>${d.name}</strong><small>${d.short}</small><span class="service-state" data-state="${esc(s.state)}">${esc(labels[s.state]||s.state)}</span></span></button>`;}).join('');
 $('serviceCards').innerHTML=data.services.map(s=>{
  const c=s.config,rootless=s.name==='rootless-docker',d=definitions[s.name];
  const recovering=s.name==='dram-bw'&&s.state==='restarting';
  const unavailable=['missing','unknown','paused','dead','removing'].includes(s.state)||s.state==='restarting'&&!recovering;
  const actions=[['start','启动'],['stop','停止'],['restart','重启'],...(rootless?[['apply','重试应用挂载']]:[])];
  return `<article id="service-${s.name}" class="management-panel service-card" data-service="${s.name}" aria-label="${d.name} 配置"><div class="service-hero"><div class="section-heading"><div><p class="eyebrow">${d.short}</p><h2>${d.name}</h2></div><strong class="service-state" data-state="${esc(s.state)}">${esc(labels[s.state]||s.state)}</strong></div><p>${d.description}</p><div class="service-runtime"><span>${esc(s.detail||'状态来自服务容器')}${s.checked_at?' · '+esc(dateTime(s.checked_at)):''}</span><div class="form-actions">${actions.map(([action,title])=>`<button type="button" data-service-action="${action}" data-service-name="${s.name}" ${unavailable||action==='apply'&&s.state!=='running'||action==='start'&&['running','restarting'].includes(s.state)||['stop','restart'].includes(action)&&s.state!=='running'&&!recovering?'disabled':''}>${title}</button>`).join('')}</div></div></div><form data-service-config="${s.name}" class="settings-form">${rootless?rootlessOptions(c):s.name==='dram-bw'?dramOptions(c):section('观测范围','Tetragon 在宿主机采集进程事件，由平台统一提供监控视图。','<div class="service-access-note">镜像、BPF 和进程缓存参数在节点的 Compose 部署中设置。此服务无需挂载到使用者容器。</div>')}${section('运行策略','服务启停使用已保存的配置。',`<div class="form-grid"><label>自动重启策略<select name="restart_policy">${[['unless-stopped','除手动停止外自动重启'],['always','总是自动重启'],['on-failure','异常退出时重启'],['no','不自动重启']].map(([v,l])=>`<option value="${v}" ${c.restart_policy===v?'selected':''}>${l}</option>`).join('')}</select></label><label>停止等待时间（秒）<input name="stop_timeout" type="number" min="1" max="180" step="1" required value="${c.stop_timeout}"><small>默认 ${rootless?'120':'10'} 秒，范围 1–180 秒。</small></label></div>`)}<div class="service-save-bar"><span>切换服务会保留未保存的修改。</span><button class="primary" type="submit">保存并应用配置</button></div></form></article>`;
 }).join('');
 for(const [key,draft] of drafts){const form=$('serviceCards').querySelector(`form[data-service-config="${key}"]`);if(!form)continue;for(const el of form.elements){if(!el.name)continue;if(el.type==='checkbox')el.checked=draft.getAll(el.name).includes(el.value);else if(draft.has(el.name))el.value=draft.get(el.name);}}
 $('serviceMountsHint').textContent=data.mount_error||'已核实当前挂载。停止服务会撤销受管理挂载，启动后按已保存配置恢复。';
 const mounts=data.mounts.map(m=>({...m,managed:true})).concat(data.external_mounts.map(m=>({...m,container_id:m.container,owner:'命令行关联',managed:false})));
 $('serviceMounts').innerHTML=mounts.map(m=>`<tr><td>${esc(m.owner)}</td><td>${esc(m.name)}<small class="mono" title="${esc(m.container_id)}">${esc(m.container_id.slice(0,12))}…</small></td><td class="mono">${esc(m.socket_path)}</td><td>${esc(labels[m.state]||'状态未知')}${m.error?`<p class="error-text">${esc(m.error)}</p>`:''}</td><td>${m.managed?'网页管理':'命令行管理'}</td></tr>`).join('')||'<tr><td colspan="5"><div class="service-empty">尚未授权挂载<small>选择使用者并保存后，在这里查看挂载状态。</small></div></td></tr>';
 $('servicesRefresh').disabled=busy;
 select(active);updateHints();
}
async function refresh(){
 const token=++epoch;$('servicesError').textContent='';$('servicesRefresh').disabled=true;
 try{const result=await api(path);if(token!==epoch)return;data=result;render();}
 catch(e){if(token!==epoch)return;$('servicesError').textContent=e.message;data=null;$('serviceCards').replaceChildren();$('serviceNav').replaceChildren();$('serviceMounts').replaceChildren();$('serviceMountPanel').hidden=true;}
 finally{if(token===epoch)$('servicesRefresh').disabled=busy;}
}
async function open(){
 reset();if(platform.user?.role!=='admin')return;
 const token=epoch;
 try{const result=await api('/api/members');if(token!==epoch)return;members=result.members.map(m=>m.username);}catch(e){if(token!==epoch)return;membersError='使用者列表读取失败：'+e.message;}
 await refresh();
}
async function mutate(name,action,config){
 if(busy)return;capture();busy=true;
 const token=epoch;
 $('servicesError').textContent='';$('servicesMessage').textContent='正在执行，服务停止及挂载恢复可能需要几分钟…';
 $('serviceCards').querySelectorAll('button,input,select').forEach(el=>el.disabled=true);$('servicesRefresh').disabled=true;
 let error='';
 try{await api(`${path}/${name}/${action}`,config?{method:'PUT',body:JSON.stringify(config)}:{method:'POST',body:'{}'});}catch(e){error=e.message;}
 if(token!==epoch)return;
 if(config&&!error)drafts.delete(name);
 busy=false;
 await refresh();
 if(token+1!==epoch)return;
 if(error)$('servicesError').textContent=error;
 $('servicesMessage').textContent=error?'操作未完全完成，请核对状态后重试。':data?'操作已完成。':'操作已完成，状态读取失败，请刷新核实。';
}
$('servicesRefresh').addEventListener('click',()=>{if(!busy){capture();refresh();}});
$('serviceNav').addEventListener('click',e=>{const button=e.target.closest('[data-service-select]');if(button)select(button.dataset.serviceSelect);});
$('serviceCards').addEventListener('input',updateHints);
$('serviceCards').addEventListener('click',e=>{
 const button=e.target.closest('button');if(!button||busy)return;
 if(button.dataset.serviceAction)mutate(button.dataset.serviceName,button.dataset.serviceAction);
 if(button.dataset.defaultPath){const name=button.dataset.defaultPath;button.form.elements[name].value=name==='socket_path'?'/var/run/docker.sock':'/run/rootless-docker/control.sock';}
 if(button.dataset.selectUsers){button.form.querySelectorAll('[name="users"]').forEach(el=>el.checked=button.dataset.selectUsers==='all');updateHints();}
});
$('serviceCards').addEventListener('submit',e=>{
 const form=e.target.closest('[data-service-config]');if(!form)return;e.preventDefault();
 const values=new FormData(form),name=form.dataset.serviceConfig;
 const config={restart_policy:values.get('restart_policy'),stop_timeout:Number(values.get('stop_timeout')),control_socket:values.get('control_socket')||'',socket_path:values.get('socket_path')||'',users:values.getAll('users')};
 if(name==='dram-bw')config.dram={interval_us:Number(values.get('interval_us')),backend:values.get('backend'),peak_gbps:Number(values.get('peak_gbps'))};
 mutate(name,'settings',config);
});
window.ServicesUI={open,reset};
})();
