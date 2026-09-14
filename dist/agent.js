'use strict';
const agentView={epoch:0,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:''};
const agentStatusNames={queued:'等待分析',scanning:'补查目录中',running:'正在生成分析',cancelling:'正在停止',completed:'分析完成',failed:'分析失败',cancelled:'分析已停止',interrupted:'分析已中断'};
const agentToolNames={get_overview:'核对整体占用',list_containers:'查看容器排行',list_owners:'查看用户归属',get_container:'核对容器存储来源',get_directory:'读取目录明细',scan_directory:'补查目录与文件时间'};
const agentBusy=session=>session && ['queued','scanning','running','cancelling'].includes(session.status);
const agentAllowed=()=>platform.user?.role==='admin';

// Render a deliberately small Markdown subset. All content is escaped first;
// raw HTML, images and model-supplied links never become active DOM content.
function reportInline(text){
  return String(text).split(/(`[^`]*`)/g).map(part=>part.startsWith('`')&&part.endsWith('`')?`<code>${esc(part.slice(1,-1))}</code>`:esc(part).replace(/\*\*([^*]+)\*\*/g,'<strong>$1</strong>')).join('');
}
function reportMarkdown(text){
  const lines=String(text).replace(/\r\n?/g,'\n').split('\n'),html=[];
  const cells=line=>line.trim().replace(/^\|/,'').replace(/\|$/,'').split(/(?<!\\)\|/).map(c=>c.trim().replace(/\\\|/g,'|'));
  const tableRule=line=>line && line.includes('|') && cells(line).every(c=>/^:?-{3,}:?$/.test(c));
  for(let i=0;i<lines.length;){
    const line=lines[i];
    if(!line.trim()){i++;continue;}
    if(/^\s*```/.test(line)){
      const code=[];i++;while(i<lines.length&&!/^\s*```/.test(lines[i]))code.push(lines[i++]);i++;
      html.push(`<pre><code>${esc(code.join('\n'))}</code></pre>`);continue;
    }
    if(line.includes('|')&&tableRule(lines[i+1])){
      const head=cells(line);i+=2;const rows=[];
      while(i<lines.length&&lines[i].trim()&&lines[i].includes('|'))rows.push(cells(lines[i++]));
      html.push(`<div class="table-scroll"><table><thead><tr>${head.map(c=>`<th>${reportInline(c)}</th>`).join('')}</tr></thead><tbody>${rows.map(row=>`<tr>${head.map((_,j)=>`<td>${reportInline(row[j]||'')}</td>`).join('')}</tr>`).join('')}</tbody></table></div>`);continue;
    }
    const heading=line.match(/^(#{1,6})\s+(.+)$/);
    if(heading){const level=Math.min(6,heading[1].length+1);html.push(`<h${level}>${reportInline(heading[2])}</h${level}>`);i++;continue;}
    const list=line.match(/^\s*(?:([-*+])|\d+[.)])\s+(.+)$/);
    if(list){
      const tag=list[1]?'ul':'ol',items=[];
      while(i<lines.length){const next=lines[i].match(/^\s*(?:([-*+])|\d+[.)])\s+(.+)$/);if(!next||(next[1]?'ul':'ol')!==tag)break;items.push(`<li>${reportInline(next[2])}</li>`);i++;}
      html.push(`<${tag}>${items.join('')}</${tag}>`);continue;
    }
    if(/^\s*([-*_])\1{2,}\s*$/.test(line)){html.push('<hr>');i++;continue;}
    html.push(line.startsWith('> ')?`<blockquote>${reportInline(line.slice(2))}</blockquote>`:`<p>${reportInline(line)}</p>`);i++;
  }
  return html.join('');
}
function agentControls(){
  const busy=agentBusy(agentView.session),pending=agentView.posting||agentView.reading;
  $('generateReport').disabled=!agentAllowed()||!snapshot||!platform.loaded||!!platform.resultLoad||!!platform.changesLoad||agentView.posting;
  $('agentSourceHint').textContent=snapshot&&platform.loaded?`基于当前${platform.followLatest?'':'历史'}扫描 · ${snapshot.host} · ${snapshot.finished_at}；补查结果同步到空间用量。`:'完成扫描后，即可基于当前记录生成总报告。';
  $('agentHistory').disabled=pending;$('reloadReports').disabled=pending;
  $('stopAgent').hidden=!busy;$('stopAgent').disabled=agentView.posting||agentView.session?.status==='cancelling';
  $('downloadReport').disabled=agentView.reading||!agentView.messages.some(m=>m.role==='assistant');
  $('agentFollowup').hidden=!agentView.session;
  $('sendAgentQuestion').disabled=pending||busy||!agentView.session?.snapshot_id;
  $('agentQuestion').disabled=pending||busy||!agentView.session?.snapshot_id;
  document.querySelectorAll('[data-agent-question]').forEach(b=>b.disabled=$('agentQuestion').disabled);
}
function agentError(error){agentView.error=error?error.message||String(error):'';$('agentError').textContent=agentView.error;}
function renderAgentSession(job){
  const session=agentView.session;
  $('agentRunStatus').textContent=session?(agentStatusNames[session.status]||session.status):'尚未生成报告';
  if(job?.progress){const p=job.progress;$('agentRunStatus').textContent+=` · 已遍历 ${(p.entries||0).toLocaleString('zh-CN')} 项${p.path?' · '+p.path:''}`;}
  $('agentReportSource').textContent=session?`模型 ${session.model} · ${dateTime(session.created_at)} · ${session.snapshot_id?'扫描记录 '+session.snapshot_id:'扫描记录已不存在，请重新生成报告'}${session.snapshot_id&&session.snapshot_id!==platform.loaded?' · 与当前页面所查看的记录不同':''}`:'';
  $('agentEmpty').hidden=agentView.messages.some(m=>m.role==='assistant');
  if(session?.error)agentError(session.error);
  agentControls();
}
function renderAgentHistory(){
  $('agentHistory').innerHTML=agentView.sessions.map(s=>`<option value="${esc(s.id)}">${esc(dateTime(s.created_at)+' · '+s.title+' · '+(agentStatusNames[s.status]||s.status))}</option>`).join('')||'<option value="">暂无分析记录</option>';
  $('agentHistory').value=agentView.session?.id||'';
}
function rememberAgentSession(session){
  agentView.session=session;
  agentView.sessions=[session,...agentView.sessions.filter(s=>s.id!==session.id)].sort((a,b)=>b.updated_at-a.updated_at);
  renderAgentHistory();
}
function appendAgentMessages(messages){
  for(const m of messages){
    agentView.messages.push(m);
    if(m.role==='assistant')$('agentReports').insertAdjacentHTML('beforeend',`<article class="agent-report"><div class="agent-answer-time">${esc(dateTime(m.created_at))}</div>${reportMarkdown(m.content)}</article>`);
    else if(m.role==='user'&&agentView.messages.some(v=>v.role==='assistant'))$('agentReports').insertAdjacentHTML('beforeend',`<p class="agent-user-question">${esc(m.content)}</p>`);
    else if(['status','note','tool_call','tool_result'].includes(m.role)){
      const label=m.tool_name?(agentToolNames[m.tool_name]||m.tool_name):m.role==='note'?'分析思路':'进度';
      let detail=m.content;
      if(m.role==='tool_call'){try{const args=JSON.parse(m.content);detail=args.path||args.container||args.sort_by||'读取统计';}catch{detail='读取统计';}}
      const content=m.role==='tool_result'?`<details><summary>${esc(label)} · 统计证据</summary><pre>${esc(detail)}</pre></details>`:`<p><strong>${esc(label)}</strong> ${esc(detail)}</p>`;
      $('agentActivityLog').insertAdjacentHTML('beforeend',content);
    }
  }
  const count=agentView.messages.filter(m=>m.role==='tool_call').length;
  $('agentActivityCount').textContent=`· ${count} 次工具调用`;
  $('agentActivity').hidden=!agentView.messages.length;
}
function scheduleAgentPoll(){
  clearTimeout(agentView.timer);
  if(agentAllowed()&&$('agentDialog').open&&agentBusy(agentView.session))agentView.timer=setTimeout(()=>readAgentSession(),1200);
}
async function readAgentSession(){
  if(!agentAllowed()||!agentView.session||agentView.reading)return;
  const epoch=agentView.epoch,selection=agentView.selection,id=agentView.session.id;
  const current=()=>epoch===agentView.epoch&&selection===agentView.selection&&agentAllowed();
  agentView.reading=true;agentControls();
  try{
    let result;
    do{
      result=await api(`/api/agent/sessions/${id}?after=${agentView.cursor}`);
      if(!current())return;
      appendAgentMessages(result.messages);agentView.cursor=result.next_after;
      rememberAgentSession(result.session);
    }while(result.has_more);
    agentError('');renderAgentSession(result.active_job);
  }catch(error){if(current())agentError(error);}
  finally{if(current()){agentView.reading=false;agentControls();scheduleAgentPoll();}}
}
async function selectAgentSession(session){
  clearTimeout(agentView.timer);agentView.selection++;agentView.cursor=0;agentView.messages=[];agentView.reading=false;
  $('agentReports').innerHTML='';$('agentActivityLog').innerHTML='';$('agentActivity').hidden=true;$('agentQuestion').value='';agentError('');
  rememberAgentSession(session);renderAgentSession();await readAgentSession();
}
async function loadAgentHistory(){
  const epoch=agentView.epoch;
  const result=await api('/api/agent/sessions');if(epoch!==agentView.epoch)return;
  agentView.sessions=result.sessions;renderAgentHistory();
}
async function openAgentReports(){
  if(!agentAllowed()||agentView.posting)return;
  if(!$('agentDialog').open)$('agentDialog').showModal();
  const epoch=agentView.epoch;agentError('');
  try{
    await loadAgentHistory();if(epoch!==agentView.epoch)return;
    if(agentView.session)await readAgentSession();
    else if(agentView.sessions.length)await selectAgentSession(agentView.sessions.find(agentBusy)||agentView.sessions[0]);
    else renderAgentSession();
  }catch(error){if(epoch===agentView.epoch)agentError(error);}
}
async function generateDiskReport(){
  if(!agentAllowed()||!snapshot||!platform.loaded||agentView.posting||platform.resultLoad||platform.changesLoad)return;
  const source={snapshot_id:platform.loaded,revision:snapshot.revision},epoch=agentView.epoch;
  agentView.posting=true;agentError('');agentControls();
  if(!$('agentDialog').open)$('agentDialog').showModal();
  try{
    await loadAgentHistory();if(epoch!==agentView.epoch)return;
    const active=agentView.sessions.find(agentBusy);
    if(active){await selectAgentSession(active);return;}
    const config=await api('/api/agent/settings');if(epoch!==agentView.epoch)return;
    if(!config.value.model||!config.value.endpoint)throw Error('请先在“模型设置”配置接口和模型，再生成报告。');
    const session=await api('/api/agent/reports',{method:'POST',body:JSON.stringify(source)});
    if(epoch!==agentView.epoch)return;
    await selectAgentSession(session);
  }catch(error){if(epoch===agentView.epoch)agentError(error);}
  finally{if(epoch===agentView.epoch){agentView.posting=false;agentControls();}}
}
async function postAgentAction(action,body){
  if(!agentAllowed()||!agentView.session||agentView.posting)return;
  const epoch=agentView.epoch,selection=agentView.selection;
  const current=()=>epoch===agentView.epoch&&selection===agentView.selection;
  agentView.posting=true;agentError('');agentControls();clearTimeout(agentView.timer);
  try{
    const result=await api(`/api/agent/sessions/${agentView.session.id}/${action}`,{method:'POST',body:JSON.stringify(body)});
    if(!current())return;
    if(action==='messages'){rememberAgentSession(result);$('agentQuestion').value='';}
    else agentView.session.status='cancelling';
    renderAgentSession();await readAgentSession();
  }catch(error){if(current())agentError(error);}
  finally{if(current()){agentView.posting=false;agentControls();scheduleAgentPoll();}}
}
function downloadAgentReport(){
  const s=agentView.session;if(!s||!agentView.messages.some(m=>m.role==='assistant'))return;
  const parts=[`# 空间分析记录\n\n生成时间：${dateTime(s.created_at)}\n\n模型：${s.model}\n\n扫描记录：${s.snapshot_id||'已删除'}\n\n状态：${agentStatusNames[s.status]||s.status}`];
  let answered=false;
  for(const m of agentView.messages){
    if(m.role==='assistant'){parts.push(m.content);answered=true;}
    else if(m.role==='user'&&answered)parts.push('## 追问\n\n'+m.content);
    else if(m.role==='status')parts.push(m.content);
  }
  const url=URL.createObjectURL(new Blob([parts.join('\n\n---\n\n')+'\n'],{type:'text/markdown;charset=utf-8'}));
  const link=document.createElement('a');link.href=url;link.download=`空间消耗报告-${s.id.slice(0,8)}.md`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
}
window.AgentUI={controls:agentControls,reset(){
  clearTimeout(agentView.timer);
  Object.assign(agentView,{epoch:agentView.epoch+1,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:''});
  if($('agentDialog').open)$('agentDialog').close();
  $('agentReports').innerHTML='';$('agentActivityLog').innerHTML='';$('agentActivity').hidden=true;$('agentQuestion').value='';
  agentError('');renderAgentHistory();renderAgentSession();
}};
$('generateReport').addEventListener('click',generateDiskReport);
$('viewReports').addEventListener('click',openAgentReports);
$('closeAgent').addEventListener('click',()=>$('agentDialog').close());
$('agentDialog').addEventListener('close',()=>clearTimeout(agentView.timer));
$('reloadReports').addEventListener('click',openAgentReports);
$('agentHistory').addEventListener('change',()=>{const session=agentView.sessions.find(s=>s.id===$('agentHistory').value);if(session)selectAgentSession(session);});
$('stopAgent').addEventListener('click',()=>postAgentAction('cancel',{}));
$('downloadReport').addEventListener('click',downloadAgentReport);
$('agentModelSettings').addEventListener('click',()=>{$('agentDialog').close();showPage('agent-settings');});
$('agentFollowup').addEventListener('submit',e=>{e.preventDefault();const message=$('agentQuestion').value.trim();if(message&&!agentBusy(agentView.session)&&!agentView.reading&&agentView.session?.snapshot_id)postAgentAction('messages',{message});});
document.querySelectorAll('[data-agent-question]').forEach(button=>button.addEventListener('click',()=>{$('agentQuestion').value=button.dataset.agentQuestion;$('agentQuestion').focus();}));
