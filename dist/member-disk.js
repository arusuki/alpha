'use strict';
const MemberDiskUI=(()=>{
  const esc=value=>String(value??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
  const bytes=value=>{
    if(value==null)return '未知';
    let n=Number(value),unit=0;
    while(n>=1024&&unit<5){n/=1024;unit++;}
    return n.toLocaleString('zh-CN',{maximumFractionDigits:unit?1:0})+' '+['B','KiB','MiB','GiB','TiB','PiB'][unit];
  };
  const size=row=>row.known?bytes(row.allocated)+(row.partial?'（部分统计）':''):'尚未统计';
  function create(root,{active,request}){
    root.innerHTML='<div class="disk-toolbar"><p id="diskObserved" class="muted"></p><button id="diskRefresh" type="button">刷新磁盘用量</button></div><p id="diskStatus" class="error" role="status" aria-live="polite"></p><div id="diskCapacitySummary" class="disk-capacity-summary"></div><div id="diskFilesystems" class="filesystem-grid"></div><section id="diskDistribution" class="disk-distribution" aria-label="节点空间归属分布"></section><div class="disk-containers-heading"><h3>我的容器</h3><span class="muted">点击展开目录用量</span></div><p class="muted disk-note">按已统计用量从大到小排列；实色为独占，斜纹为共享引用。共享空间不能跨容器重复相加，可写层已包含在内。</p><div id="diskContainers" class="disk-containers"></div>';
    const $=id=>root.querySelector('#'+id);
    let epoch=0,controllers=new Set(),loaded=false;
    const explorers=new WeakMap();
    const abort=()=>{epoch++;for(const c of controllers)c.abort();controllers.clear();$('diskRefresh').disabled=false;};
    async function fetchData(query){
      const controller=new AbortController();controllers.add(controller);
      try{return await request(query,{signal:controller.signal});}
      finally{controllers.delete(controller);}
    }
    function reset(){abort();loaded=false;for(const id of ['diskObserved','diskStatus','diskFilesystems','diskCapacitySummary','diskDistribution','diskContainers'])$(id).replaceChildren();}
    async function refresh(){
      if(!active())return;
      // A refresh replaces the overview and collapses all details, including any
      // data whose owner changed since the previous scan/view.
      reset();const version=epoch;$('diskRefresh').disabled=true;$('diskStatus').textContent='正在查询磁盘用量…';
      try{
        const data=await fetchData(new URLSearchParams());
        if(version!==epoch||!active())return;
        loaded=true;$('diskStatus').textContent='';
        $('diskObserved').textContent='扫描时间 · '+(data.observed_at||'未知')+(data.updated_at?' · 明细更新 '+data.updated_at:'');
        const disks=DiskCapacity.filesystems(data.filesystems);
        $('diskCapacitySummary').innerHTML=DiskCapacity.summary(disks,bytes);
        $('diskFilesystems').innerHTML=DiskCapacity.cards(disks,{esc,fmt:bytes});
        renderDistribution(data);
        const containers=data.containers.filter(c=>c.expandable).sort((a,b)=>Number(b.known)-Number(a.known)||(b.exclusive+b.shared)-(a.exclusive+a.shared));
        const max=containers.reduce((value,c)=>c.known?Math.max(value,c.exclusive+c.shared):value,1);
        $('diskContainers').innerHTML=containers.map(c=>{
          const title=`<span class="disk-container-heading"><span class="disk-container-name">${esc(c.name)}</span><strong>${c.known?bytes(c.exclusive+c.shared):'尚未统计'}</strong></span><span class="disk-container-track" aria-hidden="true"><i style="width:${c.known?c.exclusive/max*100:0}%"></i><i class="disk-shared-fill" style="width:${c.known?c.shared/max*100:0}%"></i></span><span class="disk-container-size">独占 ${c.known?bytes(c.exclusive):'未知'} · 共享 ${c.known?bytes(c.shared):'未知'}${c.partial?' · 部分统计':''}</span>`;
          return `<details class="disk-container" data-disk-container="${esc(c.id)}"><summary>${title}</summary><div class="disk-detail"></div></details>`;
        }).join('')||'<p class="muted">本次扫描中暂无属于你的容器用量记录</p>';
        root.querySelectorAll('[data-disk-container]').forEach(details=>{
          let generation=0;
          details.addEventListener('toggle',()=>{
            generation++;
            if(details.open){explorers.set(details,{sources:[],request:0});loadDetail(details,details.dataset.diskContainer,'',0,[],generation,()=>generation,true);}
            else details.querySelector('.disk-detail').replaceChildren();
          });
        });
      }catch(error){if(version===epoch&&error.name!=='AbortError')$('diskStatus').textContent=error.message;}
      finally{if(version===epoch)$('diskRefresh').disabled=false;}
    }
    function renderDistribution(data){
      const colors=['#4361d8','#168c8a','#b57932','#9670ca','#518c4a','#ca6386','#487fac','#9d745d'];
      const containerColors=new Map(data.containers.map(c=>c.id).sort().map((id,i)=>[id,colors[i%colors.length]]));
      const total=data.exclusive+data.shared+data.unrelated;
      const segments=[...data.containers].sort((a,b)=>b.exclusive-a.exclusive).map(c=>({
        name:`${c.owner||'未标注'} · ${c.name}${c.expandable?'（我）':''}`,
        bytes:c.exclusive,known:c.known,partial:c.partial,color:containerColors.get(c.id),
        id:c.expandable?c.id:null,container:true
      }));
      segments.push({name:'容器共享',bytes:data.shared,known:true,color:'#9472cd',className:'shared-fill'},
        {name:'Host · 未关联容器',bytes:data.unrelated,known:true,color:'#72849f'});
      const amount=s=>s.known||s.bytes>0?bytes(s.bytes):'尚未统计';
      const title=s=>`${s.name}${s.container?' · 独占':''}：${amount(s)}${s.partial?' · 部分统计':''}${s.id?' · 点击展开我的容器':''}`;
      const attributes=s=>`class="${s.className||''}" title="${esc(title(s))}" aria-label="${esc(title(s))}"`;
      const item=(s,content,style='')=>s.id
        ?`<button type="button" data-disk-open="${esc(s.id)}" ${attributes(s)} style="${style}">${content}</button>`
        :`<span ${attributes(s)} style="${style}">${content}</span>`;
      $('diskDistribution').innerHTML=`<div class="attribution-heading"><h3>已扫描空间 · 按用户 / 容器归属</h3><strong>${bytes(total)}</strong></div><div class="attribution-bar" aria-label="已扫描空间分布">${segments.filter(s=>s.bytes>0).map(s=>item(s,'',`width:${total>0?s.bytes/total*100:0}%;--swatch:${s.color}`)).join('')}</div><div class="storage-legend">${segments.filter(s=>s.container||s.bytes>0).map(s=>item({...s,className:''},`<i class="${s.className||''}" style="--swatch:${s.color}"></i><span>${esc(s.name)}</span><b>${amount(s)}${s.partial?' · 部分统计':''}</b>`)).join('')||'<span>尚无已统计的空间</span>'}</div><p class="storage-note">彩色容器段表示独占空间，共享数据在全节点只计一次；这里仅展示已扫描空间。点击自己的色块或图例展开目录明细。</p>`;
    }
    async function loadDetail(details,container,path,offset,trail,generation,currentGeneration,initial=false){
      if(!active())return;
      const version=epoch,body=details.querySelector('.disk-detail'),explorer=explorers.get(details),sequence=++explorer.request;
      body.textContent='正在读取容器用量…';
      const query=new URLSearchParams({container});if(path){query.set('path',path);query.set('offset',offset);}
      const valid=()=>version===epoch&&active()&&details.open&&details.isConnected&&generation===currentGeneration()&&sequence===explorer.request;
      const navigate=(nextPath,nextOffset,nextTrail)=>loadDetail(details,container,nextPath,nextOffset,nextTrail,generation,currentGeneration);
      try{
        const data=await fetchData(query);if(!valid())return;
        if(!path){
          explorer.sources=data.sources;
          const first=data.sources.find(source=>source.expandable);
          if(initial&&first)return navigate(first.path,0,[{path:'',offset:0}]);
        }
        const label=p=>explorer.sources.find(s=>s.path===p)?.label||p.split('/').filter(Boolean).at(-1)||'容器用量来源';
        const rows=path?data.entries:data.sources,steps=[...trail,{path,offset}];
        body.innerHTML=`<div class="disk-source-picker" aria-label="容器用量来源">${explorer.sources.map((source,i)=>`<button type="button" data-disk-root="${i}" aria-pressed="${path===source.path||trail.some(step=>step.path===source.path)}" ${source.expandable?'':'disabled'}>${esc(source.label)}<small>${size(source)}</small></button>`).join('')}</div><nav class="disk-breadcrumbs" aria-label="容量图目录路径">${path?'<button type="button" data-disk-back aria-label="返回上级目录">↑</button>':''}${steps.map((step,i)=>`<button type="button" data-disk-crumb="${i}" ${i===steps.length-1?'aria-current="location"':''}>${esc(label(step.path))}</button>`).join('<span aria-hidden="true">/</span>')}</nav><p class="disk-detail-path muted">${esc(path||'请选择一个用量来源，查看交互式容量图。来源可能共享或包含，不能直接相加。')}</p>${path?'<div class="disk-map" role="group" aria-label="目录容量图"></div><p class="muted disk-map-note">面积代表实际磁盘占用；点击目录下钻，点击文件查看用量。零占用和未知项见下方列表。</p><p class="disk-map-inspector" role="status" hidden></p>':''}<div class="disk-source-list"></div>${path?`<p class="muted">当前目录 ${size(data.node)}。${data.node.partial?'明细尚不完整，请等待管理员补充扫描。':''}</p>`:''}<div class="disk-pagination">${offset?'<button type="button" data-disk-prev>上一页</button>':''}${data.has_more?'<button type="button" data-disk-next>下一页</button>':''}</div>`;
        const openRow=i=>{
          const row=rows[i];
          if(row.expandable)navigate(row.path,0,[...trail,{path,offset}]);
          else if(path){const inspector=body.querySelector('.disk-map-inspector');inspector.hidden=false;inspector.textContent=(row.label||row.name)+' · '+size(row)+' · '+row.path;}
        };
        const list=body.querySelector('.disk-source-list');
        list.innerHTML=rows.map((r,i)=>`<div class="disk-source"><button type="button" data-disk-source="${i}">${esc(r.label||r.name)}${r.expandable?' →':''}</button><span>${size(r)}</span></div>`).join('')||'<p class="muted">暂无已采集的目录明细</p>';
        body.querySelectorAll('[data-disk-source]').forEach(button=>button.addEventListener('click',()=>openRow(Number(button.dataset.diskSource))));
        body.querySelectorAll('[data-disk-root]').forEach(button=>button.addEventListener('click',()=>navigate(explorer.sources[Number(button.dataset.diskRoot)].path,0,[{path:'',offset:0}])));
        body.querySelectorAll('[data-disk-crumb]').forEach(button=>button.addEventListener('click',()=>{const i=Number(button.dataset.diskCrumb);navigate(steps[i].path,steps[i].offset,steps.slice(0,i));}));
        body.querySelector('[data-disk-back]')?.addEventListener('click',()=>{const previous=trail.at(-1)||{path:'',offset:0};navigate(previous.path,previous.offset,trail.slice(0,-1));});
        body.querySelector('[data-disk-prev]')?.addEventListener('click',()=>navigate(path,Math.max(0,offset-50),trail));
        body.querySelector('[data-disk-next]')?.addEventListener('click',()=>navigate(path,offset+50,trail));
        if(path){
          const entries=rows.map((r,index)=>({name:r.name,bytes:r.known?r.allocated:0,index,expandable:r.expandable}));
          if(data.other_entries_allocated>0)entries.push({name:'其他页目录项',bytes:data.other_entries_allocated,index:-1,expandable:true});
          if(data.self_and_omitted_allocated>0)entries.push({name:'目录自身及未展开空间',bytes:data.self_and_omitted_allocated,index:-2});
          const map=body.querySelector('.disk-map'),height=map.clientHeight/map.clientWidth*100;
          const tiles=Usage.treemap(entries.filter(e=>e.bytes>0).sort((a,b)=>b.bytes-a.bytes),100,height);
          const colors=['#4f704f','#436b89','#84609a','#a06841','#487d78','#766a43'];
          map.innerHTML=tiles.map(tile=>{
            const title=tile.name+' · '+bytes(tile.bytes)+(tile.expandable?' · 点击下钻':' · 点击查看用量');
            return `<button type="button" class="disk-map-tile ${tile.index<0?'disk-map-remainder':''} ${tile.w<12||tile.h/height<.15?'disk-map-small':''}" data-disk-tile="${tile.index}" style="left:${tile.x}%;top:${tile.y/height*100}%;width:${tile.w}%;height:${tile.h/height*100}%;--disk-tile:${colors[Math.max(0,tile.index)%colors.length]}" title="${esc(title)}" aria-label="${esc(title)}"><span>${esc(tile.name)}</span><strong>${bytes(tile.bytes)}</strong><small>${tile.expandable?'点击下钻 →':'查看用量'}</small></button>`;
          }).join('')||'<p class="disk-map-empty">暂无已统计的非零占用；可从下方列表继续浏览目录。</p>';
          map.querySelectorAll('[data-disk-tile]').forEach(button=>button.addEventListener('click',()=>{
            const i=Number(button.dataset.diskTile);
            if(i===-1)navigate(path,data.has_more?offset+50:0,trail);
            else if(i===-2){const inspector=body.querySelector('.disk-map-inspector');inspector.hidden=false;inspector.textContent='目录自身及未展开空间 · '+bytes(data.self_and_omitted_allocated)+'。包含目录元数据或尚未采集的子项；需等待管理员补充扫描。';}
            else openRow(i);
          }));
        }
      }catch(error){
        if(!valid()||error.name==='AbortError')return;
        body.innerHTML=`<p class="error">${esc(error.message)}</p><button type="button">重试</button>`;
        body.querySelector('button').addEventListener('click',()=>navigate(path,offset,trail));
      }
    }
    root.addEventListener('click',event=>{
      const button=event.target.closest('[data-disk-open]');
      if(!button||!active())return;
      const details=[...root.querySelectorAll('[data-disk-container]')].find(d=>d.dataset.diskContainer===button.dataset.diskOpen);
      if(details){details.open=true;details.querySelector('summary').focus({preventScroll:true});details.scrollIntoView({block:'nearest'});}
    });
    $('diskRefresh').addEventListener('click',refresh);
    return {reset,open:()=>{if(!loaded)refresh();},close:reset};
  }
  return {create};
})();
