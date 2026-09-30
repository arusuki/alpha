'use strict';
const cleanupView={reports:[],reportScope:'container',reportID:'',selectedReports:{host:'',container:''},cleanup:null,entries:[],selected:new Set(),collapsed:new Set(),sortDirections:{},epoch:0,selection:0,timer:null,posting:false,loading:false,visible:false};
const cleanupCategories=['','可立即删除','存在争议','必须保留','放错位置'];
const cleanupStatuses={pending:'待处理',deleted:'已清理',failed:'删除失败',deleting:'删除中 / 结果待核对',uncertain:'结果待核对'};
const cleanupAllowed=()=>platform.user?.role==='admin';
const cleanupBusy=()=>['running','cancelling'].includes(cleanupView.cleanup?.status);
const cleanupSelectable=row=>['pending','failed'].includes(row.status)&&!cleanupBusy()&&!!cleanupView.cleanup?.snapshot_id;
function cleanupFiltered(){
  const category=$('cleanupCategory').value,query=$('cleanupSearch').value.trim().toLowerCase();
  return cleanupView.entries.filter(row=>(!category||String(row.category)===category)&&(!query||`${row.path} ${row.summary} ${row.detail.reason||''} ${row.detail.locations?.join(' ')||''}`.toLowerCase().includes(query)));
}
function cleanupVisibleSelectable(){return cleanupFiltered().filter(row=>!cleanupView.collapsed.has(row.category)&&cleanupSelectable(row));}
function cleanupSorted(rows,category){
  const direction=cleanupView.sortDirections[category]||'desc';
  return [...rows].sort((a,b)=>{
    const left=a.detail.bytes,right=b.detail.bytes;
    if(left==null||right==null)return left==null&&right==null?a.path.localeCompare(b.path):left==null?1:-1;
    return (direction==='desc'?right-left:left-right)||a.path.localeCompare(b.path);
  });
}
function cleanupRow(row){return `<tr><td><input type="checkbox" data-cleanup-entry="${esc(row.id)}" aria-label="选择 ${esc(row.path)}" ${cleanupView.selected.has(row.id)?'checked':''} ${!cleanupSelectable(row)?'disabled':''}></td><td><span class="mono cleanup-path">${esc(row.path)}</span><span class="sub">${esc((row.detail.locations||[]).join('；'))}</span></td><td>${esc(row.detail.kind||'')}</td><td class="amount">${fmt(row.detail.bytes)}</td><td>${esc(row.summary)}<details><summary>处理条件</summary><p>${esc(row.detail.reason||'')}</p></details></td><td>${esc(cleanupStatuses[row.status]||row.status)}${row.error?`<span class="sub${row.status==='deleted'?'':' error-text'}">${esc(row.error)}</span>`:''}</td></tr>`;}
function cleanupControls(){
  const blocked=cleanupView.posting||cleanupView.loading,busy=cleanupBusy(),prepared=!!cleanupView.cleanup;
  $('cleanupReport').disabled=blocked;$('cleanupHostReport').disabled=blocked;$('cleanupRefresh').disabled=blocked;
  for(const [scope,buttonID,defaultText] of [['host','cleanupHostPrepare','读取 Host 条目'],['container','cleanupPrepare','读取容器条目']]){
    const id=cleanupView.selectedReports[scope],report=cleanupView.reports.find(row=>String(row.report_id)===id&&row.report_scope===scope),current=cleanupView.reportScope===scope&&cleanupView.reportID===id;
    $(buttonID).disabled=blocked||!report||(current&&(busy||prepared));
    if(!current)$(buttonID).textContent=report?.cleanup_id?'查看条目 / 进度':'选择报告';
    else $(buttonID).textContent=prepared?'已读取全部条目':defaultText;
  }
  $('cleanupCancel').textContent='停止删除';
  $('cleanupCancel').hidden=!busy;$('cleanupCancel').disabled=blocked||cleanupView.cleanup?.status==='cancelling';
  $('cleanupRemoveHistory').hidden=!cleanupView.cleanup;$('cleanupRemoveHistory').disabled=blocked||busy;
  $('cleanupDelete').disabled=blocked||!cleanupView.selected.size||busy;
  $('cleanupSelection').textContent=cleanupView.selected.size?`已选择 ${cleanupView.selected.size} 项（每次最多 100 项）`:'未选择条目';
  const visible=cleanupVisibleSelectable(),checked=visible.filter(row=>cleanupView.selected.has(row.id)).length;
  $('cleanupSelectAll').disabled=blocked||!visible.length;
  $('cleanupSelectAll').checked=visible.length>0&&checked===visible.length;$('cleanupSelectAll').indeterminate=checked>0&&checked<visible.length;
  document.querySelectorAll('[data-cleanup-entry]').forEach(input=>input.disabled=blocked||!cleanupSelectable(cleanupView.entries.find(row=>row.id===input.dataset.cleanupEntry)));
}
function renderCleanup(){
  const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID),cleanup=cleanupView.cleanup;
  const name=cleanupView.reportScope==='host'?'Host':'容器 Agent';
  $('cleanupWorkspaceTitle').textContent=`${name} · 目录清理`;
  $('cleanupHostSection').setAttribute('data-active',String(cleanupView.reportScope==='host'));
  $('cleanupContainerSection').setAttribute('data-active',String(cleanupView.reportScope==='container'));
  $('cleanupSourceHint').textContent=report?`${dateTime(report.created_at)} · ${name} 报告 · 源扫描 ${report.snapshot_id||'已删除（可查看报告条目，不能删除实际目录）'}`:`尚无${name}完整报告。请先在空间用量页生成报告。`;
  $('cleanupStatus').textContent=cleanup?`${cleanupBusy()?'正在后台删除，离开页面后仍会继续。':`共 ${cleanupView.entries.length} 项。`} 已清理 ${cleanupView.entries.filter(row=>row.status==='deleted').length} 项，失败 ${cleanupView.entries.filter(row=>row.status==='failed').length} 项，待核对 ${cleanupView.entries.filter(row=>['deleting','uncertain'].includes(row.status)).length} 项。${cleanup.error||''}`:'';
  const rows=cleanupFiltered(),filteredCategory=$('cleanupCategory').value,searching=!!$('cleanupSearch').value.trim();
  const categories=cleanupCategories.map((_,category)=>category).slice(1).filter(category=>(!filteredCategory||String(category)===filteredCategory)&&(!searching||rows.some(row=>row.category===category)));
  $('cleanupEntries').innerHTML=cleanupView.entries.length&&categories.length?categories.map(category=>{
    const group=cleanupSorted(rows.filter(row=>row.category===category),category),direction=cleanupView.sortDirections[category]||'desc';
    return `<details class="cleanup-group" data-cleanup-category="${category}" ${cleanupView.collapsed.has(category)?'':'open'}><summary><strong>${esc(cleanupCategories[category])}</strong><span>${group.length} 项</span></summary><div class="table-scroll"><table class="cleanup-table"><thead><tr><th scope="col">选择</th><th scope="col">${cleanupView.reportScope==='host'?'Host 物理路径 / 来源':'物理路径 / 容器位置'}</th><th scope="col">用途</th><th scope="col" aria-sort="${direction==='desc'?'descending':'ascending'}"><button type="button" data-cleanup-sort="${category}" aria-label="按实际占用${direction==='desc'?'从低到高':'从高到低'}排序">实际占用 <span aria-hidden="true">${direction==='desc'?'↓':'↑'}</span></button></th><th scope="col">简要说明</th><th scope="col">处理状态</th></tr></thead><tbody>${group.map(cleanupRow).join('')||'<tr><td colspan="6" class="empty">此分类暂无条目。</td></tr>'}</tbody></table></div></details>`;
  }).join(''): `<p class="cleanup-empty">${cleanup?'没有符合条件的条目。':'选择完整报告后读取条目。'}</p>`;
  cleanupControls();
}
function cleanupApply(result){
  if(!result.cleanup||!Array.isArray(result.entries))throw Error('清理记录格式无效');
  const entries=result.entries.map(row=>{const detail=JSON.parse(row.detail);if(!row.id||typeof row.path!=='string'||typeof row.summary!=='string'||!cleanupCategories[row.category]||!detail)throw Error('报告条目格式无效');return {...row,detail};});
  cleanupView.cleanup=result.cleanup;cleanupView.entries=entries;
  const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);
  if(report){report.cleanup_id=result.cleanup.id;report.cleanup_status=result.cleanup.status;}
  const ids=new Set(entries.filter(cleanupSelectable).map(row=>row.id));cleanupView.selected=new Set([...cleanupView.selected].filter(id=>ids.has(id)));
  renderCleanup();
}
function cleanupPoll(){
  clearTimeout(cleanupView.timer);
  if(cleanupView.visible&&cleanupBusy()&&!cleanupView.posting)cleanupView.timer=setTimeout(()=>readCleanup().catch(error=>{$('cleanupError').textContent=error.message;cleanupPoll();}),1000);
}
async function readCleanup(){
  if(!cleanupView.cleanup||!cleanupAllowed())return;
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.cleanup.id;
  try{const result=await api(`/api/agent/cleanups/${id}`);if(epoch===cleanupView.epoch&&selection===cleanupView.selection){cleanupApply(result);$('cleanupError').textContent='';}}
  finally{if(epoch===cleanupView.epoch&&selection===cleanupView.selection)cleanupPoll();}
}
async function selectCleanupReport(scope=cleanupView.reportScope){
  if(!['host','container'].includes(scope))throw Error('报告类型无效');
  const reportID=$(scope==='host'?'cleanupHostReport':'cleanupReport').value;
  clearTimeout(cleanupView.timer);cleanupView.selection++;cleanupView.reportScope=scope;cleanupView.reportID=reportID;cleanupView.selectedReports[scope]=reportID;cleanupView.cleanup=null;cleanupView.entries=[];cleanupView.selected.clear();cleanupView.collapsed.clear();cleanupView.sortDirections={};$('cleanupError').textContent='';
  const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);
  if(report&&report.report_scope!==scope)throw Error('报告类型与当前清理栏目不匹配');
  if(report?.cleanup_id){cleanupView.cleanup={id:report.cleanup_id,status:report.cleanup_status};}
  renderCleanup();if(cleanupView.cleanup){await readCleanup();}
}
async function loadCleanupReports(){
  if(!cleanupAllowed()||cleanupView.loading||cleanupView.posting)return;
  const epoch=cleanupView.epoch;cleanupView.loading=true;cleanupControls();
  try{
    const result=await api('/api/agent/cleanup-reports');if(epoch!==cleanupView.epoch)return;
    if(!Array.isArray(result.reports)||result.reports.some(row=>!['host','container'].includes(row.report_scope)))throw Error('清理报告列表格式无效');
    cleanupView.reports=result.reports;
    for(const [scope,selectID] of [['host','cleanupHostReport'],['container','cleanupReport']]){
      const rows=result.reports.filter(row=>row.report_scope===scope),selected=cleanupView.selectedReports[scope];
      $(selectID).innerHTML=rows.map(row=>`<option value="${row.report_id}">${esc(dateTime(row.created_at))} · ${esc(row.title)} · ${esc((row.snapshot_id||'已删除').slice(0,8))}</option>`).join('')||'<option value="">暂无完整报告</option>';
      if(rows.some(row=>String(row.report_id)===selected))$(selectID).value=selected;
      cleanupView.selectedReports[scope]=$(selectID).value;
    }
    const scope=cleanupView.selectedReports[cleanupView.reportScope]?cleanupView.reportScope:cleanupView.selectedReports.host?'host':'container';
    await selectCleanupReport(scope);
  }catch(error){if(epoch===cleanupView.epoch)$('cleanupError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.loading=false;cleanupControls();}}
}
async function cleanupAction(action){
  if(!cleanupAllowed()||cleanupView.posting||cleanupView.loading)return;
  const epoch=cleanupView.epoch;cleanupView.posting=true;cleanupControls();$('cleanupError').textContent='';
  try{
    if(action==='prepare'){
      const result=await api('/api/agent/cleanups',{method:'POST',body:JSON.stringify({report_id:Number(cleanupView.reportID)})});
      if(epoch===cleanupView.epoch)cleanupApply(result);
    }else {
      await api(`/api/agent/cleanups/${cleanupView.cleanup.id}/cancel`,{method:'POST',body:'{}'});
      if(epoch===cleanupView.epoch)await readCleanup();
    }
  }catch(error){if(epoch===cleanupView.epoch)$('cleanupError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.posting=false;cleanupControls();cleanupPoll();}}
}
async function openCleanupScope(scope){
  if(cleanupView.posting||cleanupView.loading)return;
  const epoch=cleanupView.epoch;cleanupView.loading=true;cleanupControls();
  try{await selectCleanupReport(scope);}
  catch(error){if(epoch===cleanupView.epoch)$('cleanupError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.loading=false;cleanupControls();}}
}
async function prepareCleanupScope(scope){
  if(cleanupView.posting||cleanupView.loading)return;
  if(cleanupView.reportScope!==scope||cleanupView.reportID!==$(scope==='host'?'cleanupHostReport':'cleanupReport').value){await openCleanupScope(scope);return;}
  if($(scope==='host'?'cleanupHostPrepare':'cleanupPrepare').disabled)return;
  await cleanupAction('prepare');
}
function openCleanupHistoryDialog(){
  if(!cleanupView.cleanup||cleanupBusy()||cleanupView.posting)return;
  const count=cleanupView.entries.length,deleted=cleanupView.entries.filter(row=>row.status==='deleted').length;
  $('cleanupHistoryDetails').textContent=`本次清理记录包含 ${count} 个条目${deleted?`，其中 ${deleted} 个已标记为删除成功`:''}。`;
  $('cleanupHistoryError').textContent='';$('cleanupHistoryDialog').showModal();
}
async function removeCleanupHistory(){
  if(!cleanupView.cleanup||cleanupBusy()||cleanupView.posting)return;
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.cleanup.id;
  cleanupView.posting=true;cleanupControls();$('cleanupHistoryConfirm').disabled=true;$('cleanupHistoryClose').disabled=true;$('cleanupHistoryError').textContent='';
  try{
    await api(`/api/agent/cleanups/${id}`,{method:'DELETE'});
    if(epoch!==cleanupView.epoch||selection!==cleanupView.selection)return;
    cleanupView.selection++;clearTimeout(cleanupView.timer);
    const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);
    if(report){report.cleanup_id=null;report.cleanup_status=null;}
    cleanupView.cleanup=null;cleanupView.entries=[];cleanupView.selected.clear();
    $('cleanupHistoryDialog').close();renderCleanup();$('cleanupStatus').textContent='清理记录已删除，可重新读取报告条目。';
  }catch(error){if(epoch===cleanupView.epoch&&selection===cleanupView.selection)$('cleanupHistoryError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.posting=false;$('cleanupHistoryConfirm').disabled=false;$('cleanupHistoryClose').disabled=false;cleanupControls();}}
}
function openCleanupDelete(){
  const rows=cleanupView.entries.filter(row=>cleanupView.selected.has(row.id));
  if(!rows.length||cleanupView.posting)return;
  if(rows.length>100){$('cleanupError').textContent='每次最多删除 100 项，请减少所选条目。';return;}
  if(rows.some(row=>rows.some(other=>row.id!==other.id&&row.path.startsWith(other.path+'/')))){$('cleanupError').textContent='所选条目包含父子目录，请仅选择其中一项。';return;}
  $('cleanupDeletePaths').innerHTML=rows.map(row=>`<p><strong>${esc(cleanupCategories[row.category])}</strong><br><span class="mono cleanup-path">${esc(row.path)}</span><br>${esc(row.summary)}</p>`).join('');
  $('cleanupSudoPassword').value='';$('cleanupDeleteError').textContent='';$('cleanupDeleteDialog').showModal();$('cleanupSudoPassword').focus();
}
async function deleteCleanupSelection(){
  if(cleanupView.posting||!cleanupAllowed()||!cleanupView.selected.size)return;
  if(!$('cleanupSudoPassword').reportValidity())return;
  const epoch=cleanupView.epoch,id=cleanupView.cleanup.id,entry_ids=[...cleanupView.selected];
  cleanupView.posting=true;cleanupControls();$('cleanupDeleteConfirm').disabled=true;$('cleanupDeleteClose').disabled=true;$('cleanupSudoPassword').disabled=true;
  try{
    const request={method:'POST',body:JSON.stringify({entry_ids,sudo_password:$('cleanupSudoPassword').value})};
    $('cleanupSudoPassword').value='';
    const pending=api(`/api/agent/cleanups/${id}/delete`,request);request.body='';
    const result=await pending;if(epoch!==cleanupView.epoch)return;
    cleanupView.selected.clear();cleanupApply(result);$('cleanupDeleteDialog').close();
    cleanupPoll();
  }catch(error){if(epoch===cleanupView.epoch){$('cleanupDeleteError').textContent=error.message;try{await readCleanup();}catch{}}}
  finally{if(epoch===cleanupView.epoch){cleanupView.posting=false;cleanupControls();$('cleanupDeleteConfirm').disabled=false;$('cleanupDeleteClose').disabled=false;$('cleanupSudoPassword').value='';$('cleanupSudoPassword').disabled=false;cleanupPoll();}}
}
window.CleanupUI={open(){cleanupView.visible=true;loadCleanupReports();},close(){cleanupView.visible=false;clearTimeout(cleanupView.timer);if($('cleanupHistoryDialog').open)$('cleanupHistoryDialog').close();},reset(){
  clearTimeout(cleanupView.timer);Object.assign(cleanupView,{reports:[],reportScope:'container',reportID:'',selectedReports:{host:'',container:''},cleanup:null,entries:[],selected:new Set(),collapsed:new Set(),sortDirections:{},epoch:cleanupView.epoch+1,selection:0,timer:null,posting:false,loading:false,visible:false});
  if($('cleanupDeleteDialog').open)$('cleanupDeleteDialog').close();
  if($('cleanupHistoryDialog').open)$('cleanupHistoryDialog').close();
  $('cleanupReport').innerHTML='<option value="">暂无完整报告</option>';$('cleanupHostReport').innerHTML='<option value="">暂无完整报告</option>';$('cleanupSearch').value='';$('cleanupCategory').value='';$('cleanupError').textContent='';$('cleanupDeletePaths').innerHTML='';$('cleanupDeleteError').textContent='';$('cleanupDeleteConfirm').disabled=false;$('cleanupDeleteClose').disabled=false;$('cleanupSudoPassword').value='';$('cleanupSudoPassword').disabled=false;$('cleanupHistoryError').textContent='';$('cleanupHistoryConfirm').disabled=false;$('cleanupHistoryClose').disabled=false;renderCleanup();
}};
$('cleanupRefresh').addEventListener('click',loadCleanupReports);
for(const [scope,selectID] of [['host','cleanupHostReport'],['container','cleanupReport']])$(selectID).addEventListener('change',()=>openCleanupScope(scope));
$('cleanupPrepare').addEventListener('click',()=>prepareCleanupScope('container'));
$('cleanupHostPrepare').addEventListener('click',()=>prepareCleanupScope('host'));
$('cleanupCancel').addEventListener('click',()=>cleanupAction('cancel'));
$('cleanupRemoveHistory').addEventListener('click',openCleanupHistoryDialog);
$('cleanupHistoryConfirm').addEventListener('click',removeCleanupHistory);
$('cleanupHistoryClose').addEventListener('click',()=>{if(!cleanupView.posting)$('cleanupHistoryDialog').close();});
$('cleanupHistoryDialog').addEventListener('cancel',event=>{if(cleanupView.posting)event.preventDefault();});
$('cleanupCategory').addEventListener('change',renderCleanup);$('cleanupSearch').addEventListener('input',renderCleanup);
$('cleanupEntries').addEventListener('toggle',event=>{if(!event.target.isConnected)return;const category=Number(event.target.dataset.cleanupCategory);if(!category)return;if(event.target.open)cleanupView.collapsed.delete(category);else cleanupView.collapsed.add(category);cleanupControls();},true);
$('cleanupEntries').addEventListener('click',event=>{const button=event.target.closest('[data-cleanup-sort]');if(!button)return;const category=Number(button.dataset.cleanupSort);cleanupView.sortDirections[category]=(cleanupView.sortDirections[category]||'desc')==='desc'?'asc':'desc';renderCleanup();});
$('cleanupEntries').addEventListener('change',event=>{const id=event.target.dataset.cleanupEntry;if(!id)return;if(event.target.checked)cleanupView.selected.add(id);else cleanupView.selected.delete(id);cleanupControls();});
$('cleanupSelectAll').addEventListener('change',event=>{for(const row of cleanupVisibleSelectable()){if(event.target.checked)cleanupView.selected.add(row.id);else cleanupView.selected.delete(row.id);}renderCleanup();});
$('cleanupDelete').addEventListener('click',openCleanupDelete);$('cleanupDeleteConfirm').addEventListener('click',deleteCleanupSelection);
$('cleanupDeleteClose').addEventListener('click',()=>{if(!cleanupView.posting){$('cleanupSudoPassword').value='';$('cleanupDeleteDialog').close();}});
$('cleanupDeleteDialog').addEventListener('cancel',event=>{if(cleanupView.posting)event.preventDefault();else $('cleanupSudoPassword').value='';});

$('cleanupDeleteDialog').addEventListener('close',()=>{$('cleanupSudoPassword').value='';});
$('cleanupSudoPassword').addEventListener('keydown',event=>{if(event.key==='Enter'){event.preventDefault();deleteCleanupSelection();}});
