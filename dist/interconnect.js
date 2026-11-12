'use strict';
(()=>{
  const path='/api/cluster/interconnect';
  let view=null, editing='',editingRevision=null,busy=false,epoch=0;
  const key=e=>e.node+'/'+e.container;
  const ref=s=>{const [node,container]=s.split('/');return {node,container};};
  const node=id=>view.nodes.find(n=>n.id===id);
  const endpoint=r=>view.endpoints.find(e=>key(e)===key(r));
  const title=e=>e?[(node(e.node)?.name||e.node),e.name,e.owner||'未归属'].join(' / '):'端点不可用';
  const option=(v,label)=>new Option(label,v);
  function choices(id,items){
    const select=$(id),old=select.value;
    select.replaceChildren(...items.map(([v,label])=>option(v,label)));
    if(items.some(([v])=>v===old))select.value=old;
  }
  function item(parent,label,description){
    const div=document.createElement('article');div.className='link-item';
    const strong=document.createElement('strong'),p=document.createElement('p');
    strong.textContent=label;p.textContent=description;div.append(strong,p);$(parent).append(div);return div;
  }
  function button(div,label,run){
    const b=document.createElement('button');b.type='button';b.textContent=label;b.addEventListener('click',run);div.append(b);
  }
  function state(c){
    if(c.removing)return '待撤销';
    const sides=[...new Set([c.a.node,c.b.node])].map(node);
    if(sides.every(n=>n.applied>=c.revision&&!n.error&&!n.capability?.error))return '已应用';
    if(sides.some(n=>n.error||n.capability?.error))return sides.some(n=>n.applied>=c.revision&&!n.error&&!n.capability?.error)?'部分失败':'应用失败';
    return '待应用';
  }
  function rule(r={direction:'ab',protocol:'tcp',first:0,last:0,enabled:true}){
    const div=document.createElement('div');div.className='link-rule';
    div.innerHTML='<label>方向<select name="direction"><option value="ab">A → B</option><option value="ba">B → A</option><option value="both">双向</option></select></label><label>协议<select name="protocol"><option value="tcp">TCP</option><option value="udp">UDP</option><option value="icmp">ICMP</option><option value="all">全部 IP 协议</option></select></label><label>起始目的端口<input name="first" type="number" min="1" max="65535" placeholder="全部"></label><label>结束目的端口<input name="last" type="number" min="1" max="65535" placeholder="同起始"></label><label><input name="enabled" type="checkbox">启用规则</label>';
    const field=n=>div.querySelector('[name="'+n+'"]');
    for(const n of ['direction','protocol'])field(n).value=r[n];
    for(const n of ['first','last'])field(n).value=r[n]||'';
    field('enabled').checked=r.enabled;
    const ports=()=>{const disabled=!['tcp','udp'].includes(field('protocol').value);for(const n of ['first','last'])field(n).disabled=disabled;};
    field('protocol').addEventListener('change',ports);ports();button(div,'移除',()=>div.remove());$('linkRules').append(div);
  }
  function editorVersion(){
    $('linkEditorVersion').textContent=!editing?'':editingRevision===view.revision?'编辑基于配置版本 '+editingRevision:'配置已更新到 '+view.revision+'，当前编辑仍基于版本 '+editingRevision+'。请点击该连接的“编辑”重载后保存。';
  }
  function editor(c){
    editing=c?.id||'';editingRevision=c?view.revision:null;$('linkA').disabled=$('linkB').disabled=!!c;
    if(c){$('linkA').value=key(c.a);$('linkB').value=key(c.b);}
    else if($('linkA').value===$('linkB').value&&$('linkB').options.length>1)$('linkB').selectedIndex=1;
    $('linkRules').replaceChildren();if(c)c.rules.forEach(rule);else rule();
    $('linkEnabled').checked=c?.enabled??true;editorVersion();
  }
  function render(v){
    if(view?.revision!==v.revision)$('linkProbeResult').textContent='尚未测试';
    view=v;$('linkVersion').textContent='配置版本 '+v.revision;$('linkPool').value=v.pool;
    $('linkPoolForm').querySelector('button').disabled=v.endpoints.length>0;
    for(const id of ['linkNodes','linkEndpoints','linkConnections'])$(id).replaceChildren();
    for(const n of v.nodes){
      const s=n.capability||{};
      item('linkNodes',n.name,[n.internal_ip,s.interface||'待检查接口',n.subnet||'待分配 /24',s.protocol?'helper v'+s.protocol+' · '+s.firewall:'helper 未就绪','配置 '+n.desired+' / 已应用 '+n.applied,n.error,s.error].filter(Boolean).join(' · '));
    }
    const available=v.nodes.flatMap(n=>(n.candidates||[]).map(c=>({node:n.id,container:c.id,...c}))).filter(c=>!endpoint(c));
    choices('linkCandidate',available.map(e=>[key(e),title(e)]));
    for(const e of v.endpoints){
      const br='alb'+e.ip.split('.').map(x=>Number(x).toString(16).padStart(2,'0')).join('');
      const row=item('linkEndpoints',title(e),[e.ip,br,e.removing?'待撤销':''].filter(Boolean).join(' · '));
      if(!e.removing)button(row,'退出',()=>mutate({action:'detach',endpoint:ref(key(e))}));
    }
    const opts=v.endpoints.filter(e=>!e.removing).map(e=>[key(e),title(e)+' · '+e.ip]);
    choices('linkA',opts);choices('linkB',opts);
    const probes=[];
    for(const c of v.connections){
      const a=title(endpoint(c.a)),b=title(endpoint(c.b));
      const status=state(c),row=item('linkConnections',a+' ↔ '+b,[c.enabled?'已启用':'已禁用',status,'配置 '+c.revision,'A '+node(c.a.node).applied,'B '+node(c.b.node).applied].join(' · '));
      for(const rule of c.rules){
        const line=document.createElement('p'),direction={ab:'A → B',ba:'B → A',both:'A ↔ B'}[rule.direction];
        const ports=['tcp','udp'].includes(rule.protocol)?(rule.first?' · '+rule.first+(rule.last!==rule.first?'–'+rule.last:''):' · 全部端口'):'';
        line.textContent=direction+' · '+rule.protocol.toUpperCase()+ports+(rule.enabled?'':' · 已禁用');row.append(line);
      }
      if(!c.removing){button(row,'编辑',()=>editor(c));if(c.enabled)button(row,'禁用',()=>mutate({action:'disable',id:c.id}));button(row,'删除',()=>mutate({action:'delete',id:c.id}));}
      if(c.enabled&&!c.removing)probes.push([c.id+'/ab',a+' → '+b],[c.id+'/ba',b+' → '+a]);
    }
    choices('linkProbePair',probes);
    if(editing&&!v.connections.some(c=>c.id===editing&&!c.removing))editor(null);
    editorVersion();
  }
  async function mutate(payload){
    if(busy||!view)return;busy=true;const seq=epoch;
    $('linkMessage').textContent='正在应用配置…';
    try{
      const revision=payload.action==='save'&&editing?editingRevision:view.revision;
      const next=await api(path,{method:'POST',body:JSON.stringify({...payload,revision})});
      if(seq!==epoch||platform.user?.role!=='admin')return;render(next);const failed=next.nodes.filter(n=>n.error);
      $('linkMessage').textContent=failed.length?'配置已保存，等待重试。'+failed.map(n=>n.name+' · '+n.error).join(' / '):'操作已完成';
      if(payload.action==='save'){const c=next.connections.find(c=>c.revision===next.revision);if(c)editor(c);}
    }catch(e){$('linkMessage').textContent=e.message;}finally{busy=false;}
  }
  const submit=(id,run)=>$(id).addEventListener('submit',event=>{event.preventDefault();run();});
  submit('linkPoolForm',()=>mutate({action:'settings',pool:$('linkPool').value.trim()}));
  submit('linkAttachForm',()=>mutate({action:'attach',endpoint:ref($('linkCandidate').value)}));
  submit('linkEditor',()=>{
    const rules=[...$('linkRules').children].map(row=>{
      const get=n=>row.querySelector('[name="'+n+'"]'),port=n=>get(n).disabled?0:Number(get(n).value);
      return {direction:get('direction').value,protocol:get('protocol').value,first:port('first'),last:port('last')||port('first'),enabled:get('enabled').checked};
    });
    mutate({action:'save',connection:{id:editing,a:ref($('linkA').value),b:ref($('linkB').value),enabled:$('linkEnabled').checked,rules}});
  });
  submit('linkProbeForm',async()=>{
    const [id,dir]=$('linkProbePair').value.split('/'),c=view.connections.find(c=>c.id===id);
    try{await api(path,{method:'POST',body:JSON.stringify({action:'probe',probe:{from:dir==='ab'?c.a:c.b,to:dir==='ab'?c.b:c.a,port:Number($('linkProbePort').value)}})});$('linkProbeResult').textContent='TCP 连接成功';}
    catch(e){$('linkProbeResult').textContent='TCP 连接失败 · '+e.message;}
  });
  $('linkNew').onclick=()=>editor(null);$('linkAddRule').onclick=()=>rule();
  $('linkRetry').onclick=()=>mutate({action:'retry'});$('linkRefresh').onclick=()=>open();
  async function open(){
    const seq=++epoch;
    try{const v=await api(path);if(seq!==epoch||platform.page!=='interconnect')return;render(v);if(!$('linkRules').children.length)editor(null);$('linkMessage').textContent='';}
    catch(e){if(seq===epoch)$('linkMessage').textContent=e.message;}
  }
  function reset(){
    ++epoch;view=null;editing='';editingRevision=null;busy=false;
    for(const id of ['linkNodes','linkEndpoints','linkConnections','linkA','linkB','linkRules','linkCandidate','linkProbePair'])$(id).replaceChildren();
    $('linkMessage').textContent='';$('linkVersion').textContent='';$('linkEditorVersion').textContent='';$('linkProbeResult').textContent='尚未测试';
  }
  window.InterconnectUI={open,reset};
})();
