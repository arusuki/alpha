'use strict';
const processView={epoch:0,timer:null,controller:null,data:null,error:'',selected:null,collapsed:new Set(),query:'',host:false};
const processHTML=new Map();
function processGroups() {
  const forest=processView.data?.forest;
  return forest?[...(forest.containers||[]),...(forest.host?[{...forest.host,id:'@host'}]:[])]:[];
}
function processGroupName(group) { return group.id==='@host'?'宿主机':group.id; }
// Flatten iteratively so unusually deep process ancestry cannot overflow the stack.
function processRows(group) {
  const rows=[],stack=(group.roots||[]).slice().reverse().map(node=>({node,depth:0,parent:-1}));
  while(stack.length){
    const row=stack.pop(),index=rows.length;rows.push(row);
    for(let i=(row.node.children||[]).length-1;i>=0;i--)stack.push({node:row.node.children[i],depth:row.depth+1,parent:index});
  }
  const query=processView.query.toLocaleLowerCase(),all=!query||processGroupName(group).toLocaleLowerCase().includes(query);
  const matches=new Set();
  rows.forEach((row,index)=>{if(all||`${row.node.pid} ${row.node.binary} ${row.node.command} ${row.node.cwd||''}`.toLocaleLowerCase().includes(query))matches.add(index);});
  // Search keeps ancestors visible to explain where a matching process belongs.
  for(let index=rows.length-1;index>=0;index--)if(matches.has(index)&&rows[index].parent>=0)matches.add(rows[index].parent);
  const hidden=new Set();
  return rows.filter((row,index)=>{
    if(!query&&row.parent>=0&&(hidden.has(row.parent)||processView.collapsed.has(rows[row.parent].node.exec_id)))hidden.add(index);
    return matches.has(index)&&!hidden.has(index);
  });
}
function updateProcessHTML(id,html) { if(processHTML.get(id)!==html){$(id).innerHTML=html;processHTML.set(id,html);} }
function renderProcesses() {
  const data=processView.data,status=data?.status,groups=processGroups();
  const connected=!!status?.connected,ready=connected&&status.bootstrapped;
  $('processConnection').textContent=processView.error?'连接不可用':ready?'监控已连接':connected?'正在同步进程':'正在重连';
  $('processConnection').dataset.state=processView.error?'error':ready?'connected':'pending';
  const alignment=status?.alignment;
  $('processStatusHint').textContent=processView.error||(status?.error?`连接提示：${status.error}`:!ready?'正在等待进程监控完成同步。':'实时观测容器进程，退出的进程会自动移出列表。');
  if(alignment&&ready)$('processStatusHint').textContent+=` 最近校准一致率 ${Number(alignment.aligned_percent).toLocaleString('zh-CN',{maximumFractionDigits:1})}%（估计值）${alignment.error?' · 校准异常：'+alignment.error:''}`;
  $('processContainerCount').textContent=data?String(data.forest.containers?.length||0):'—';
  $('processTotalCount').textContent=data?groups.reduce((sum,g)=>sum+g.process_count,0).toLocaleString('zh-CN'):'—';
  $('processCaptured').textContent=data?new Date(data.forest.captured_at).toLocaleTimeString('zh-CN'):'—';
  const filtered=groups.map(group=>({group,rows:processRows(group)})).filter(item=>item.rows.length);
  if(!filtered.some(item=>item.group.id===processView.selected))processView.selected=filtered[0]?.group.id||null;
  const selected=filtered.find(item=>item.group.id===processView.selected);
  $('processWorkspace').hidden=!selected;$('processEmpty').hidden=!!selected;
  $('processRetry').hidden=!!data&&!processView.error;
  $('processEmptyTitle').textContent=processView.error?'暂时无法读取进程':processView.query?'没有匹配的进程':!ready?'正在等待进程监控':'暂未观测到活动进程';
  $('processEmptyHint').textContent=processView.error?`${processView.error}。请检查主机上的 Tetragon 服务与连接配置，页面会自动重试。`:processView.query?'试试进程名称、PID、命令中的关键词或容器 ID。':!ready?'监控连接并完成同步后，进程关系会自动显示。':'新进程出现后会自动更新；也可以开启“包含宿主机”查看主机进程。';
  $('processGroupCount').textContent=String(filtered.length);
  updateProcessHTML('processContainers',filtered.map(({group})=>`<button data-process-group="${esc(group.id)}" aria-pressed="${group.id===processView.selected}"><span class="process-group-symbol" aria-hidden="true">${group.id==='@host'?'H':'C'}</span><span><strong>${esc(processGroupName(group))}</strong><small>${group.process_count} 个活动进程</small></span><span aria-hidden="true">›</span></button>`).join(''));
  if(!selected){updateProcessHTML('processTree','');return;}
  $('processTreeTitle').textContent=processGroupName(selected.group);
  $('processTreeHint').textContent=processView.error||!ready?'上次采集的数据 · 连接恢复后更新':processView.query?'搜索结果保留父进程，方便追踪关系。':'按父子关系排列 · 点击箭头收起分支';
  const limit=500,rows=selected.rows.slice(0,limit);
  updateProcessHTML('processTree',rows.map(({node,depth})=>`<tr><td><div class="process-command" style="--depth:${Math.min(depth,8)}"><button class="process-toggle" data-process-toggle="${esc(node.exec_id)}" aria-label="${processView.collapsed.has(node.exec_id)?'展开':'收起'} PID ${node.pid} 的子进程" aria-expanded="${!!processView.query||!processView.collapsed.has(node.exec_id)}" ${!node.children?.length||processView.query?'disabled':''}>${node.children?.length?(processView.collapsed.has(node.exec_id)&&!processView.query?'›':'⌄'):'·'}</button><span><strong title="${esc(node.binary)}">${esc(node.binary.split('/').pop()||node.binary)}</strong><code>${esc(node.command)}</code>${node.cwd?`<small>工作目录 ${esc(node.cwd)}</small>`:''}</span></div></td><td class="mono">${node.pid}</td><td class="mono">${node.uid}</td><td>${node.started_at?esc(new Date(node.started_at).toLocaleString('zh-CN')):'—'}</td></tr>`).join(''));
  $('processTreeFoot').textContent=`显示 ${rows.length} 个进程${selected.rows.length>limit?`（共 ${selected.rows.length} 个，请搜索缩小范围）`:''} · 进程数据来自实时观测，不保证覆盖所有系统进程。`;
}
function scheduleProcessPoll() {
  clearTimeout(processView.timer);
  if(platform.user&&platform.page==='processes')processView.timer=setTimeout(loadProcesses,3000);
}
async function loadProcesses() {
  if(!platform.user||platform.page!=='processes'||processView.controller)return;
  clearTimeout(processView.timer);
  const epoch=processView.epoch,controller=new AbortController();processView.controller=controller;
  const timeout=setTimeout(()=>controller.abort(),15000);
  $('processRefresh').disabled=true;$('processRetry').disabled=true;
  try{
    const result=await api('/api/process/forest'+(processView.host?'?host=1':''),{signal:controller.signal});
    if(epoch!==processView.epoch)return;
    if(!result.status||!result.forest||!Array.isArray(result.forest.containers))throw Error('进程监控返回了无效数据');
    processView.data=result;processView.error='';renderProcesses();
  }catch(error){if(epoch===processView.epoch){processView.error=error.name==='AbortError'?'读取进程超时，请重试':error.message;renderProcesses();}}
  finally{clearTimeout(timeout);if(epoch===processView.epoch){processView.controller=null;$('processRefresh').disabled=false;$('processRetry').disabled=false;scheduleProcessPoll();}}
}
function stopProcessPoll() {
  clearTimeout(processView.timer);processView.epoch++;processView.controller?.abort();processView.controller=null;
  $('processRefresh').disabled=false;$('processRetry').disabled=false;
}
window.ProcessUI={
  open(){renderProcesses();loadProcesses();},close:stopProcessPoll,
  reset(){stopProcessPoll();Object.assign(processView,{data:null,error:'',selected:null,collapsed:new Set(),query:'',host:false});$('processSearch').value='';$('processHost').checked=false;renderProcesses();}
};
$('processRefresh').addEventListener('click',loadProcesses);
$('processRetry').addEventListener('click',loadProcesses);
$('processSearch').addEventListener('input',event=>{processView.query=event.target.value.trim();renderProcesses();});
$('processHost').addEventListener('change',event=>{stopProcessPoll();processView.host=event.target.checked;processView.data=null;renderProcesses();loadProcesses();});
$('processExpand').addEventListener('click',()=>{processView.collapsed.clear();renderProcesses();});
$('processContainers').addEventListener('click',event=>{const button=event.target.closest('[data-process-group]');if(button){processView.selected=button.dataset.processGroup;renderProcesses();}});
$('processTree').addEventListener('click',event=>{const button=event.target.closest('[data-process-toggle]');if(button&&!button.disabled){const key=button.dataset.processToggle;if(processView.collapsed.has(key))processView.collapsed.delete(key);else processView.collapsed.add(key);renderProcesses();Array.from($('processTree').querySelectorAll('[data-process-toggle]')).find(el=>el.dataset.processToggle===key)?.focus({preventScroll:true});}});
