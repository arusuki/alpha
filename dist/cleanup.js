'use strict';
const cleanupView={reports:[],reportID:'',session:null,phase:'extract',entries:[],selected:new Set(),collapsed:new Set(),sortDirections:{},epoch:0,selection:0,timer:null,stream:null,traceReady:false,readingTrace:false,cursor:0,requests:new Map(),posting:false,loading:false,visible:false};
const cleanupCategories=['','可立即删除','存在争议','必须保留','放错位置'];
const cleanupStatuses={pending:'待处理',deleted:'已清理',failed:'删除失败',deleting:'删除中 / 结果待核对',uncertain:'结果待核对'};
const cleanupTraceWindowChars=32000;
const cleanupAllowed=()=>platform.user?.role==='admin';
const cleanupBusy=()=>['queued','running','scanning','cancelling'].includes(cleanupView.session?.status);
const cleanupSelectable=row=>['pending','failed'].includes(row.status)&&(cleanupView.session?.status==='completed'||cleanupView.phase==='delete')&&!cleanupBusy()&&!!cleanupView.session?.snapshot_id;
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
function cleanupRow(row){return `<tr><td><input type="checkbox" data-cleanup-entry="${esc(row.id)}" aria-label="选择 ${esc(row.path)}" ${cleanupView.selected.has(row.id)?'checked':''} ${!cleanupSelectable(row)?'disabled':''}></td><td><span class="mono cleanup-path">${esc(row.path)}</span><span class="sub">${esc((row.detail.locations||[]).join('；'))}</span></td><td>${esc(row.detail.kind||'')}</td><td class="amount">${fmt(row.detail.bytes)}</td><td>${esc(row.summary)}<details><summary>原报告说明</summary><p>${esc(row.detail.summary||'')} ${esc(row.detail.reason||'')}</p></details></td><td>${esc(cleanupStatuses[row.status]||row.status)}${row.error?`<span class="sub${row.status==='deleted'?'':' error-text'}">${esc(row.error)}</span>`:''}</td></tr>`;}
function cleanupControls(){
  const blocked=cleanupView.posting||cleanupView.loading;
  $('cleanupReport').disabled=blocked;$('cleanupRefresh').disabled=blocked;
  $('cleanupExtract').disabled=blocked||!cleanupView.reportID||cleanupBusy()||cleanupView.session?.status==='completed'||cleanupView.phase==='delete';
  $('cleanupExtract').textContent=cleanupView.phase==='delete'||cleanupView.session?.status==='completed'?'已提取全部条目':cleanupBusy()?'正在提取…':'提取报告条目';
  $('cleanupCancel').textContent=cleanupView.phase==='delete'?'停止删除':'停止提取';
  $('cleanupCancel').hidden=!cleanupBusy();$('cleanupCancel').disabled=blocked||cleanupView.session?.status==='cancelling';
  $('cleanupRemoveHistory').hidden=!cleanupView.session;$('cleanupRemoveHistory').disabled=blocked||cleanupBusy();
  $('cleanupDelete').disabled=blocked||!cleanupView.selected.size||cleanupBusy();
  $('cleanupSelection').textContent=cleanupView.selected.size?`已选择 ${cleanupView.selected.size} 项（每次最多 100 项）`:'未选择条目';
  const visible=cleanupVisibleSelectable(),checked=visible.filter(row=>cleanupView.selected.has(row.id)).length;
  $('cleanupSelectAll').disabled=blocked||!visible.length;
  $('cleanupSelectAll').checked=visible.length>0&&checked===visible.length;$('cleanupSelectAll').indeterminate=checked>0&&checked<visible.length;
  document.querySelectorAll('[data-cleanup-entry]').forEach(input=>input.disabled=blocked||!cleanupSelectable(cleanupView.entries.find(row=>row.id===input.dataset.cleanupEntry)));
}
function renderCleanup(){
  const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID),session=cleanupView.session;
  $('cleanupSourceHint').textContent=report?`${dateTime(report.created_at)} · 源扫描 ${report.snapshot_id||'已删除（可查看报告条目，不能删除实际目录）'}`:'尚无完整报告。请先在空间用量页生成空间报告。';
  $('cleanupStatus').textContent=cleanupView.phase==='delete'?`${cleanupBusy()?'正在后台删除，离开页面后仍会继续。':'删除任务已结束。'} 已清理 ${cleanupView.entries.filter(row=>row.status==='deleted').length} 项，失败 ${cleanupView.entries.filter(row=>row.status==='failed').length} 项，待核对 ${cleanupView.entries.filter(row=>['deleting','uncertain'].includes(row.status)).length} 项。${session?.error||''} 请重新扫描以更新空间统计。`:cleanupBusy()?'Agent 正在读取完整报告并提取全部四类条目，离开页面后仍会继续。':session?.status==='completed'?`已提取 ${cleanupView.entries.length} 项。`:(session?.error||'');
  const rows=cleanupFiltered(),filteredCategory=$('cleanupCategory').value,searching=!!$('cleanupSearch').value.trim();
  const categories=cleanupCategories.map((_,category)=>category).slice(1).filter(category=>(!filteredCategory||String(category)===filteredCategory)&&(!searching||rows.some(row=>row.category===category)));
  $('cleanupEntries').innerHTML=cleanupView.entries.length&&categories.length?categories.map(category=>{
    const group=cleanupSorted(rows.filter(row=>row.category===category),category),direction=cleanupView.sortDirections[category]||'desc';
    return `<details class="cleanup-group" data-cleanup-category="${category}" ${cleanupView.collapsed.has(category)?'':'open'}><summary><strong>${esc(cleanupCategories[category])}</strong><span>${group.length} 项</span></summary><div class="table-scroll"><table class="cleanup-table"><thead><tr><th scope="col">选择</th><th scope="col">物理路径 / 容器位置</th><th scope="col">用途</th><th scope="col" aria-sort="${direction==='desc'?'descending':'ascending'}"><button type="button" data-cleanup-sort="${category}" aria-label="按实际占用${direction==='desc'?'从低到高':'从高到低'}排序">实际占用 <span aria-hidden="true">${direction==='desc'?'↓':'↑'}</span></button></th><th scope="col">简要说明</th><th scope="col">处理状态</th></tr></thead><tbody>${group.map(cleanupRow).join('')||'<tr><td colspan="6" class="empty">此分类暂无条目。</td></tr>'}</tbody></table></div></details>`;
  }).join(''): `<p class="cleanup-empty">${cleanupBusy()?'正在提取，完成后展示全部条目。':session?.status==='completed'?'没有符合条件的条目。':'选择完整报告后提取条目。'}</p>`;
  cleanupControls();
}
function cleanupApply(result){
  if(!result.session||!['extract','delete'].includes(result.phase)||!Array.isArray(result.entries))throw Error('目录提取结果格式无效');
  const entries=result.entries.map(row=>{const detail=JSON.parse(row.detail);if(!row.id||typeof row.path!=='string'||typeof row.summary!=='string'||!cleanupCategories[row.category]||!detail)throw Error('报告条目格式无效');return {...row,detail};});
  cleanupView.session=result.session;cleanupView.phase=result.phase;cleanupView.entries=entries;
  if(cleanupView.phase!=='extract'||!cleanupBusy()){closeCleanupStream();cleanupFinishPending();}
  const ids=new Set(entries.filter(cleanupSelectable).map(row=>row.id));cleanupView.selected=new Set([...cleanupView.selected].filter(id=>ids.has(id)));
  renderCleanup();
}
function cleanupPoll(){
  clearTimeout(cleanupView.timer);
  if(cleanupView.visible&&cleanupBusy()&&!cleanupView.posting&&!(cleanupView.phase==='extract'&&cleanupView.stream))cleanupView.timer=setTimeout(()=>readCleanup().then(()=>{if(cleanupView.phase==='extract'&&!cleanupView.stream)readCleanupTrace();}).catch(error=>{$('cleanupError').textContent=error.message;cleanupPoll();}),1000);
}
function closeCleanupStream(){if(cleanupView.stream){cleanupView.stream.close();cleanupView.stream=null;}}
function resetCleanupTrace(){
  closeCleanupStream();cleanupView.cursor=0;cleanupView.traceReady=false;cleanupView.readingTrace=false;cleanupView.requests=new Map();
  $('cleanupAgentLog').replaceChildren();$('cleanupAgentPanel').hidden=true;$('cleanupAgentConnection').textContent='';
}
function cleanupTraceStep(title,time){
  const article=document.createElement('article'),header=document.createElement('header'),heading=document.createElement('strong'),date=document.createElement('span');
  article.className='cleanup-agent-step';heading.textContent=title;date.className='form-note';date.textContent=time?dateTime(time):'';
  header.append(heading,date);article.append(header);$('cleanupAgentLog').append(article);return article;
}
function updateCleanupStreamText(element,current,previous,replace=false){
  if(current===previous)return;
  if(!replace&&current.startsWith(previous))element.append(document.createTextNode(current.slice(previous.length)));
  else element.textContent=current;
}
function setCleanupTraceTail(request,kind,value,replace=false,truncated=false){
  let next=replace?value:(request[kind]||'')+value;
  if(next.length>cleanupTraceWindowChars){next=next.slice(-cleanupTraceWindowChars);truncated=true;}
  request[kind]=next;request[kind+'Truncated']=replace?truncated:request[kind+'Truncated']||truncated;
}
function renderCleanupRequest(request){
  const thinking=request.protocol==='completions'?request.reasoning:request.summary;
  request.thinking.hidden=!thinking;
  const thinkingFollow=request.thinkingText.scrollHeight-request.thinkingText.scrollTop-request.thinkingText.clientHeight<40;
  const outputFollow=request.output.scrollHeight-request.output.scrollTop-request.output.clientHeight<40;
  request.outputSection.hidden=!request.text;
  updateCleanupStreamText(request.thinkingText,thinking,request.renderedThinking||'',request.reasoningTruncated||request.summaryTruncated);request.renderedThinking=thinking;
  updateCleanupStreamText(request.output,request.text,request.renderedText||'',request.textTruncated);request.renderedText=request.text;
  if(thinkingFollow)request.thinkingText.scrollTop=request.thinkingText.scrollHeight;
  if(outputFollow)request.output.scrollTop=request.output.scrollHeight;
  const status=request.status||'running';
  request.state.textContent=({running:request.text?'正在流式输出…':thinking?'正在思考…':'等待模型输出…',completed:'输出完成',failed:'模型请求失败 · 已收到的内容保留',cancelled:'已停止 · 已收到的内容保留',interrupted:'已中断 · 已收到的内容保留'}[status]||status);
  request.error.textContent=request.errorText||'';
  request.thinkingHint.textContent=request.reasoningTruncated||request.summaryTruncated?'较早的思考内容已截断，只显示最近片段。':status==='running'||thinking?'':request.protocol==='responses'?'模型未返回公开思考摘要。':'模型未返回思考内容。';
  request.outputHint.textContent=request.textTruncated?'较早的模型输出已截断，只显示最近片段；完整结果仍用于条目校验。':'';
}
function cleanupFinishPending(){
  if(cleanupBusy())return;
  for(const request of cleanupView.requests.values())if(!request.status){request.status=cleanupView.session?.status||'interrupted';renderCleanupRequest(request);}
}
function appendCleanupTrace(messages){
  const log=$('cleanupAgentLog'),follow=log.scrollHeight-log.scrollTop-log.clientHeight<80;
  for(const message of messages){
    let data;
    if(['model_request','model_delta','model_response','cleanup_validation'].includes(message.role)){
      try{data=JSON.parse(message.content);}catch{continue;}
    }
    if(message.role==='model_request'){
      const article=cleanupTraceStep(`第 ${data.round||1} 次提取`,message.created_at);
      const thinking=document.createElement('details'),summary=document.createElement('summary'),thinkingText=document.createElement('pre'),thinkingHint=document.createElement('p');
      summary.textContent=data.protocol==='completions'?'完整思考':'思考摘要';thinking.append(summary,thinkingText);thinking.hidden=true;
      const outputSection=document.createElement('div'),outputLabel=document.createElement('strong'),outputHint=document.createElement('p'),output=document.createElement('pre');
      outputSection.className='cleanup-agent-output';outputLabel.textContent='模型输出';outputHint.className='form-note';outputSection.append(outputLabel,outputHint,output);outputSection.hidden=true;
      const state=document.createElement('p'),error=document.createElement('p');state.className='form-note';error.className='error-text';thinkingHint.className='form-note';
      article.append(thinking,thinkingHint,outputSection,state,error);
      const request={protocol:data.protocol,text:'',summary:'',reasoning:'',status:'',errorText:'',thinking,thinkingText,thinkingHint,outputSection,outputHint,output,state,error};
      cleanupView.requests.set(data.request_id,request);renderCleanupRequest(request);
    }else if(message.role==='model_delta'){
      const request=cleanupView.requests.get(data.request_id);if(!request||!Array.isArray(data.deltas))continue;
      for(const delta of data.deltas){
        if(['text','summary','reasoning'].includes(delta.kind))setCleanupTraceTail(request,delta.kind,delta.text||'');
        else if(['text_snapshot','summary_snapshot','reasoning_snapshot'].includes(delta.kind))setCleanupTraceTail(request,delta.kind.replace('_snapshot',''),delta.text||'',true,!!delta.dropped_bytes);
      }
      renderCleanupRequest(request);
    }else if(message.role==='model_response'){
      const request=cleanupView.requests.get(data.request_id);if(!request)continue;
      request.status=data.status;request.errorText=data.error||'';
      for(const kind of ['text','summary','reasoning'])if(data[kind])setCleanupTraceTail(request,kind,data[kind],true,!!data[kind+'_dropped_bytes']);
      renderCleanupRequest(request);
    }else if(message.role==='cleanup_validation'){
      const article=cleanupTraceStep(`第 ${data.round||1} 次结果未通过校验`,message.created_at),note=document.createElement('p');
      note.className='error-text';note.textContent=data.error||'输出格式无效';article.append(note);
    }else if(message.role==='assistant'){
      const article=cleanupTraceStep('提取结果',message.created_at),note=document.createElement('p');note.textContent=message.content;article.append(note);
    }
  }
  $('cleanupAgentPanel').hidden=false;
  if(follow)log.scrollTop=log.scrollHeight;
}
function connectCleanupStream(){
  if(cleanupView.stream||!cleanupView.traceReady||!cleanupView.visible||!cleanupAllowed()||cleanupView.phase!=='extract'||!cleanupBusy())return;
  clearTimeout(cleanupView.timer);
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.session.id;
  const source=new EventSource(`/api/agent/sessions/${id}/events?after=${cleanupView.cursor}`);cleanupView.stream=source;
  const current=()=>epoch===cleanupView.epoch&&selection===cleanupView.selection&&cleanupView.stream===source&&cleanupView.visible;
  $('cleanupAgentConnection').textContent='正在连接实时输出';
  source.addEventListener('open',()=>{if(current())$('cleanupAgentConnection').textContent='实时连接已建立';});
  source.addEventListener('session',event=>{
    if(!current())return;
    try{
      const update=JSON.parse(event.data),previous=cleanupView.session.status;
      appendCleanupTrace(update.messages);cleanupView.cursor=update.next_after;cleanupView.session=update.session;
      if(previous!==update.session.status)renderCleanup();
      if(!cleanupBusy()&&!update.has_more){closeCleanupStream();cleanupFinishPending();$('cleanupAgentConnection').textContent='提取过程已完整接收';readCleanup().catch(error=>{$('cleanupError').textContent=error.message;});}
    }catch{closeCleanupStream();$('cleanupAgentConnection').textContent='实时连接中断，正在恢复';cleanupPoll();}
  });
  source.addEventListener('error',()=>{if(current()){closeCleanupStream();$('cleanupAgentConnection').textContent='实时连接中断，正在恢复';cleanupPoll();}});
}
async function readCleanupTrace(){
  if(!cleanupView.session||!cleanupView.visible||!cleanupAllowed()||cleanupView.readingTrace||cleanupView.stream)return;
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.session.id;
  const current=()=>epoch===cleanupView.epoch&&selection===cleanupView.selection&&cleanupView.session?.id===id&&cleanupView.visible;
  cleanupView.readingTrace=true;$('cleanupAgentPanel').hidden=false;$('cleanupAgentConnection').textContent='正在读取提取过程';
  try{
    let update;
    do{
      update=await api(`/api/agent/sessions/${id}?after=${cleanupView.cursor}`);if(!current())return;
      appendCleanupTrace(update.messages);cleanupView.cursor=update.next_after;cleanupView.session=update.session;
    }while(update.has_more);
    cleanupView.traceReady=true;renderCleanup();
  }catch(error){if(current())$('cleanupAgentConnection').textContent=`提取过程读取失败：${error.message}`;}
  finally{if(current()){cleanupView.readingTrace=false;connectCleanupStream();cleanupPoll();}}
}
async function readCleanup(){
  if(!cleanupView.session||!cleanupAllowed())return;
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.session.id;
  try{const result=await api(`/api/agent/cleanups/${id}`);if(epoch===cleanupView.epoch&&selection===cleanupView.selection){cleanupApply(result);$('cleanupError').textContent='';}}
  finally{if(epoch===cleanupView.epoch&&selection===cleanupView.selection)cleanupPoll();}
}
async function selectCleanupReport(){
  clearTimeout(cleanupView.timer);cleanupView.selection++;resetCleanupTrace();cleanupView.reportID=$('cleanupReport').value;cleanupView.session=null;cleanupView.phase='extract';cleanupView.entries=[];cleanupView.selected.clear();cleanupView.collapsed.clear();cleanupView.sortDirections={};$('cleanupError').textContent='';
  const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);
  if(report?.cleanup_id){cleanupView.session={id:report.cleanup_id,status:report.cleanup_status};cleanupView.phase=report.phase;}
  renderCleanup();if(cleanupView.session){await readCleanup();await readCleanupTrace();}
}
async function loadCleanupReports(){
  if(!cleanupAllowed()||cleanupView.loading||cleanupView.posting)return;
  const epoch=cleanupView.epoch;cleanupView.loading=true;cleanupControls();
  try{
    const result=await api('/api/agent/cleanup-reports');if(epoch!==cleanupView.epoch)return;
    cleanupView.reports=result.reports;
    $('cleanupReport').innerHTML=result.reports.map(row=>`<option value="${row.report_id}">${esc(dateTime(row.created_at))} · ${esc(row.title)} · ${esc((row.snapshot_id||'已删除').slice(0,8))}</option>`).join('')||'<option value="">暂无完整报告</option>';
    if(result.reports.some(row=>String(row.report_id)===cleanupView.reportID))$('cleanupReport').value=cleanupView.reportID;
    await selectCleanupReport();
  }catch(error){if(epoch===cleanupView.epoch)$('cleanupError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.loading=false;cleanupControls();}}
}
async function cleanupAction(action){
  if(!cleanupAllowed()||cleanupView.posting||cleanupView.loading)return;
  const epoch=cleanupView.epoch;cleanupView.posting=true;cleanupControls();$('cleanupError').textContent='';
  try{
    if(action==='extract'){
      const session=await api('/api/agent/cleanups',{method:'POST',body:JSON.stringify({report_id:Number(cleanupView.reportID)})});if(epoch!==cleanupView.epoch)return;
      if(cleanupView.session?.id!==session.id)resetCleanupTrace();
      cleanupView.session=session;const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);if(report){report.cleanup_id=session.id;report.cleanup_status=session.status;}
    }else await api(`/api/agent/sessions/${cleanupView.session.id}/cancel`,{method:'POST',body:'{}'});
    if(epoch===cleanupView.epoch){await readCleanup();await readCleanupTrace();}
  }catch(error){if(epoch===cleanupView.epoch)$('cleanupError').textContent=error.message;}
  finally{if(epoch===cleanupView.epoch){cleanupView.posting=false;cleanupControls();cleanupPoll();}}
}
function openCleanupHistoryDialog(){
  if(!cleanupView.session||cleanupBusy()||cleanupView.posting)return;
  const count=cleanupView.entries.length,deleted=cleanupView.entries.filter(row=>row.status==='deleted').length;
  $('cleanupHistoryDetails').textContent=`本次提取包含 ${count} 个条目${deleted?`，其中 ${deleted} 个已标记为删除成功`:''}。`;
  $('cleanupHistoryError').textContent='';$('cleanupHistoryDialog').showModal();
}
async function removeCleanupHistory(){
  if(!cleanupView.session||cleanupBusy()||cleanupView.posting)return;
  const epoch=cleanupView.epoch,selection=cleanupView.selection,id=cleanupView.session.id;
  cleanupView.posting=true;cleanupControls();$('cleanupHistoryConfirm').disabled=true;$('cleanupHistoryClose').disabled=true;$('cleanupHistoryError').textContent='';
  try{
    await api(`/api/agent/cleanups/${id}`,{method:'DELETE'});
    if(epoch!==cleanupView.epoch||selection!==cleanupView.selection)return;
    cleanupView.selection++;clearTimeout(cleanupView.timer);resetCleanupTrace();
    const report=cleanupView.reports.find(row=>String(row.report_id)===cleanupView.reportID);
    if(report){report.cleanup_id=null;report.cleanup_status=null;report.phase=null;}
    cleanupView.session=null;cleanupView.phase='extract';cleanupView.entries=[];cleanupView.selected.clear();
    $('cleanupHistoryDialog').close();renderCleanup();$('cleanupStatus').textContent='提取记录已删除，可基于完整报告重新提取。';
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
  const epoch=cleanupView.epoch,id=cleanupView.session.id,entry_ids=[...cleanupView.selected];
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
window.CleanupUI={open(){cleanupView.visible=true;loadCleanupReports();},close(){cleanupView.visible=false;clearTimeout(cleanupView.timer);closeCleanupStream();if($('cleanupHistoryDialog').open)$('cleanupHistoryDialog').close();},reset(){
  clearTimeout(cleanupView.timer);resetCleanupTrace();Object.assign(cleanupView,{reports:[],reportID:'',session:null,phase:'extract',entries:[],selected:new Set(),collapsed:new Set(),sortDirections:{},epoch:cleanupView.epoch+1,selection:0,timer:null,stream:null,traceReady:false,readingTrace:false,cursor:0,requests:new Map(),posting:false,loading:false,visible:false});
  if($('cleanupDeleteDialog').open)$('cleanupDeleteDialog').close();
  if($('cleanupHistoryDialog').open)$('cleanupHistoryDialog').close();
  $('cleanupReport').innerHTML='<option value="">暂无完整报告</option>';$('cleanupSearch').value='';$('cleanupCategory').value='';$('cleanupError').textContent='';$('cleanupDeletePaths').innerHTML='';$('cleanupDeleteError').textContent='';$('cleanupDeleteConfirm').disabled=false;$('cleanupDeleteClose').disabled=false;$('cleanupSudoPassword').value='';$('cleanupSudoPassword').disabled=false;$('cleanupHistoryError').textContent='';$('cleanupHistoryConfirm').disabled=false;$('cleanupHistoryClose').disabled=false;renderCleanup();
}};
$('cleanupRefresh').addEventListener('click',loadCleanupReports);
$('cleanupReport').addEventListener('change',()=>{cleanupView.loading=true;cleanupControls();selectCleanupReport().catch(error=>{$('cleanupError').textContent=error.message;}).finally(()=>{cleanupView.loading=false;cleanupControls();});});
$('cleanupExtract').addEventListener('click',()=>cleanupAction('extract'));
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
