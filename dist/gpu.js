'use strict';
(()=>{
function create(root,{request,active}){
root.classList.add('gpu-view');
root.innerHTML=`<div class="gpu-topline"><span class="gpu-kicker"><i></i> 设备状态</span><button id="gpuRefresh">刷新 ↻</button></div>
      <div class="gpu-summary"><div><span>已检测 GPU</span><strong id="gpuCount">—</strong><small>NVIDIA · 物理设备</small></div><div><span>当前占用</span><strong id="gpuBusy">—</strong><small>有活动进程的 GPU</small></div><div><span>活动用户</span><strong id="gpuUsers">—</strong><small>按进程归属统计</small></div><div><span>滚动保留</span><strong>72 <em>h</em></strong><small>每 15 秒自动采集</small></div></div>
      <p id="gpuStatus" class="gpu-status" role="status"></p>
      <div id="gpuCards" class="gpu-cards"></div>
      <div id="gpuEmpty" class="gpu-empty" hidden><span aria-hidden="true">▤</span><h2>尚未检测到 GPU</h2><p>节点检测到 NVIDIA GPU 后，会自动展示设备和运行中的进程。</p></div>
      <section id="gpuProcesses" class="gpu-panel" hidden><header><div><span class="gpu-kicker">LIVE PROCESSES</span><h2 id="gpuProcessTitle">正在运行</h2></div><span id="gpuProcessCount" class="gpu-meta"></span></header><div class="gpu-table-wrap"><table><thead><tr><th>用户 / 容器</th><th>进程</th><th>PID</th><th>显存</th><th>进程已运行</th></tr></thead><tbody id="gpuProcessRows"></tbody></table></div><p class="gpu-footnote">运行时长从进程启动时间计算；GPU 占用时长按采样记录单独累计。未识别的归属会明确标注。</p></section>
      <section class="gpu-panel"><header><div><span class="gpu-kicker">UTILIZATION OVER TIME</span><h2>GPU 利用率历史</h2></div><span id="gpuResolution" class="gpu-meta"></span></header>
        <div class="gpu-chart-controls"><div id="gpuRanges" class="gpu-segments" aria-label="时间范围"><button data-gpu-hours="1">1h</button><button data-gpu-hours="6" aria-pressed="true">6h</button><button data-gpu-hours="24">24h</button><button data-gpu-hours="72">72h</button></div><div class="gpu-zoom"><button id="gpuZoomIn" aria-label="放大时间线">＋</button><button id="gpuZoomOut" aria-label="缩小时间线">−</button><span id="gpuWindow"></span></div><label class="gpu-step">采样口径 <select id="gpuStep"><option value="0">自动</option><option value="15">15 秒</option><option value="60">1 分钟</option><option value="300">5 分钟</option><option value="900">15 分钟</option><option value="3600">1 小时</option></select></label><button id="gpuLive" aria-pressed="true">● 跟随实时</button></div>
        <div class="gpu-pan"><span>72h 前</span><input id="gpuPan" type="range" min="0" max="66" step="0.25" value="66" aria-label="在最近 72 小时内平移时间窗口"><span>现在</span></div>
        <div id="gpuLegend" class="gpu-legend"></div><div id="gpuCharts"></div>
        <p class="gpu-footnote">曲线为整卡利用率，颜色表示该时段的进程归属；多人共享或同一采样区间内换人时用灰色表示，下方色带分别展示每位用户。缺测留空，零利用率仍可能有进程占用。</p>
      </section>
      <section class="gpu-panel"><header><div><span class="gpu-kicker">GPU HOURS / LAST 72H</span><h2>用户 GPU 占用时长</h2></div><span class="gpu-meta">最近 72h · 自动滚动</span></header><div id="gpuRanking" class="gpu-ranking"></div><p id="gpuCoverage" class="gpu-footnote"></p><p class="gpu-footnote">单位为 GPU·小时。同一用户在同卡上的多个进程去重，多卡相加；共享时各用户分别累计完整占用时间，因此合计可能超过设备总时长。此图始终统计最近 72 小时。</p></section>`;
const $=id=>root.querySelector('#'+id);
const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const state={data:null,error:'',selected:null,hours:6,step:0,end:null,timer:null,controller:null,epoch:0};
const palette=['#2563a6','#2f7a55','#9f3973','#5f4ba5','#9b6b15','#c35b23','#167d8d','#af3f43'];
function color(owner){let h=0;for(const c of owner)h=((h*31)+c.charCodeAt(0))>>>0;return palette[h%palette.length];}
const value=(n,d=0)=>n==null?'—':Number(n).toLocaleString('zh-CN',{maximumFractionDigits:d});
const timestamp=t=>new Date(t*1000).toLocaleString('zh-CN',{month:'2-digit',day:'2-digit',hour:'2-digit',minute:'2-digit',hour12:false});
function duration(s){if(s==null)return '未知';s=Math.max(0,s);if(s<60)return `${Math.floor(s)} 秒`;if(s<3600)return `${Math.floor(s/60)} 分钟`;return `${Math.floor(s/3600)} 小时 ${Math.floor(s%3600/60)} 分钟`;}
function ownerLabel(name){return name.startsWith('host:')?`宿主机 · ${name.slice(5)}`:name;}
function owners(d){return [...new Set(d.processes.map(p=>p.owner))].sort();}
const dot=name=>`<i style="background:${color(name)}"></i>`;
// Code-native flat illustration: fine strokes and the workspace's paper/olive palette.
function drawing(){return `<svg class="gpu-device" viewBox="0 0 180 102" fill="none" aria-hidden="true"><path d="M8 18v65m0-57H3m5 49H3" stroke="currentColor" stroke-width="2"/><rect x="14" y="22" width="155" height="61" rx="5" fill="var(--paper)" stroke="currentColor" stroke-width="1.4"/><path d="M22 30h139M22 76h139" stroke="currentColor" opacity=".35"/><rect x="116" y="36" width="39" height="30" rx="2" class="gpu-chip" stroke="currentColor"/><path d="M122 42h27m-27 6h27m-27 6h27m-27 6h27M35 83v8h57v-8m-48 0v8m8-8v8m8-8v8m8-8v8m8-8v8m8-8v8" stroke="currentColor" stroke-width="1.1"/><circle cx="62" cy="52" r="23" stroke="currentColor" stroke-width="1.4"/><circle cx="62" cy="52" r="7" class="gpu-chip" stroke="currentColor"/><g stroke="currentColor" stroke-width="1.1"><path d="M62 45c-9-9-15-5-16-2m23 9c9-9 5-15 2-16m-9 23c9 9 15 5 16 2M55 52c-9 9-5 15-2 16M67 47c0-13-7-14-10-13m10 23c13 0 14-7 13-10M57 57c0 13 7 14 10 13M57 47c-13 0-14 7-13 10"/></g><rect x="157" y="14" width="9" height="8" rx="1" stroke="currentColor"/><circle cx="159" cy="74" r="2" class="gpu-indicator" fill="currentColor"/><path d="M24 15h22m-22-4h10" stroke="currentColor" opacity=".4"/></svg>`;}
function replace(id,html){const el=$(id);if(el.innerHTML!==html)el.innerHTML=html;}
function controls(){
 root.querySelectorAll('[data-gpu-hours]').forEach(b=>b.setAttribute('aria-pressed',Number(b.dataset.gpuHours)===state.hours));
 $('gpuWindow').textContent=`${value(state.hours,2)}h`;$('gpuLive').setAttribute('aria-pressed',state.end===null);
 $('gpuZoomIn').disabled=state.hours<=.25;$('gpuZoomOut').disabled=state.hours>=72;
 const max=72-state.hours,now=state.data?.now||Date.now()/1000,offset=state.end===null?0:Math.max(0,(now-state.end)/3600);
 $('gpuPan').max=String(max);$('gpuPan').value=String(Math.max(0,max-offset));$('gpuPan').disabled=max<=0;
}
function render(){
 controls();const data=state.data,current=data?.current,devices=current?.devices||[],stale=!!(state.error||current?.error||current?.at&&data.now-current.at>45);
 $('gpuStatus').dataset.error=String(stale);
 $('gpuStatus').textContent=state.error||(current?.error?`${current.error}${current.at?' · 保留下方最近一次成功采集的数据':''}`:current?.at?`最近采集 ${timestamp(current.at)} · ${current.warning||'实时更新中，历史最多保留 72 小时'}`:'正在检测 GPU…');
 $('gpuCount').textContent=current?.at?String(devices.length):'—';$('gpuBusy').textContent=current?.at?String(devices.filter(d=>d.processes.length).length):'—';$('gpuUsers').textContent=current?.at?String(new Set(devices.flatMap(owners)).size):'—';
 $('gpuEmpty').hidden=devices.length>0||!current?.at||stale;
 if(!devices.some(d=>d.uuid===state.selected))state.selected=devices[0]?.uuid||null;
 const active=document.activeElement?.dataset?.gpuSelect;
 replace('gpuCards',devices.map(d=>{const busy=d.processes.length>0,names=owners(d);const mode=d.compute_mode==='Prohibited'?'计算已禁用':d.mig?'MIG 已启用':busy?'使用中':'未发现进程';return `<button class="gpu-card" data-gpu-select="${esc(d.uuid)}" data-busy="${busy}" aria-pressed="${state.selected===d.uuid}"><div class="gpu-card-heading"><span>GPU ${d.index.toString().padStart(2,'0')}</span><span class="gpu-state" data-busy="${busy}">${stale?'数据过期':mode}</span></div><div class="gpu-card-body">${drawing()}<div><h3>${esc(d.name)}</h3><small>${esc(d.uuid)}</small><small>${value(d.temperature)} °C${d.mig?' · MIG':''}</small></div></div><div class="gpu-card-metrics"><div><small>GPU 利用率</small><strong>${value(d.utilization)}<em>%</em></strong><div class="gpu-meter"><i style="width:${Math.min(100,Math.max(0,d.utilization||0))}%"></i></div></div><div><small>显存占用</small><strong>${value(d.memory_used_mib==null?null:d.memory_used_mib/1024,1)}<em>/ ${value(d.memory_total_mib==null?null:d.memory_total_mib/1024,1)} GiB</em></strong><div class="gpu-meter"><i style="width:${d.memory_total_mib?Math.min(100,100*(d.memory_used_mib||0)/d.memory_total_mib):0}%"></i></div></div></div><div class="gpu-card-bottom"><span>${esc(names.map(ownerLabel).join('、')||'无活动进程')}</span><span>${d.processes.length} 个进程 ↗</span></div></button>`;}).join(''));
 if(active)Array.from($('gpuCards').querySelectorAll('[data-gpu-select]')).find(b=>b.dataset.gpuSelect===active)?.focus({preventScroll:true});
 const selected=devices.find(d=>d.uuid===state.selected);$('gpuProcesses').hidden=!selected;
 if(selected){$('gpuProcessTitle').textContent=`GPU ${selected.index.toString().padStart(2,'0')} · 正在运行`;$('gpuProcessCount').textContent=`${selected.processes.length} 个进程${stale?' · 上次采集':''}`;replace('gpuProcessRows',selected.processes.map(p=>`<tr><td><span class="gpu-owner">${dot(p.owner)}${esc(ownerLabel(p.owner))}</span><small title="${esc(p.container_id)}">${esc(p.container|| (p.owner.startsWith('host:')?'宿主机':'归属待识别'))}</small></td><td>${esc(p.name)}<small>${esc(p.kind)}</small></td><td class="mono">${p.pid}</td><td>${value(p.memory_mib)} MiB</td><td>${duration(p.started_at==null?null:current.at-p.started_at)}</td></tr>`).join('')||'<tr><td colspan="5">当前没有观测到 GPU 进程</td></tr>');}
 renderHistory();
}
function renderHistory(){
 const h=state.data?.history;if(!h){replace('gpuCharts','<p class="gpu-no-data">等待历史数据…</p>');replace('gpuRanking','<p class="gpu-no-data">等待占用记录…</p>');return;}
 $('gpuResolution').textContent=`${duration(h.step)} / 点 · 每卡最多 864 点`;
 const legend=new Set(h.series.flatMap(s=>s.points.flatMap(p=>Object.keys(p.owners))));
 replace('gpuLegend',[...legend].sort().map(name=>`<span>${dot(name)}${esc(ownerLabel(name))}</span>`).join('')+'<span><i style="background:#505760"></i>共享 / 时段内多用户</span>');
 const current=state.data.current.devices||[],byID=new Map(h.series.map(s=>[s.uuid,s]));
 const list=[...current.map(d=>byID.get(d.uuid)||{uuid:d.uuid,name:d.name,points:[]}),...h.series.filter(s=>!current.some(d=>d.uuid===s.uuid))];
 replace('gpuCharts',list.map(s=>chart(s,h,current)).join('')||'<p class="gpu-no-data">还没有 GPU 历史记录。采集开始后自动积累，不补造过去的数据。</p>');
 const max=h.users[0]?.seconds||1;
 replace('gpuRanking',h.users.map(u=>`<div class="gpu-rank-row"><span class="gpu-owner">${dot(u.owner)}${esc(ownerLabel(u.owner))}</span><div class="gpu-rank-track" role="img" aria-label="${esc(ownerLabel(u.owner))}，${value(u.seconds/3600,2)} GPU 小时"><i style="width:${u.seconds/max*100}%;background:${color(u.owner)}"></i></div><strong>${value(u.seconds/3600,2)}<small>GPU·h</small></strong></div>`).join('')||'<p class="gpu-no-data">最近 72 小时尚无已记录的用户占用。</p>');
 $('gpuCoverage').textContent=h.since?`窗口：${timestamp(state.data.now-72*3600)} — ${timestamp(state.data.now)}；最早有效采样 ${timestamp(h.since)}。`:'服务开始采集后逐步积累，采集空档不计时。';
}
function chart(s,h,current){
 const device=current.find(d=>d.uuid===s.uuid),width=Math.max(280,$('gpuCharts').clientWidth-48),left=32,right=10,plot=width-left-right,span=h.to-h.from||1,x=t=>left+(t-h.from)/span*plot,y=v=>145-v*1.2;
 let svg=[0,50,100].map(v=>`<path class="gpu-grid" d="M${left} ${y(v)}H${width-right}"/><text x="0" y="${y(v)+4}">${v}%</text>`).join('');
 for(let i=0;i<=4;i++){const t=h.from+span*i/4;svg+=`<text x="${x(t)}" y="172" text-anchor="${i===0?'start':i===4?'end':'middle'}">${esc(new Date(t*1000).toLocaleTimeString('zh-CN',{hour:'2-digit',minute:'2-digit',hour12:false}))}</text>`;}
 s.points.forEach((p,i)=>{
 if(p.utilization==null)return;const names=Object.keys(p.owners),stroke=names.length===1?color(names[0]):'#505760',end=Math.min(h.to,p.at+h.step),next=s.points[i+1];
 const title=`${timestamp(p.at)} · ${value(p.utilization,1)}% · ${names.map(ownerLabel).join('、')||'未观测到用户进程'} · 有效采集 ${duration(p.observed_seconds)}`;
 const contiguous=next&&Math.abs(next.at-end)<1&&next.utilization!=null;
 svg+=`<path d="M${x(p.at)} 145V${y(p.utilization)}H${x(end)}V145Z" fill="${stroke}" opacity=".10"/><path class="gpu-line" d="M${x(p.at)} ${y(p.utilization)}H${x(end)}${contiguous?`V${y(next.utilization)}`:''}" stroke="${stroke}"><title>${esc(title)}</title></path>`;
 });
 if(!s.points.some(p=>p.utilization!=null))svg+=`<text x="${width/2}" y="85" text-anchor="middle">${s.points.length?'设备未提供利用率，占用记录见下方':'此时间段暂无采集数据'}</text>`;
 const names=[...new Set(s.points.flatMap(p=>Object.keys(p.owners)))].sort();
 const lanes=names.map(name=>`<div class="gpu-lane"><span title="${esc(ownerLabel(name))}">${esc(ownerLabel(name))}</span><svg viewBox="0 0 ${width} 7" preserveAspectRatio="none" role="img" aria-label="${esc(ownerLabel(name))}的占用时间带">${s.points.filter(p=>p.owners[name]>0).map(p=>`<rect x="${x(p.at)}" y="0" width="${Math.min(h.step,h.to-p.at)/span*plot}" height="7" fill="${color(name)}" opacity="${.6+.4*Math.min(1,p.owners[name]/h.step)}"><title>${esc(timestamp(p.at))} · ${esc(ownerLabel(name))} · 占用 ${duration(p.owners[name])}</title></rect>`).join('')}</svg></div>`).join('');
 return `<article class="gpu-chart"><div class="gpu-chart-title"><strong>${device?`GPU ${device.index.toString().padStart(2,'0')} · `:'历史设备 · '}${esc(s.name)}</strong><small>${timestamp(h.from)} — ${timestamp(h.to)}</small></div><svg class="gpu-plot" viewBox="0 0 ${width} 185" role="img" aria-label="${esc(s.name)}利用率，${esc(timestamp(h.from))}至${esc(timestamp(h.to))}" tabindex="0">${svg}</svg><div class="gpu-lanes">${lanes}</div></article>`;
}
function stop(){clearTimeout(state.timer);state.epoch++;state.controller?.abort();state.controller=null;$('gpuRefresh').disabled=false;}
async function load(){
 if(!active()||state.controller)return;
 clearTimeout(state.timer);const epoch=state.epoch,controller=new AbortController();state.controller=controller;$('gpuRefresh').disabled=true;
 const timeout=setTimeout(()=>controller.abort(),20000);
 const query=new URLSearchParams({hours:state.hours,step:state.step||Math.max(15,Math.ceil(state.hours*3600/360/15)*15)});
 if(state.end!==null){state.end=Math.max(state.end,Date.now()/1000-72*3600+state.hours*3600);query.set('end',state.end);}
 try{const data=await request('/api/gpu/overview?'+query,{signal:controller.signal});if(epoch!==state.epoch)return;if(!data.current||!Array.isArray(data.current.devices)||!data.history||!Array.isArray(data.history.series)||!Array.isArray(data.history.users))throw Error('GPU 监控返回了无效数据');state.data=data;state.error='';render();}
 catch(e){if(epoch===state.epoch){state.error=e.name==='AbortError'?'GPU 数据读取超时，稍后自动重试':e.message;render();}}
 finally{clearTimeout(timeout);if(epoch===state.epoch){state.controller=null;$('gpuRefresh').disabled=false;if(active())state.timer=setTimeout(load,15000);}}
}
function reload(){stop();controls();load();}
function zoom(hours){state.hours=Math.max(.25,Math.min(72,hours));if(state.hours===72)state.end=null;reload();}
$('gpuRefresh').addEventListener('click',load);
$('gpuCards').addEventListener('click',e=>{const b=e.target.closest('[data-gpu-select]');if(b){state.selected=b.dataset.gpuSelect;render();}});
$('gpuRanges').addEventListener('click',e=>{const b=e.target.closest('[data-gpu-hours]');if(b)zoom(Number(b.dataset.gpuHours));});
$('gpuZoomIn').addEventListener('click',()=>zoom(state.hours/2));$('gpuZoomOut').addEventListener('click',()=>zoom(state.hours*2));
$('gpuStep').addEventListener('change',e=>{state.step=Number(e.target.value);reload();});
$('gpuLive').addEventListener('click',()=>{state.end=null;reload();});
$('gpuPan').addEventListener('change',e=>{const offset=Number(e.target.max)-Number(e.target.value);state.end=offset<=0?null:(state.data?.now||Date.now()/1000)-offset*3600;reload();});
let resizeTimer;window.addEventListener('resize',()=>{clearTimeout(resizeTimer);if(active())resizeTimer=setTimeout(renderHistory,150);});
return {open(){render();load();},close:stop,reset(){stop();Object.assign(state,{data:null,error:'',selected:null,hours:6,step:0,end:null});$('gpuStep').value='0';replace('gpuCards','');replace('gpuProcessRows','');replace('gpuCharts','');replace('gpuRanking','');replace('gpuLegend','');$('gpuCoverage').textContent='';render();}};
}
window.GPUUI={create};
const root=document.getElementById('page-gpus');
if(root)Object.assign(window.GPUUI,create(root,{request:(...args)=>api(...args),active:()=>!!platform.user&&platform.page==='gpus'}));
})();
