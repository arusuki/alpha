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
    root.innerHTML='<div class="disk-toolbar"><p id="diskObserved" class="muted"></p><button id="diskRefresh" type="button">刷新磁盘用量</button></div><p id="diskStatus" class="error" role="status" aria-live="polite"></p><div id="diskFilesystems" class="disk-filesystems"></div><div id="diskTotals" class="disk-totals"></div><p class="muted disk-note">独占与共享按实际磁盘占用统计，共享空间不能跨容器重复相加。可写层包含在容器用量中。目录仅展示已采集的数据。</p><div id="diskContainers" class="disk-containers"></div>';
    const $=id=>root.querySelector('#'+id);
    let epoch=0,controllers=new Set(),loaded=false;
    const explorers=new WeakMap();
    const abort=()=>{epoch++;for(const c of controllers)c.abort();controllers.clear();$('diskRefresh').disabled=false;};
    async function fetchData(query){
      const controller=new AbortController();controllers.add(controller);
      try{return await request(query,{signal:controller.signal});}
      finally{controllers.delete(controller);}
    }
    function reset(){abort();loaded=false;for(const id of ['diskObserved','diskStatus','diskFilesystems','diskTotals','diskContainers'])$(id).replaceChildren();}
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
        $('diskFilesystems').innerHTML=data.filesystems.map(fs=>{
          const percent=fs.total>0&&fs.used!=null?Math.min(100,Math.max(0,fs.used/fs.total*100)):null;
          return `<article class="disk-filesystem"><strong>${esc(fs.mount)}</strong><span class="muted">${esc(fs.fs)}</span><p>已用 ${bytes(fs.used)} / 总计 ${bytes(fs.total)}</p><progress max="100" value="${percent??0}" aria-label="${esc(fs.mount)} 已用比例"></progress><small>${percent==null?'容量未知':percent.toFixed(1)+'% 已用'} · 可用 ${bytes(fs.available)}</small></article>`;
        }).join('')||'<p class="muted">暂无分区容量数据</p>';
        $('diskTotals').innerHTML=`<span>容器独占 <b>${bytes(data.exclusive)}</b></span><span>容器共享 <b>${bytes(data.shared)}</b></span><span>未关联容器 <b>${bytes(data.unrelated)}</b></span>`;
        $('diskContainers').innerHTML=data.containers.map(c=>{
          const title=`<span class="disk-container-name">${esc(c.name)} <small>${esc(c.owner||'未标注')}${c.expandable?' · 我的容器':''}</small></span><span class="disk-container-size">独占 ${c.known?bytes(c.exclusive):'未知'} · 共享 ${c.known?bytes(c.shared):'未知'}${c.partial?' · 部分统计':''}</span>`;
          return c.expandable?`<details class="disk-container" data-disk-container="${esc(c.id)}"><summary>${title}</summary><div class="disk-detail"></div></details>`:`<div class="disk-container disk-container-locked">${title}<small class="muted">仅本人可展开</small></div>`;
        }).join('')||'<p class="muted">本次扫描没有容器用量记录</p>';
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
    $('diskRefresh').addEventListener('click',refresh);
    return {reset,open:()=>{if(!loaded)refresh();},close:reset};
  }
  return {create};
})();
