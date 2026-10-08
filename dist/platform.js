'use strict';
const platform = {nodeID:(window.location?.pathname||'').match(/^\/nodes\/([a-f0-9]{32})\/$/)?.[1]||'',user:null,csrf:'',setup:false,page:'dashboard',active:null,jobs:[],history:[],historyExhausted:false,latest:null,loaded:null,followLatest:true,config:null,poll:null,generation:0,loadSequence:0,starting:false,resultLoad:null,changesLoad:null,changesError:null,skippedAutoLoad:null,expandStarting:null,expandError:null,expandDepth:3,deletedIDs:new Set(),deleteTarget:null,deleting:false};
const statusNames = {queued:'等待启动',running:'扫描中',cancelling:'正在取消',cancelled:'已取消',completed:'已完成',failed:'失败',interrupted:'服务中断'};
const phaseNames = {discovering:'发现容器与数据卷',preparing:'准备扫描环境',host:'扫描 host',container:'扫描容器',directory:'扫描目录',scanning:'扫描存储',summarizing:'汇总结果',saving:'保存结果',completed:'已完成'};
const triggerNames = {scheduled:'定时',manual:'手动','agent-full':'Agent 全盘扫描',incremental:'目录扫描'};
const actionNames = {'bastion.key.sync':'补齐跳板用户公钥','bastion.key.clean':'清理 free 跳板公钥','bastion.account.init.start':'安装跳板开始','bastion.account.init.completed':'安装跳板完成','bastion.account.init.failed':'安装跳板失败','bastion.account.adopt.start':'接管跳板开始','bastion.account.adopt.completed':'接管跳板完成','bastion.account.adopt.failed':'接管跳板失败','bastion.account.release.start':'取消跳板接管开始','bastion.account.release.completed':'取消跳板接管完成','bastion.account.release.failed':'取消跳板接管失败','bastion.account.delete.start':'删除跳板账号开始','bastion.account.delete.completed':'删除跳板账号完成','bastion.account.delete.failed':'删除跳板账号失败','cluster.node.add':'添加节点','cluster.node.update':'修改节点连接','cluster.node.remove':'移除节点连接','member.schema':'修改使用者注册配置','member.register':'登记机器使用者','member.invitation.create':'生成注册邀请码','member.invitation.revoke':'作废注册邀请码','member.invitation.delete':'删除注册邀请码','container.create':'创建容器','container.adopt':'接管容器','container.start':'启动容器','container.stop':'停止容器','container.restart':'重启容器','container.delete':'删除容器','container.release':'解除容器接管','container.initialize':'初始化容器密码','container.settings':'修改容器配置','container.permissions.start':'开始修复节点权限','container.permissions.completed':'节点权限修复完成','container.permissions.failed':'节点权限修复失败','scan.expand':'补充扫描明细','scan.start':'启动扫描','scan.cancel':'取消扫描','scan.delete':'删除扫描记录','settings.update':'修改扫描配置','storage.cleanup.prepare':'读取报告清理条目','storage.cleanup.remove':'删除清理记录','storage.cleanup':'删除报告目录','agent.start':'启动 Agent 分析','agent.retry':'重试 Agent 失败请求','agent.settings':'修改模型配置','user.create':'创建账号','user.update':'修改账号权限','user.password':'修改登录密码','session.login':'登录','container.owner':'设置容器归属'};
const dateTime = value => value ? new Date(value*1000).toLocaleString('zh-CN') : '—';
function apiURL(path) {
  if(path==='/api/agent/settings')return path;
  return platform.nodeID && /^\/api\/(state|settings|jobs|owners|containers|process|gpu|agent|snapshot)([/?]|$)/.test(path) ? `/api/cluster/nodes/${platform.nodeID}${path}` : path;
}
async function api(path,options={}) {
  const response=await fetch(apiURL(path),{credentials:'same-origin',cache:'no-store',...options,headers:{'Content-Type':'application/json','X-CSRF-Token':platform.csrf,...options.headers}});
  const data=await response.json();
  if(!response.ok) {
    if(response.status===401 && platform.user) showAuth(false,'会话已过期，请重新登录。');
    throw Error(data.error || '请求失败，请稍后重试');
  }
  return data;
}
function showAuth(setup,error='') {
  stopSnapshotStream();
  if(platform.resultLoad)platform.resultLoad.controller.abort();
  if(platform.changesLoad)platform.changesLoad.controller.abort();
  window.SettingsUI.reset();
  window.UpdatesUI?.reset();
  window.MihomoUI?.reset();
  window.AgentUI?.reset();
  window.ProcessUI?.reset();
  window.GPUUI?.reset();
  window.ContainersUI?.reset();
  window.MembersUI?.reset();
  window.BastionUI?.reset();
  window.ClusterUI?.reset();
  window.CleanupUI?.reset();
  clearTimeout(platform.poll);platform.generation++;platform.user=null;platform.csrf='';platform.setup=setup;
  $('console').hidden=true;$('sessionControls').hidden=true;$('authPanel').hidden=false;
  for (const id of ['containerDialog','ownerDialog','passwordDialog','deleteJobDialog']) if ($(id).open) $(id).close();
  $('authTitle').textContent=setup?'初始化管理员':'欢迎回来。';
  $('authEyebrow').textContent=setup?'MAKE YOURSELF AT HOME':'YOUR WORKSPACE AWAITS';
  $('authHint').textContent=setup?'创建首个管理员账号，开启你的主机工作台。':'登录，回到你的主机工作台。';
  $('authSubmit').textContent=setup?'创建管理员并进入平台':'进入工作台';
  $('authPassword').minLength=setup?12:1;$('authPassword').autocomplete=setup?'new-password':'current-password';
  $('authPasswordHint').hidden=!setup;
  $('authError').textContent=error;
  window.AuthUI?.show({immediate:!!error});
}
async function enter(session,restorePage=false) {

  stopSnapshotStream();
  if(platform.resultLoad)platform.resultLoad.controller.abort();
  if(platform.changesLoad)platform.changesLoad.controller.abort();
  platform.changesError=null;
  platform.skippedAutoLoad=null;platform.expandStarting=null;platform.expandError=null;
  window.SettingsUI.reset();
  window.UpdatesUI?.reset();
  window.MihomoUI?.reset();
  window.AgentUI?.reset();
  window.ProcessUI?.reset();
  window.GPUUI?.reset();
  window.ContainersUI?.reset();
  window.MembersUI?.reset();
  window.BastionUI?.reset();
  window.ClusterUI?.reset();
  window.CleanupUI?.reset();
  platform.deletedIDs=new Set();platform.deleteTarget=null;platform.deleting=false;platform.generation++;platform.user=session.user;platform.csrf=session.csrf;platform.config=null;platform.history=[];platform.jobs=[];platform.active=null;platform.latest=null;platform.interval=0;platform.schedule=null;platform.historyExhausted=false;platform.loaded=null;platform.followLatest=true;
  window.AuthUI?.hide();
  $('authPanel').hidden=true;$('console').hidden=false;$('sessionControls').hidden=false;
  $('sessionUser').textContent=`${session.user.username} · ${session.user.role==='admin'?'管理员':'只读'}`;
  document.querySelectorAll('[data-admin]').forEach(e=>e.hidden=session.user.role!=='admin');
  snapshot=null;usage=null;selected=null;query='';ownerFilter=null;stateFilter='all';tablePage=0;$('resultContent').hidden=true;$('firstScan').hidden=false;
  $('sourceBadge').textContent='尚未扫描';
  $('hostInfo').textContent='尚未完成扫描 · 启用 Docker 自动发现后开始扫描';message('');
  $('firstScanHint').textContent=session.user.role==='admin'?'完成扫描后可独立分析 Host 容量；如需分析容器，请在扫描配置中启用 Docker 自动发现。':'管理员完成首次扫描后，这里会显示 Host 和容器的空间用量。';
  window.ClusterUI?.configure();
  const home=!platform.nodeID?'cluster':'dashboard';
  showPage(home,false);
  if(restorePage&&window.location?.hash)showPage(window.location.hash.slice(1),false);
  window.history?.replaceState(null,'',`#${platform.page}`);
  try{await syncState();}catch(error){if(platform.user){message(error.message);if(platform.nodeID)window.ClusterUI?.unavailable(error.message);}}finally{schedulePoll();}
}
function schedulePoll() {
  clearTimeout(platform.poll);
  if(platform.user) platform.poll=setTimeout(async()=>{try{await syncState();}catch(e){if(platform.user)message('状态更新失败：'+e.message);}finally{schedulePoll();}},!platform.nodeID?10000:platform.active?1000:2000);
}
function controls() {
  window.AgentUI?.controls();
  $('startScan').disabled=!!platform.active || platform.starting;
  $('startScan').textContent=platform.starting?'正在创建任务…':platform.active?'扫描进行中':'开始扫描';
  $('cancelScan').hidden=!platform.active || platform.user.role!=='admin';
  $('cancelScan').disabled=platform.active && platform.active.status==='cancelling';
  $('latestResult').hidden=platform.followLatest && !platform.skippedAutoLoad || !platform.latest;
  refreshDirectoryScan();
}
async function syncState() {
  if(!platform.nodeID){if(["cluster","allocations"].includes(platform.page))await window.ClusterUI?.refresh();return;}
  const generation=platform.generation;
  const state=await api('/api/state');if(generation!==platform.generation)return;
  if(platform.nodeID)window.ClusterUI?.connected();
  state.jobs=state.jobs.filter(j=>!platform.deletedIDs.has(j.id));
  if(platform.deletedIDs.has(state.latest_id))state.latest_id=null;
  platform.jobs=[...state.directory_jobs.filter(j=>!platform.deletedIDs.has(j.id)),...state.jobs];platform.latest=state.latest_id;platform.active=state.active;
  const previouslyLoaded=platform.history.find(j=>j.id===platform.loaded);
  const history=new Map(platform.history.filter(j=>j.trigger!=='incremental').map(j=>[j.id,j]));state.jobs.filter(j=>j.trigger!=='incremental').forEach(j=>history.set(j.id,j));
  platform.history=Array.from(history.values()).sort((a,b)=>b.created_at-a.created_at);
  if(state.jobs.length<50)platform.historyExhausted=true;
  platform.interval=state.interval_minutes;platform.schedule=state;
  if(state.jobs.length<50)platform.history=state.jobs.slice();
  else if(state.history_floor)platform.history=platform.history.filter(j=>j.created_at>state.history_floor.created_at || j.created_at===state.history_floor.created_at && j.id>=state.history_floor.id);
  if(previouslyLoaded && !platform.history.some(j=>j.id===previouslyLoaded.id)){
    platform.deletedIDs.add(previouslyLoaded.id);clearLoadedRecord();
    message('当前扫描记录已被清理。');
  }
  renderTask(state.interval_minutes);renderHistory();controls();window.DashboardUI?.render();
  // Loading large snapshots is an explicit storage action, never a login side effect.
  if(platform.page!=='overview')return;
  const loadedJob=platform.history.find(j=>j.id===platform.loaded);
  if(snapshot && platform.loaded && loadedJob && loadedJob.snapshot_revision > snapshot.revision && !platform.resultLoad && !platform.changesLoad && platform.skippedAutoLoad!==platform.loaded) {
    const updated=await loadSnapshotChanges();
    if(updated) message('目录明细与空间用量已更新。');
    return updated;
  }
  if(platform.followLatest && state.latest_id && platform.loaded!==state.latest_id && !platform.resultLoad && !platform.changesLoad && platform.skippedAutoLoad!==state.latest_id) return await loadJob(state.latest_id);
}
function scanScheduleLabel(interval) {
  const s=platform.schedule;
  if(s?.schedule_mode==='calendar')return `时间计划 · ${s.schedule_times.join('、')} · ${s.schedule_timezone}`;
  if(s?.schedule_mode==='off')return '手动扫描';
  return interval?`定时扫描 · ${interval} 分钟`:'手动扫描';
}
function renderTask(interval) {
  if (!platform.active && snapshot) { renderScanSummary(interval); return; }
  const job=platform.active || platform.jobs.find(j=>j.trigger!=='incremental');
  $('scheduleStatus').textContent=scanScheduleLabel(interval);
  $('taskMonitor').hidden=!job;
  if(!job)return;
  const p=job.progress || {};
  const running=job.status==='running', waiting=job.status==='queued';
  $('taskMonitor').classList.toggle('busy',running || waiting);
  $('taskMonitor').dataset.status=job.status;
  $('taskStatus').textContent=job.trigger==='incremental'?'扫描目录':triggerNames[job.trigger] || '扫描任务';
  $('taskPhase').textContent=running?(phaseNames[p.phase] || '准备扫描'):(statusNames[job.status] || job.status);
  const current=Array.isArray(p.current_containers)?p.current_containers:[];
  const names=current.map(c=>c.name || c.id || '未命名容器');
  const targets={discovering:p.path || '正在发现 Docker 容器与挂载',preparing:p.path || '正在准备扫描环境',host:'正在扫描 host · 宿主机文件',container:names.length>1?`正在扫描共享存储 · ${names.slice(0,3).join('、')}${names.length>3?` 等 ${names.length} 个容器`:''}`:`正在扫描容器 · ${names[0] || '容器存储'}`,directory:'正在扫描目录',scanning:'正在遍历目录与统计空间',summarizing:'正在汇总空间用量与容器归属',saving:'扫描结束，正在保存结果',completed:'扫描完成'};
  $('taskTarget').textContent=running?(targets[p.phase] || '正在启动扫描进程'):waiting?'等待扫描进程启动':job.status==='cancelling'?'正在停止扫描':job.status==='completed'?'扫描完成':`扫描已停止 · ${statusNames[job.status] || job.status}`;
  $('taskTarget').title=names.join('、');
  const total=Number.isInteger(p.containers_total)&&p.containers_total>=0?p.containers_total:null;
  if(total===null)$('taskContainers').textContent=running || waiting?'容器数量待发现':'本次任务未提供容器进度';
  else if(total===0)$('taskContainers').textContent='本次无容器扫描任务';
  else if(p.phase==='discovering')$('taskContainers').textContent=`已发现 ${p.containers_discovered || 0} / ${total} 个 · ${total} 个容器待扫描`;
  else {
    const done=Math.max(0,Math.min(total,Number(p.containers_done)||0));
    $('taskContainers').textContent=`容器已处理 ${done} / ${total} · 剩余 ${total-done} 个`;
  }
  const directoryRecord=job.trigger==='incremental' && snapshot && job.config && job.config.base_job_id===snapshot.job_id;
  const recordDisks=directoryRecord?diskFilesystems(snapshot.filesystems):[];
  const recordCapacity=directoryRecord?recordDisks.reduce((sum,d)=>sum+d.total,0):null;
  const capacityDisks=directoryRecord?recordDisks:diskFilesystems(p.capacity_filesystems || []);
  const scanned=directoryRecord?snapshot.tree.allocated:Number.isFinite(p.allocated)?Math.max(0,p.allocated):Math.max(0,job.allocated || 0);
  const capacity=recordCapacity || (Number.isFinite(p.capacity_total)&&p.capacity_total>0&&p.capacity_known!==false?p.capacity_total:null);
  const preparing=p.phase==='discovering' || p.phase==='preparing' || waiting || !p.phase && running;
  const preparationTotal=Number.isFinite(p.preparation_total)&&p.preparation_total>0?p.preparation_total:null;
  const preparationDone=Math.max(0,Number(p.preparation_done)||0);
  const percent=preparing?(preparationTotal===null?null:Math.min(preparationTotal,preparationDone)/preparationTotal*100):capacity===null?null:scanned/capacity*100;
  const ratio=percent===null?'':percent>0&&percent<0.1?'< 0.1%':`${percent.toLocaleString('zh-CN',{maximumFractionDigits:1})}%`;
  $('taskCountLabel').textContent=preparing?'准备已完成':'累计已扫描';
  $('taskScanned').textContent=preparing?`${preparationDone.toLocaleString('zh-CN')} 项`:fmt(scanned);
  $('taskCapacity').textContent=preparing?(preparationTotal===null?' / 正在获取总数':` / 共 ${preparationTotal.toLocaleString('zh-CN')} 项`):capacity===null?' / 文件系统总容量待获取':` / 总容量合计 ${fmt(capacity)}`;
  $('taskPercent').textContent=preparing?(percent===null?'准备中':`本阶段 ${ratio}`):capacity===null?'容量未知':`占总容量 ${ratio}`;
  const meter=$('taskMeter'), fill=$('taskFill');
  meter.classList.toggle('indeterminate',percent===null && (running || waiting));
  // Keep the fill element mounted across polls so CSS can interpolate updates.
  // Preparation and byte coverage use different units; reset at that boundary.
  const meterStage=preparing?(p.phase || 'queued'):'bytes';
  if(meter.dataset.job!==job.id || meter.dataset.stage!==meterStage){fill.style.transition='none';fill.style.width='0%';void fill.offsetWidth;fill.style.transition='';meter.dataset.job=job.id;meter.dataset.stage=meterStage;}
  fill.style.width=`${percent===null?0:Math.min(100,percent)}%`;
  $('taskUsedFill').style.width=`${preparing || capacity===null?0:Math.min(100,Math.max(0,Number(p.capacity_used)||0)/capacity*100)}%`;
  if(percent===null)meter.removeAttribute('aria-valuenow');
  else meter.setAttribute('aria-valuenow',String(Math.min(100,percent)));
  meter.setAttribute('aria-label',preparing?'当前准备阶段完成进度':'已扫描空间占文件系统总容量');
  meter.setAttribute('aria-valuetext',preparing?`准备已完成 ${preparationDone} 项${preparationTotal===null?'，总数待获取':`，共 ${preparationTotal} 项，本阶段 ${ratio}`}，${$('taskPhase').textContent}`:`已扫描 ${fmt(scanned)}${capacity===null?'，文件系统总容量未知':`，文件系统总容量 ${fmt(capacity)}，占 ${ratio}`}，${$('taskPhase').textContent}`);
  $('taskCapacityNote').textContent=preparing?'按已完成的准备事项计数，不代表耗时比例':capacity===null?'正在获取扫描范围内的文件系统容量':filesystemCapacityNote(capacityDisks);
  if(!preparing && capacity===null && !running && !waiting)$('taskCapacityNote').textContent='本次任务未提供整盘容量';
  const elapsed=Math.max(0,Math.round((job.finished_at || Date.now()/1000)-(job.started_at || job.created_at)));
  const duration=elapsed<60?`${elapsed} 秒`:`${Math.floor(elapsed/60)} 分 ${elapsed%60} 秒`;
  $('taskProgress').textContent=preparing?`尚未开始文件遍历 · 已用 ${duration}`:`${(p.entries||0).toLocaleString('zh-CN')} 项已遍历 · ${duration}${p.errors?` · ${p.errors} 处读取异常`:''}`;
  $('taskPath').textContent=job.error || p.path || (job.finished_at ? `结束于 ${dateTime(job.finished_at)}` : '正在启动扫描进程');
  $('taskPath').title=$('taskPath').textContent;
}
// The idle monitor belongs to the displayed record, regardless of which
// background operation happened most recently.
function renderScanSummary(interval) {
  const filesystems=diskFilesystems(snapshot.filesystems || []);
  const total=filesystems.reduce((sum,d)=>sum+d.total,0);
  const dockerDisk=dockerFilesystem(snapshot);
  const capacityNote=filesystemCapacityNote(filesystems);
  const used=filesystems.reduce((sum,d)=>sum+d.used,0);
  const scanned=snapshot.tree.allocated;
  $('scheduleStatus').textContent=scanScheduleLabel(interval);
  $('taskMonitor').hidden=false;$('taskMonitor').classList.toggle('busy',false);$('taskMonitor').dataset.status='completed';
  $('taskStatus').textContent='累计扫描结果';$('taskPhase').textContent='已保存';
  $('taskTarget').textContent=dockerDisk?`Docker 所在文件系统 ${dockerDisk.mount} · 可用 ${fmt(dockerDisk.available)}`:'各文件系统容量与用量';$('taskTarget').title='';
  $('taskContainers').textContent=`${snapshot.containers.length} 个容器 · Host`;
  $('taskCountLabel').textContent='累计已扫描';$('taskScanned').textContent=fmt(scanned);
  $('taskCapacity').textContent=total?` / 总容量合计 ${fmt(total)}`:' / 文件系统总容量未知';
  $('taskPercent').textContent=total?`占总容量 ${percentLabel(scanned,total)}`:'容量未知';
  $('taskMeter').classList.toggle('indeterminate',false);
  $('taskFill').style.width=`${percent(scanned,total)}%`;$('taskUsedFill').style.width=`${percent(used,total)}%`;
  $('taskMeter').setAttribute('aria-label','累计已扫描空间');
  $('taskMeter').setAttribute('aria-valuetext',`累计已扫描 ${fmt(scanned)}，${capacityNote}`);
  if(total)$('taskMeter').setAttribute('aria-valuenow',String(percent(scanned,total)));else $('taskMeter').removeAttribute('aria-valuenow');
  $('taskCapacityNote').textContent=capacityNote;
  $('taskProgress').textContent=`已统计 ${(snapshot.tree.files || 0).toLocaleString('zh-CN')} 个文件`;
  $('taskPath').textContent=`记录更新于 ${new Date(snapshot.updated_at || snapshot.finished_at).toLocaleString('zh-CN')}`;$('taskPath').title=$('taskPath').textContent;
}
function stopSnapshotStream() {
  if(platform.stream)platform.stream.source.close();
  platform.stream=null;
}
function startSnapshotStream() {
  if(!snapshot || !platform.user || platform.resultLoad || platform.changesLoad)return;
  const id=snapshot.job_id;
  if(platform.stream && platform.stream.id===id)return;
  stopSnapshotStream();
  const stream={id,generation:platform.generation,source:new EventSource(apiURL(`/api/jobs/${encodeURIComponent(id)}/events?view=1&revision=${snapshot.revision}`))};
  platform.stream=stream;
  stream.source.addEventListener('changes',event=>{
    if(platform.stream!==stream || platform.generation!==stream.generation || !snapshot || snapshot.job_id!==id || platform.resultLoad || platform.changesLoad)return;
    try {
      const changes=JSON.parse(event.data);
      if(changes.revision<=snapshot.revision)return;
      stopSnapshotStream();
      loadSnapshotChanges();
    } catch(error) {
      stopSnapshotStream();
      platform.changesError={id,message:error.message};
      message('目录明细读取失败：'+error.message);controls();
    }
  });
  // Network interruptions reconnect automatically. State polling also repairs
  // a missed notification by reloading the current display view.
}
function renderResultLoading(p) {
  const stages=SnapshotLoader.stages, index=stages.findIndex(([key])=>key===p.stage);
  const measured=Number.isFinite(p.total)&&p.total>0;
  const done=Math.max(0,Number(p.done)||0), ratio=measured?Math.min(100,done/p.total*100):null;
  $('resultLoadingPhase').textContent=index<0?'正在处理扫描结果':stages[index][1];
  $('resultLoadingStep').textContent=`阶段 ${Math.max(0,index)+1} / ${stages.length}`;
  $('resultLoadingDetail').textContent=p.detail || '';
  $('resultLoadingPath').textContent=p.current || '';$('resultLoadingPath').title=p.current || '';
  const count=value=>p.unit==='bytes'?fmt(value):`${value.toLocaleString('zh-CN')} ${p.unit || '项'}`;
  $('resultLoadingCount').textContent=measured?`${count(done)} / ${count(p.total)}`:done?`已处理 ${count(done)}`:p.bytes?`文档大小 ${fmt(p.bytes)}`:'正在处理…';
  $('resultLoadingPercent').textContent=ratio===null?'进行中':`${Math.floor(ratio)}%`;
  const meter=$('resultLoadingMeter'),fill=$('resultLoadingFill');
  meter.classList.toggle('indeterminate',!measured);
  const phaseKey=`${p.stage}:${p.detail}`;
  if(meter.dataset.phase!==phaseKey){fill.style.transition='none';fill.style.width='0%';void fill.offsetWidth;fill.style.transition='';meter.dataset.phase=phaseKey;}
  fill.style.width=`${ratio===null?0:ratio}%`;
  if(ratio===null)meter.removeAttribute('aria-valuenow');else meter.setAttribute('aria-valuenow',String(ratio));
  meter.setAttribute('aria-valuetext',`${$('resultLoadingPhase').textContent}，${$('resultLoadingCount').textContent}`);
  $('resultLoadingStages').innerHTML=stages.map(([key,label],i)=>`<li class="${i<index?'done':i===index?'current':''}"${i===index?' aria-current="step"':''}><span aria-hidden="true">${i<index?'✓':i+1}</span>${esc(label)}</li>`).join('');
}
function cancelResultLoading() {
  const pending=platform.resultLoad;
  if(!pending)return;
  platform.skippedAutoLoad=pending.id;
  pending.controller.abort();
  message('已取消读取扫描结果。当前查看的结果已保留。');
}
function loadJob(id, historical=false, preserveExplorer=false) {
  stopSnapshotStream();
  if(platform.changesLoad)platform.changesLoad.controller.abort();
  if(platform.resultLoad && platform.resultLoad.id===id)return platform.resultLoad.promise;
  if(platform.resultLoad)platform.resultLoad.controller.abort();
  const pending={id,controller:new AbortController(),sequence:++platform.loadSequence,generation:platform.generation};
  platform.resultLoad=pending;
  const current=()=>!pending.controller.signal.aborted && pending.sequence===platform.loadSequence && pending.generation===platform.generation;
  pending.promise=(async()=>{
    try {
      $('resultLoadingTitle').textContent=historical?'正在打开历史扫描结果':'正在打开扫描结果';
      $('resultLoadingJob').textContent=`任务 ${id.slice(0,8)}`;
      renderResultLoading({stage:'download',done:0,total:null,detail:'等待服务器读取扫描结果'});
      if(!$('resultLoadingDialog').open)$('resultLoadingDialog').showModal();
      const prepared=await SnapshotLoader.read(viewURL(id,preserveExplorer),{signal:pending.controller.signal,onProgress:p=>{if(current())renderResultLoading(p);}});
      if(!current())return false;
      renderResultLoading({stage:'render',done:0,total:1,unit:'个视图',detail:'展示磁盘概览、容器排行与目录明细'});
      // Let the browser paint and handle cancellation before committing the new
      // result. Accounting has already been completed by the server.
      await new Promise(resolve=>requestAnimationFrame(()=>requestAnimationFrame(resolve)));
      if(!current())return false;
      load(prepared.data,historical?'历史扫描结果':'最新扫描结果',prepared.usage,preserveExplorer);
      platform.loaded=id;platform.followLatest=!historical;platform.skippedAutoLoad=null;
      platform.changesError=null;
      $('resultContent').hidden=false;$('firstScan').hidden=true;
      renderHistory();controls();message('');startSnapshotStream();
      return true;
    } catch(error) {
      if(error.name==='AbortError' || !current())return false;
      // A failed automatic read should also wait for an explicit retry.
      platform.skippedAutoLoad=id;
      if(error.status===401){showAuth(false,'会话已过期，请重新登录。');return false;}
      throw error;
    } finally {
      if(platform.resultLoad===pending){
        platform.resultLoad=null;
        if($('resultLoadingDialog').open)$('resultLoadingDialog').close();
        if(platform.user){controls();startSnapshotStream();}
      }
    }
  })();
  return pending.promise;
}
function viewURL(id,preserve=true,path) {
  const current=path ?? (preserve && explorer?.trail.length ? explorer.trail[explorer.trail.length-1].path : '');
  return apiURL(`/api/jobs/${encodeURIComponent(id)}/view${current ? '?path='+encodeURIComponent(current) : ''}`);
}
function loadSnapshotChanges() {
  if(platform.changesLoad)return platform.changesLoad.promise;
  if(!snapshot || !platform.loaded || platform.resultLoad)return Promise.resolve(false);
  const base=snapshot, id=platform.loaded, requestedURL=viewURL(platform.loaded);
  const pending={id,controller:new AbortController(),generation:platform.generation,sequence:platform.loadSequence};
  platform.changesLoad=pending;platform.changesError=null;
  const current=()=>!pending.controller.signal.aborted && pending.generation===platform.generation && pending.sequence===platform.loadSequence && snapshot===base && platform.loaded===id;
  pending.promise=(async()=>{
    try {
      const prepared=await SnapshotLoader.read(requestedURL,{signal:pending.controller.signal});
      if(!current() || viewURL(id)!==requestedURL)return false;
      load(prepared.data,platform.followLatest?'最新扫描结果':'历史扫描结果',prepared.usage,true);
      platform.skippedAutoLoad=null;
      renderHistory();controls();startSnapshotStream();
      return true;
    } catch(error) {
      if(error.name==='AbortError' || !current())return false;
      platform.skippedAutoLoad=id;
      platform.changesError={id,message:error.message};
      message('目录明细读取失败：'+error.message);
      return false;
    } finally {
      if(platform.changesLoad===pending){platform.changesLoad=null;if(platform.user){controls();startSnapshotStream();}}
    }
  })();
  if(platform.user)controls();
  return pending.promise;
}
$('cancelResultLoading').addEventListener('click',cancelResultLoading);
$('resultLoadingDialog').addEventListener('cancel',e=>{e.preventDefault();cancelResultLoading();});
function showPage(page,navigate=true) {
  if(!platform.user || !['dashboard','overview','history','cleanup','scan-settings','processes','gpus','containers','bastion','members','agent-settings','update-settings','mihomo','settings','cluster','allocations'].includes(page))return;
  if(['scan-settings','agent-settings','update-settings','mihomo','cleanup','bastion','members'].includes(page) && platform.user.role!=='admin')return;
  const central=['cluster','allocations','bastion','members','agent-settings','update-settings','mihomo','settings'].includes(page);
  if(platform.nodeID&&central){window.location.href='/#'+page;return;}
  if(!platform.nodeID&&!central)return;
  const changed=platform.page!==page;
  platform.page=page;
  $('console').dataset.page=page;
  const storage=['overview','history','cleanup','scan-settings'].includes(page);
  const headings={mihomo:['CLUSTER / PROXY','代理管理','统一管理订阅、模板和各节点的 mihomo 服务。'],gpus:['GPU / MONITORING','GPU 管理','查看 GPU 状态、进程归属和最近 72 小时的使用记录。'],'update-settings':['SYSTEM / UPDATES','更新设置','统一配置更新器、HTTP 代理和 GitHub webhook，查看版本及更新状态。'],bastion:['CLUSTER / SHARE NODES','Share node 管理','按节点查看连接信息、账号、公钥池和使用者资源。'],cluster:['CLUSTER OVERVIEW','集群总控','查看节点状态，进入每台主机的工作台。'],allocations:['CLUSTER / USER CONTAINERS','使用者容器','按使用者查看各节点的容器归属与数量。'],members:['CLUSTER / MEMBERS','集群使用者','配置登记信息，管理使用者与注册名额。'],containers:['CONTAINER MANAGEMENT','容器管理','创建工作环境，管理容器运行状态。'],cleanup:['STORAGE / DIAGNOSTIC CLEANUP','诊断清理','读取完整报告，逐项核对并清理目录。'],dashboard:['WORKSPACE OVERVIEW','总面板','主机的每个侧面，都在这里。'],overview:['STORAGE / SPACE USAGE','空间用量','从整盘到目录，看清空间的去向。'],history:['STORAGE / SCAN HISTORY','扫描记录','回看每次扫描，掌握空间变化。'],'scan-settings':['STORAGE / CONFIGURATION','扫描配置','按主机需要，定义扫描范围与节奏。'],processes:['PROCESS MANAGEMENT','进程管理','追踪活动进程，看清容器内的运行关系。'],'agent-settings':['AGENT / CONFIGURATION','Agent 设置','连接模型服务，为空间分析准备好你的 Agent。'],settings:['WORKSPACE / SETTINGS','设置','配置总控访问地址，管理工作台账号与权限。']};
  $('pageEyebrow').textContent=headings[page][0];$('pageTitle').textContent=headings[page][1];
  $('pageDescription').textContent=headings[page][2];$('moduleCrumb').textContent=storage?'存储':headings[page][1];
  document.title=`project alpha · ${headings[page][1]}`;
  document.querySelectorAll('.platform-page').forEach(el=>el.hidden=el.id!==`page-${page}`);
  document.querySelectorAll('.platform-nav [data-page]').forEach(el=>{const active=el.dataset.page===(storage?'overview':page);el.classList.toggle('active',active);el.setAttribute('aria-current',active?'page':'false');});
  document.querySelectorAll('#storageNav [data-page]').forEach(el=>el.setAttribute('aria-current',el.dataset.page===page?'page':'false'));

  $('storageNav').hidden=!storage;$('storageMonitor').hidden=!['overview','history'].includes(page);
  $('scanActions').hidden=!['overview','history'].includes(page);$('hostInfo').hidden=!storage;$('dashboardRefresh').hidden=page!=='dashboard';
  if(navigate && changed){window.history?.pushState(null,'',`#${page}`);window.scrollTo?.({top:0,behavior:'instant'});$(page==='cluster'?'clusterTitle':'pageTitle').focus?.({preventScroll:true});}
  if(page==='overview' && changed)syncState().catch(e=>{if(platform.user)message(e.message);});
  if(page==='scan-settings' && !platform.config) loadSettings().catch(e=>message(e.message));
  if(page==='settings')window.SettingsUI.open();
  if(page==='update-settings')window.UpdatesUI?.open();
  if(page==='mihomo')window.MihomoUI?.open();else window.MihomoUI?.leave();
  if(page==='agent-settings')window.SettingsUI.openModel();
  if(page==='dashboard')window.DashboardUI?.render();
  if(page==='containers')window.ContainersUI?.open();
  if(page==='members')window.MembersUI?.open();
  if(page==='bastion')window.BastionUI?.open();else window.BastionUI?.leave();
  if(['cluster','allocations'].includes(page))window.ClusterUI?.refresh();
  if(page==='gpus')window.GPUUI?.open();else window.GPUUI?.close();
  if(page==='processes')window.ProcessUI?.open();else window.ProcessUI?.close();
  if(page==='cleanup')window.CleanupUI?.open();else window.CleanupUI?.close();
}
window.addEventListener('popstate',()=>showPage(window.location.hash.slice(1)||(!platform.nodeID?'cluster':'dashboard'),false));
function renderHistory() {
  $('jobsBody').innerHTML=platform.history.filter(j=>j.trigger!=='incremental').map(j=>`<tr class="${j.id===platform.loaded?'history-selected':''}"><td>${esc(dateTime(j.created_at))}<span class="sub">${esc(j.created_by)} · ${esc(triggerNames[j.trigger]||j.trigger)} · ${esc(j.id.slice(0,8))}${j.snapshot_revision ? ` · 明细版本 ${j.snapshot_revision}` : ''}</span></td><td><span class="pill status-${esc(j.status)}">${esc(statusNames[j.status]||j.status)}</span>${j.warnings?`<span class="sub">${j.warnings} 条扫描提示</span>`:''}</td><td class="amount">${fmt(j.allocated)}</td><td>${j.status==='completed'?`<button data-open-job="${esc(j.id)}">查看结果</button> `:''}<button data-job-detail="${esc(j.id)}">详情</button>${platform.user && platform.user.role==='admin'?` <button class="danger" data-delete-job="${esc(j.id)}" ${['queued','running','cancelling'].includes(j.status)?'disabled title="任务结束后可删除"':''}>删除</button>`:''}</td></tr>`).join('') || '<tr><td colspan="4" class="empty">还没有扫描记录。任务开始后会自动保存在这里。</td></tr>';
  $('moreHistory').hidden=platform.historyExhausted || platform.history.length<50;
}
function openDeleteJob(id) {
  if(!platform.user || platform.user.role!=='admin' || platform.deleting)return;
  const job=platform.history.find(j=>j.id===id);
  if(!job || ['queued','running','cancelling'].includes(job.status))return;
  platform.deleteTarget=id;
  $('deleteJobSummary').textContent=`${dateTime(job.created_at)} · ${statusNames[job.status] || job.status} · ${id.slice(0,8)}`;
  $('deleteJobHint').textContent=job.trigger==='incremental'?'将永久删除该目录任务及其日志，原扫描记录中的目录结果会保留。':'将永久删除该记录、关联的目录扫描任务及结果文件，无法恢复。';
  $('deleteJobError').textContent='';$('confirmDeleteJob').disabled=false;
  $('deleteJobDialog').showModal();
}
function clearLoadedRecord(){
  stopSnapshotStream();
  if(platform.changesLoad)platform.changesLoad.controller.abort();
  platform.loadSequence++;platform.resultLoad=null;platform.changesLoad=null;
  if($('resultLoadingDialog').open)$('resultLoadingDialog').close();
  for(const name of ['containerDialog','ownerDialog'])if($(name).open)$(name).close();
  platform.loaded=null;platform.followLatest=true;platform.skippedAutoLoad=null;platform.changesError=null;
  snapshot=null;usage=null;selected=null;query='';ownerFilter=null;stateFilter='all';tablePage=0;
  $('resultContent').hidden=true;$('firstScan').hidden=false;
  $('sourceBadge').textContent='尚未扫描';$('hostInfo').textContent='尚未完成扫描';
}
async function deleteJob() {
  if(!platform.user || platform.user.role!=='admin' || !platform.deleteTarget || platform.deleting)return;
  const id=platform.deleteTarget,generation=platform.generation;
  platform.deleting=true;$('confirmDeleteJob').disabled=true;$('deleteJobError').textContent='';
  try {
    const result=await api(`/api/jobs/${encodeURIComponent(id)}`,{method:'DELETE'});
    if(generation!==platform.generation)return;
    const deleted=new Set(result.deleted_ids);
    deleted.forEach(id=>platform.deletedIDs.add(id));
    if(platform.resultLoad && deleted.has(platform.resultLoad.id))platform.resultLoad.controller.abort();
    if(deleted.has(platform.loaded)) {
      clearLoadedRecord();
    }
    platform.history=platform.history.filter(j=>!deleted.has(j.id));
    platform.jobs=platform.jobs.filter(j=>!deleted.has(j.id));
    if(deleted.has(platform.latest))platform.latest=null;
    $('jobDetails').hidden=true;$('jobDetails').innerHTML='';
    $('deleteJobDialog').close();platform.deleteTarget=null;renderHistory();controls();
    const note=result.cleanup_pending?'记录已删除，但部分结果文件清理失败，请检查服务日志。':`已删除 ${deleted.size} 条扫描记录。`;
    try{await syncState();message(note);}catch(error){message(note+' 刷新失败：'+error.message);}
  } catch(error) {
    if(generation===platform.generation)$('deleteJobError').textContent=error.message;
  } finally {
    if(generation===platform.generation){platform.deleting=false;$('confirmDeleteJob').disabled=false;}
  }
}
$('confirmDeleteJob').addEventListener('click',deleteJob);
$('closeDeleteJob').addEventListener('click',()=>{if(!platform.deleting){$('deleteJobDialog').close();platform.deleteTarget=null;}});
$('deleteJobDialog').addEventListener('cancel',e=>{if(platform.deleting)e.preventDefault();else platform.deleteTarget=null;});
async function jobDetails(id) {
  const generation=platform.generation;
  const j=await api(`/api/jobs/${encodeURIComponent(id)}`);
  if(generation!==platform.generation || platform.deletedIDs.has(id))return;
  $('jobDetails').hidden=false;$('jobDetails').innerHTML=`<h2>任务 ${esc(j.id.slice(0,8))} · ${esc(statusNames[j.status])}</h2><p>开始：${esc(dateTime(j.started_at))} / 结束：${esc(dateTime(j.finished_at))}</p>${j.error?`<p class="error-text">${esc(j.error)}</p>`:''}<h3>任务创建时的配置</h3><pre>${esc(JSON.stringify(j.config,null,2))}</pre><h3>最后进度</h3><pre>${esc(JSON.stringify(j.progress,null,2))}</pre>`;
}
async function loadSettings() {
  const generation=platform.generation;
  const result=await api('/api/settings');if(generation!==platform.generation)return;platform.config=result;
  const c=result.value;
  $('cfgRoots').value=c.root.join('\n');$('cfgExcludes').value=c.exclude.join('\n');$('cfgDocker').checked=!c.no_docker;
  $('cfgScanBackend').value=c.scan_backend;$('cfgDockerRoot').checked=c.include_docker_root;$('cfgDepth').value=c.max_depth;$('cfgNodes').value=c.max_nodes;
  $('cfgScanMode').value=c.scan_mode;
  $('cfgTimeout').value=c.docker_timeout;$('cfgOwnerLabel').value=c.owner_label;$('cfgInterval').value=c.interval_minutes;
  $('cfgScheduleMode').value=c.schedule_mode;$('cfgRetainRecords').value=c.retain_records;
  $('cfgScheduleTimes').value=c.schedule_times.join('\n');$('cfgScheduleTimezone').value=c.schedule_timezone;
  for(let day=0;day<7;day++)$('cfgWeekday'+day).checked=c.schedule_weekdays.includes(day);
  scanScheduleControls();
  $('settingsStatus').textContent=`已保存配置 · 版本 ${result.revision}`;
}
async function loadAccounts() {
  const generation=platform.generation;
  const [users,audit]=await Promise.all([api('/api/users'),api('/api/audit')]);
  if(generation!==platform.generation || !platform.user)return;
  $('usersBody').innerHTML=users.users.map(u=>`<tr><td>${esc(u.username)}${u.id===platform.user.id?'<span class="sub">当前账号</span>':''}</td><td>${u.id===platform.user.id?'管理员':`<select aria-label="${esc(u.username)} 的角色" data-user-role="${esc(u.id)}" data-enabled="${u.enabled}"><option value="viewer" ${u.role==='viewer'?'selected':''}>只读</option><option value="admin" ${u.role==='admin'?'selected':''}>管理员</option></select>`}</td><td>${u.enabled?'已启用':'已禁用'}</td><td>${u.id!==platform.user.id?`<button data-user-id="${esc(u.id)}" data-user-enabled="${u.enabled?0:1}" data-role="${esc(u.role)}">${u.enabled?'禁用':'启用'}</button>`:'—'}</td></tr>`).join('');
  $('auditList').innerHTML=audit.events.map(e=>`<div class="audit-row"><span>${esc(dateTime(e.at))}</span><b>${esc(e.actor)}</b><span>${esc(actionNames[e.action]||e.action)}</span><span>${esc(e.detail)}</span></div>`).join('') || '<p>暂无操作记录</p>';
}
async function act(fn,button) {
  if(button)button.disabled=true;
  try{await fn();}catch(error){message(error.message);}finally{if(button)button.disabled=false;if(platform.user)controls();}
}
$('authForm').addEventListener('submit',async e=>{
  e.preventDefault();$('authSubmit').disabled=true;$('authError').textContent='';
  try{const session=await api(platform.setup?'/api/setup':'/api/login',{method:'POST',body:JSON.stringify({username:$('authUsername').value,password:$('authPassword').value})});$('authPassword').value='';await enter(session);}
  catch(error){$('authError').textContent=error.message;if(platform.user)message(error.message);}
  finally{$('authSubmit').disabled=false;}
});
$('passwordButton').addEventListener('click',()=>{$('passwordForm').reset();$('passwordError').textContent='';$('passwordDialog').showModal();});
$('closePassword').addEventListener('click',()=>$('passwordDialog').close());
$('passwordForm').addEventListener('submit',async e=>{e.preventDefault();const button=e.submitter;if(button)button.disabled=true;try{await api('/api/password',{method:'POST',body:JSON.stringify({old_password:$('oldPassword').value,new_password:$('changedPassword').value})});$('passwordDialog').close();showAuth(false,'密码已修改，请使用新密码登录。');}catch(error){$('passwordError').textContent=error.message;}finally{if(button)button.disabled=false;}});
$('logoutButton').addEventListener('click',()=>act(async()=>{await api('/api/logout',{method:'POST',body:'{}'});showAuth(false);},$('logoutButton')));
$('startScan').addEventListener('click',()=>act(async()=>{
  platform.starting=true;controls();
  try{await api('/api/jobs',{method:'POST',body:'{}'});platform.followLatest=true;message('扫描任务已创建。可离开页面，任务会继续在后台运行。');await syncState();}
  finally{platform.starting=false;}
}));
$('cancelScan').addEventListener('click',()=>act(async()=>{if(!platform.active)return;await api(`/api/jobs/${platform.active.id}/cancel`,{method:'POST',body:'{}'});message('扫描已取消，之前完成的结果仍可查看。');await syncState();},$('cancelScan')));
$('refresh').addEventListener('click',()=>act(async()=>{const loaded=platform.loaded,revision=snapshot && snapshot.revision;platform.skippedAutoLoad=null;if(await syncState()===false)return;if(loaded && loaded===platform.loaded && (snapshot && snapshot.revision)===revision){if(!await loadSnapshotChanges())return;}message('已刷新扫描记录与容器归属。');},$('refresh')));
$('latestResult').addEventListener('click',()=>act(async()=>{if(platform.latest && await loadJob(platform.latest))showPage('overview');}));
$('reloadHistory').addEventListener('click',()=>act(()=>syncState(),$('reloadHistory')));
$('moreHistory').addEventListener('click',()=>act(async()=>{
  const last=platform.history[platform.history.length-1];if(!last)return;
  const response=await api(`/api/jobs?before=${last.created_at}`);
  const existing=new Set(platform.history.map(j=>j.id));platform.history.push(...response.jobs.filter(j=>!existing.has(j.id) && !platform.deletedIDs.has(j.id)));renderHistory();
  if(response.jobs.length<50){platform.historyExhausted=true;$('moreHistory').hidden=true;}
},$('moreHistory')));
function scanScheduleControls(){
  const mode=$('cfgScheduleMode').value;
  $('cfgIntervalFields').hidden=mode!=='interval';$('cfgInterval').disabled=mode!=='interval';
  $('cfgCalendarFields').hidden=mode!=='calendar';$('cfgScheduleTimezone').disabled=mode!=='calendar';
}
$('cfgScheduleMode').addEventListener('change',scanScheduleControls);
$('reloadSettings').addEventListener('click',()=>act(()=>loadSettings(),$('reloadSettings')));
$('settingsForm').addEventListener('submit',e=>{e.preventDefault();act(async()=>{
  if(!platform.config)throw Error('请先载入扫描配置');
  const lines=id=>$(id).value.split('\n').map(x=>x.trim()).filter(Boolean);
  const value={scan_backend:$('cfgScanBackend').value,scan_mode:$('cfgScanMode').value,root:lines('cfgRoots'),exclude:lines('cfgExcludes'),no_docker:!$('cfgDocker').checked,include_docker_root:$('cfgDockerRoot').checked,max_depth:Number($('cfgDepth').value),max_nodes:Number($('cfgNodes').value),docker_timeout:Number($('cfgTimeout').value),owner_label:$('cfgOwnerLabel').value.trim(),interval_minutes:Number($('cfgInterval').value),schedule_mode:$('cfgScheduleMode').value,schedule_times:lines('cfgScheduleTimes'),schedule_weekdays:[0,1,2,3,4,5,6].filter(day=>$('cfgWeekday'+day).checked),schedule_timezone:$('cfgScheduleTimezone').value.trim(),retain_records:Number($('cfgRetainRecords').value)};
  const generation=platform.generation;
  const result=await api('/api/settings',{method:'PUT',body:JSON.stringify({value,revision:platform.config.revision})});
  if(generation!==platform.generation)return;platform.config=result;
  $('settingsStatus').textContent=`保存成功 · 版本 ${platform.config.revision}`;message('扫描配置已保存，将应用于下一次任务。');await syncState();
},$('saveSettings'));});
$('createUserForm').addEventListener('submit',e=>{e.preventDefault();act(async()=>{
  await api('/api/users',{method:'POST',body:JSON.stringify({username:$('newUsername').value,password:$('newPassword').value,role:$('newRole').value})});
  $('createUserForm').reset();await loadAccounts();message('账号已创建。');
},e.submitter);});
$('reloadAudit').addEventListener('click',()=>act(()=>loadAccounts(),$('reloadAudit')));
let ownerId;
$('closeOwner').addEventListener('click',()=>$('ownerDialog').close());
$('ownerForm').addEventListener('submit',e=>{e.preventDefault();act(async()=>{
  await api('/api/owners',{method:'PUT',body:JSON.stringify({container_id:ownerId,owner:$('ownerInput').value})});
  if(platform.changesLoad){platform.changesLoad.controller.abort();await platform.changesLoad.promise;}
  stopSnapshotStream();
  if(directoryViewLoad){directoryViewLoad.controller.abort();directoryViewLoad=null;}
  $('ownerDialog').close();
  if(await loadSnapshotChanges())message('容器所属用户已保存，用户用量已更新。');
},e.submitter);});
async function expandLeaf(path) {
  if(!platform.user || platform.user.role!=='admin' || !snapshot || platform.active || platform.expandStarting || platform.changesLoad || platform.changesError && platform.changesError.id===snapshot.job_id) return;
  const id=snapshot.job_id,generation=platform.generation,revision=snapshot.revision;
  platform.expandStarting=path;platform.expandError=null;controls();
  try {
    const job=await api(`/api/jobs/${encodeURIComponent(id)}/expand`,{method:'POST',body:JSON.stringify({path,revision,depth:platform.expandDepth})});
    if(generation!==platform.generation)return;
    platform.active=job;platform.jobs=[job,...platform.jobs.filter(j=>j.id!==job.id)];
    message('正在扫描此目录，已读到的明细会实时更新。');
    startSnapshotStream();
    await syncState();
  } catch(error) {
    if(generation===platform.generation){platform.expandError={job:id,path,message:error.message};message(error.message);}
  } finally {
    if(generation===platform.generation){platform.expandStarting=null;if(platform.user)controls();}
  }
}
document.addEventListener('click',e=>{
  const button=e.target.closest('button');if(!button || !platform.user)return;
  if(button.dataset.retryChanges!==undefined)loadSnapshotChanges();
  if(button.dataset.cancelExpansion)act(async()=>{await api(`/api/jobs/${button.dataset.cancelExpansion}/cancel`,{method:'POST',body:'{}'});await syncState();},button);
  if(button.dataset.page)showPage(button.dataset.page);
  if(button.dataset.openJob)act(async()=>{if(await loadJob(button.dataset.openJob,true))showPage('overview');},button);
  if(button.dataset.deleteJob)openDeleteJob(button.dataset.deleteJob);
  if(button.dataset.jobDetail)act(()=>jobDetails(button.dataset.jobDetail),button);
  if(button.dataset.userId)act(async()=>{await api(`/api/users/${button.dataset.userId}`,{method:'PATCH',body:JSON.stringify({enabled:button.dataset.userEnabled==='1',role:button.dataset.role})});await loadAccounts();},button);
  if(button.dataset.editOwner && platform.user.role==='admin' && snapshot){const c=snapshot.containers.find(c=>c.id===button.dataset.editOwner);if(!c)return;ownerId=c.id;$('ownerContainer').textContent=c.name;$('ownerInput').value=Usage.ownerOf(c);$('ownerDialog').showModal();}
});
document.addEventListener('change',e=>{
  const el=e.target;if(el.dataset && el.dataset.userRole)act(async()=>{try{await api(`/api/users/${el.dataset.userRole}`,{method:'PATCH',body:JSON.stringify({enabled:el.dataset.enabled==='1',role:el.value})});}finally{await loadAccounts();}},el);
});
// Deferred modules must finish registering before restoring a session.
window.addEventListener('DOMContentLoaded',async()=>{try{const session=await api('/api/session');if(session.user)await enter(session,true);else showAuth(session.setup_required);}catch(error){showAuth(false,'无法连接后端：'+error.message);}},{once:true});
