'use strict';
const agentView={epoch:0,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:'',stream:null,tab:'conversation',requests:new Map(),tools:new Map(),seen:new Set()};
const agentStatusNames={queued:'等待分析',scanning:'补查目录中',running:'正在生成分析',cancelling:'正在停止',completed:'分析完成',failed:'分析失败',cancelled:'分析已停止',interrupted:'分析已中断'};
const agentToolNames={get_overview:'核对整体占用',list_containers:'查看容器排行',list_owners:'查看用户归属',get_container:'核对容器存储来源',get_directory:'读取目录明细',scan_directory:'补查大目录与文件'};
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
  if(agentBusy(session)){
    const tool=[...agentView.tools.values()].filter(t=>!t.status).at(-1),request=[...agentView.requests.values()].at(-1);
    if(tool){const args=agentJSON(tool.arguments);$('agentRunStatus').textContent+=` · ${agentToolNames[tool.name]||tool.name}${args.path?' · '+args.path:args.container?' · '+args.container:''}`;}
    else if(request&&!request.status){$('agentRunStatus').textContent+=` · 第 ${request.round} 次请求 · ${request.text?'正在输出分析':request.summary?'正在生成思考摘要':request.drafts.size?'正在准备工具调用':'等待模型返回公开内容'}`;}
  }
  if(job?.progress){const p=job.progress;$('agentRunStatus').textContent+=` · 已遍历 ${(p.entries||0).toLocaleString('zh-CN')} 项${p.path?' · '+p.path:''}`;}
  $('agentReportSource').textContent=session?`模型 ${session.model} · ${dateTime(session.created_at)} · ${session.snapshot_id?'扫描记录 '+session.snapshot_id:'扫描记录已不存在，请重新生成报告'}${session.snapshot_id&&session.snapshot_id!==platform.loaded?' · 与当前页面所查看的记录不同':''}`:'';
  $('agentEmpty').hidden=agentView.messages.length>0;
  renderAgentMetrics();
  renderAgentPending();
  if(session?.error)agentError(session.error);
  agentControls();
}
function renderAgentHistory(){
  const options=agentView.sessions.map(s=>`<option value="${esc(s.id)}">${esc(dateTime(s.created_at)+' · '+s.title+' · '+(agentStatusNames[s.status]||s.status))}</option>`).join('')||'<option value="">暂无分析记录</option>';
  if($('agentHistory').innerHTML!==options)$('agentHistory').innerHTML=options;
  $('agentHistory').value=agentView.session?.id||'';
}
function rememberAgentSession(session){
  agentView.session=session;
  agentView.sessions=[session,...agentView.sessions.filter(s=>s.id!==session.id)].sort((a,b)=>b.updated_at-a.updated_at);
  renderAgentHistory();
}
function agentJSON(text){try{return JSON.parse(text);}catch{return {};}}
function agentPretty(value){return typeof value==='string'?(()=>{try{return JSON.stringify(JSON.parse(value),null,2);}catch{return value;}})():JSON.stringify(value,null,2);}
function agentTokens(value){return Number.isFinite(value)?value.toLocaleString('zh-CN'):'未提供';}
function agentDuration(ms){return ms<1000?`${ms} ms`:`${(ms/1000).toFixed(1)} 秒`;}
function agentDetails(label,value){return `<details><summary>${esc(label)}</summary><pre>${esc(agentPretty(value))}</pre></details>`;}
function agentContextItem(item,index){
  const names={system:'系统指令与观察数据',developer:'开发者指令',user:'用户消息',assistant:'助手消息',tool:'工具结果',function_call:'工具调用',function_call_output:'工具结果',reasoning:'推理状态'};
  const role=item.role||item.type||'消息',content=item.content??item.output??item.arguments??item.note;
  let readable=content;
  if(Array.isArray(content))readable=content.map(part=>part.text||part.refusal||agentPretty(part)).join('\n');
  return `<details><summary>${index+1}. ${esc(names[role]||role)}${item.name?' · '+esc(item.name):''}</summary>${readable!=null?`<pre>${esc(agentPretty(readable))}</pre>`:''}${agentDetails('查看该项完整结构',item)}</details>`;
}
function agentSetTab(tab){
  agentView.tab=tab;$('agentReports').hidden=tab!=='report';$('agentActivity').hidden=tab!=='conversation'||!agentView.messages.length;
  $('agentConversationTab').setAttribute('aria-pressed',String(tab==='conversation'));$('agentReportTab').setAttribute('aria-pressed',String(tab==='report'));
}
function agentUsageText(usage){
  if(!usage)return 'Token 用量：接口未提供';
  return `输入 ${agentTokens(usage.input_tokens)} · 输出 ${agentTokens(usage.output_tokens)} · 合计 ${agentTokens(usage.total_tokens)} token · 缓存输入 ${agentTokens(usage.cached_tokens)} · 推理 ${agentTokens(usage.reasoning_tokens)}`;
}
function renderAgentMetrics(){
  const requests=[...agentView.requests.values()],last=requests.at(-1),reported=requests.filter(r=>r.usage);
  $('agentMetrics').hidden=!last;
  if(!last)return;
  const sum=key=>reported.reduce((n,r)=>n+r.usage[key],0),tools=agentView.messages.filter(m=>m.role==='tool_start').length;
  const metrics=`<div><span>本次上下文</span><strong>${last.context_items} 项 · ${(last.context_bytes/1024).toFixed(1)} KiB</strong><small>${last.usage?`输入 ${agentTokens(last.usage.input_tokens)} token`:last.status?'接口未提供输入 token':'输入 token 等待接口统计'}</small></div><div><span>本次输出</span><strong>${last.usage?agentTokens(last.usage.output_tokens)+' token':'尚无统计'}</strong><small>${last.usage?`推理 ${agentTokens(last.usage.reasoning_tokens)} · 包含在输出内`:'通常在本次请求结束时返回'}</small></div><div><span>会话累计用量</span><strong>${reported.length?agentTokens(sum('total_tokens'))+' token':'尚无统计'}</strong><small>已统计 ${reported.length}/${requests.length} 次请求${reported.length?` · 输入 ${agentTokens(sum('input_tokens'))} / 输出 ${agentTokens(sum('output_tokens'))}`:''}</small></div><div><span>执行步骤</span><strong>${requests.length} 次请求 · ${tools} 次工具</strong><small>本轮第 ${last.round} 次模型请求</small></div>`;
  if($('agentMetrics').innerHTML!==metrics)$('agentMetrics').innerHTML=metrics;
}
function renderAgentRequest(request){
  const busy=agentBusy(agentView.session),status=request.status||(busy?'running':'interrupted');
  const statusText={running:'等待模型输出',completed:'模型响应完成',failed:'响应失败 · 内容未完成',cancelled:'已停止 · 内容未完成',interrupted:'已中断 · 内容未完成'}[status]||status;
  $('agentRequestState-'+request.node).textContent=(status==='running'?(request.text?'正在输出文字':request.summary?'正在接收思考摘要':request.drafts.size?'正在生成工具参数':statusText):statusText)+(request.duration_ms!=null?' · '+agentDuration(request.duration_ms):'');
  const output=$('agentRequestOutput-'+request.node);
  // Public prose is rendered independently of the context so expanded details
  // and the user's position survive every streaming update.
  if(request.renderedText!==request.text||request.renderedSummary!==request.summary){
  output.innerHTML=(request.summary?`<div class="agent-summary"><strong>思考摘要 · 接口公开内容</strong><div>${reportMarkdown(request.summary)}</div></div>`:'')+(request.text?`<div class="agent-model-text">${reportMarkdown(request.text)}</div>`:'');
  request.renderedText=request.text;request.renderedSummary=request.summary;
  }
  $('agentRequestDraft-'+request.node).textContent=[...request.drafts.values()].map(d=>`准备调用 ${agentToolNames[d.name]||d.name||'工具'}\n${d.arguments||'等待参数…'}`).join('\n\n');
  $('agentRequestDraft-'+request.node).hidden=!!request.status||!request.drafts.size;
  $('agentRequestUsage-'+request.node).textContent=(request.status||!busy)?agentUsageText(request.usage):'Token 用量等待接口返回；上下文字节数不等于 token 数。';
  $('agentRequestError-'+request.node).textContent=request.error||'';
}
function renderAgentTool(tool){
  const status=tool.status||(agentBusy(agentView.session)?'running':'interrupted');
  $('agentToolState-'+tool.node).textContent=({running:'执行中',completed:'完成',failed:'失败',cancelled:'已停止',interrupted:'已中断'}[status]||status)+(tool.duration_ms!=null?' · '+agentDuration(tool.duration_ms):'');
  if(tool.result!=null)$('agentToolResult-'+tool.node).textContent=agentPretty(tool.result);
}
function renderAgentPending(){
  for(const request of agentView.requests.values())if(!request.status)renderAgentRequest(request);
  for(const tool of agentView.tools.values())if(!tool.status)renderAgentTool(tool);
}
function interruptAgentPending(){
  for(const request of agentView.requests.values())if(!request.status){request.status='interrupted';renderAgentRequest(request);}
  for(const tool of agentView.tools.values())if(!tool.status){tool.status='interrupted';renderAgentTool(tool);}
}
function appendAgentMessages(messages){
  const timeline=$('agentActivityLog'),follow=timeline.scrollHeight-timeline.scrollTop-timeline.clientHeight<80;
  for(const m of messages){
    if(agentView.seen.has(m.id))continue;
    agentView.seen.add(m.id);agentView.messages.push(m);
    const log=$('agentActivityLog'),time=m.created_at?dateTime(m.created_at):'',data=agentJSON(m.content);
    if(m.role==='model_request'){
      const request={...data,node:m.id,text:'',summary:'',drafts:new Map()};agentView.requests.set(data.request_id,request);
      log.insertAdjacentHTML('beforeend',`<article class="agent-step agent-model-step"><header><strong>模型请求 ${agentView.requests.size} <small>本轮 ${data.round}</small></strong><time>${esc(time)}</time></header><p id="agentRequestState-${m.id}" class="agent-step-state"></p><details class="agent-context"><summary>查看本次上下文 · ${data.context_items} 项 · ${esc(data.model)} · ${esc(data.protocol)}</summary><p>按发送顺序排列。追问使用最新概览及最近 24 条用户/助手消息，本轮工具结果逐次加入；内部推理状态仅在内存传递。上下文大小 ${(data.context_bytes/1024).toFixed(1)} KiB，请求正文 ${(data.request_bytes/1024).toFixed(1)} KiB，均非 token 数；模型窗口上限未提供。</p>${data.context.map(agentContextItem).join('')}${agentDetails('可用工具定义',data.tools||[])}${data.reasoning?agentDetails('推理配置',data.reasoning):''}</details><div id="agentRequestOutput-${m.id}"></div><pre id="agentRequestDraft-${m.id}" hidden></pre><p id="agentRequestUsage-${m.id}" class="agent-token-note"></p><p id="agentRequestError-${m.id}" class="error-text"></p></article>`);
      renderAgentRequest(request);
    }else if(m.role==='model_delta'){
      const request=agentView.requests.get(data.request_id);if(!request)continue;
      for(const delta of data.deltas){if(delta.kind==='text')request.text+=delta.text||'';else if(delta.kind==='summary')request.summary+=delta.text||'';else if(delta.kind==='tool')request.drafts.set(delta.index,delta);}
      renderAgentRequest(request);
    }else if(m.role==='model_response'){
      const request=agentView.requests.get(data.request_id);if(!request)continue;
      Object.assign(request,{status:data.status,error:data.error,usage:data.usage,duration_ms:data.duration_ms});
      if(data.text)request.text=data.text;if(data.summary)request.summary=data.summary;renderAgentRequest(request);
    }else if(m.role==='tool_start'){
      const tool={...data,node:m.id,name:m.tool_name};agentView.tools.set(data.call_id,tool);
      log.insertAdjacentHTML('beforeend',`<article class="agent-step agent-tool-step"><header><strong>${esc(agentToolNames[m.tool_name]||m.tool_name)} <code>${esc(m.tool_name)}</code></strong><time>${esc(time)}</time></header><p id="agentToolState-${m.id}" class="agent-step-state"></p>${agentDetails('调用参数',data.arguments)}<details><summary>工具结果与统计证据</summary><pre id="agentToolResult-${m.id}">等待结果…</pre></details></article>`);renderAgentTool(tool);
    }else if(m.role==='tool_end'){
      const tool=agentView.tools.get(data.call_id);if(tool){Object.assign(tool,data);renderAgentTool(tool);}
    }else if(m.role==='group_report'){
      log.insertAdjacentHTML('beforeend',`<details class="agent-step"><summary>${esc(m.content.split('\n')[0].replace(/^#+\s*/,''))} · 分组结果</summary>${reportMarkdown(m.content)}</details>`);
    }else if(m.role==='assistant'){
      $('agentReports').insertAdjacentHTML('beforeend',`<article class="agent-report"><div class="agent-answer-time">${esc(time)}</div>${reportMarkdown(m.content)}</article>`);
      log.insertAdjacentHTML('beforeend','<p class="agent-report-ready">本轮完整回复已保存，可切换到“完整报告”阅读或导出。</p>');
    }else if(m.role==='user'){
      // A followup begins a new turn. Unfinished steps from an interrupted turn
      // must not start looking active again when the session returns to running.
      interruptAgentPending();
      log.insertAdjacentHTML('beforeend',`<article class="agent-step agent-user-step"><header><strong>用户问题 / 报告任务</strong><time>${esc(time)}</time></header>${m.content.length>600?agentDetails(m.content.split('\n')[0],m.content):`<p>${esc(m.content)}</p>`}</article>`);
      if(agentView.messages.some(v=>v.role==='assistant'))$('agentReports').insertAdjacentHTML('beforeend',`<p class="agent-user-question">${esc(m.content)}</p>`);
    }else if(m.role==='status'){
      log.insertAdjacentHTML('beforeend',`<p class="agent-system-event"><span>进度</span> ${esc(m.content)}</p>`);
    }else if(m.role==='tool_result'){
      log.insertAdjacentHTML('beforeend',`<article class="agent-step agent-tool-step"><header><strong>预载证据 · ${esc(agentToolNames[m.tool_name]||m.tool_name)}</strong><time>${esc(time)}</time></header>${agentDetails('发送给模型的统计证据',m.content)}</article>`);
    }
  }
  $('agentActivityCount').textContent=`· ${agentView.messages.filter(m=>m.role==='tool_start').length} 次工具`;
  renderAgentMetrics();agentSetTab(agentView.tab);
  if(follow)timeline.scrollTop=timeline.scrollHeight;
}
function closeAgentStream(){if(agentView.stream){agentView.stream.close();agentView.stream=null;}}
function scheduleAgentPoll(){
  clearTimeout(agentView.timer);
  if(!agentAllowed()||!$('agentDialog').open||!agentBusy(agentView.session)){closeAgentStream();$('agentConnection').textContent='';return;}
  if(!agentView.stream)agentView.timer=setTimeout(()=>readAgentSession(),1500);
}
function connectAgentStream(){
  if(agentView.stream||!agentAllowed()||!$('agentDialog').open||!agentBusy(agentView.session))return;
  const epoch=agentView.epoch,selection=agentView.selection;
  const source=new EventSource(`/api/agent/sessions/${agentView.session.id}/events?after=${agentView.cursor}`);agentView.stream=source;
  const current=()=>epoch===agentView.epoch&&selection===agentView.selection&&agentView.stream===source&&agentAllowed();
  $('agentConnection').textContent='正在连接实时进度';
  source.addEventListener('open',()=>{if(current())$('agentConnection').textContent='实时连接已建立';});
  source.addEventListener('session',event=>{
    if(!current())return;
    try{
      const result=JSON.parse(event.data);rememberAgentSession(result.session);appendAgentMessages(result.messages);agentView.cursor=result.next_after;
      agentError('');renderAgentSession(result.active_job);
      if(!agentBusy(result.session)&&!result.has_more){closeAgentStream();$('agentConnection').textContent='全部事件已接收';}
    }catch{closeAgentStream();$('agentConnection').textContent='正在恢复进度';scheduleAgentPoll();}
  });
  source.addEventListener('error',()=>{if(current()){closeAgentStream();$('agentConnection').textContent='连接中断，正在重连';scheduleAgentPoll();}});
}
async function readAgentSession(){
  if(!agentAllowed()||!agentView.session||agentView.reading)return;
  closeAgentStream();
  const epoch=agentView.epoch,selection=agentView.selection,id=agentView.session.id;
  const current=()=>epoch===agentView.epoch&&selection===agentView.selection&&agentAllowed();
  agentView.reading=true;agentControls();
  try{
    let result;
    do{
      result=await api(`/api/agent/sessions/${id}?after=${agentView.cursor}`);
      if(!current())return;
      rememberAgentSession(result.session);appendAgentMessages(result.messages);agentView.cursor=result.next_after;
    }while(result.has_more);
    agentError('');renderAgentSession(result.active_job);
  }catch(error){if(current())agentError(error);}
  finally{if(current()){agentView.reading=false;agentControls();connectAgentStream();scheduleAgentPoll();}}
}
async function selectAgentSession(session){
  clearTimeout(agentView.timer);closeAgentStream();agentView.selection++;agentView.cursor=0;agentView.messages=[];agentView.reading=false;agentView.requests.clear();agentView.tools.clear();agentView.seen.clear();agentView.tab='conversation';
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
  agentView.posting=true;agentError('');agentControls();clearTimeout(agentView.timer);closeAgentStream();
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
  clearTimeout(agentView.timer);closeAgentStream();
  Object.assign(agentView,{epoch:agentView.epoch+1,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:'',stream:null,tab:'conversation',requests:new Map(),tools:new Map(),seen:new Set()});
  if($('agentDialog').open)$('agentDialog').close();
  $('agentReports').innerHTML='';$('agentActivityLog').innerHTML='';$('agentActivity').hidden=true;$('agentQuestion').value='';
  agentError('');renderAgentHistory();renderAgentSession();
}};
$('generateReport').addEventListener('click',generateDiskReport);
$('viewReports').addEventListener('click',openAgentReports);
$('closeAgent').addEventListener('click',()=>$('agentDialog').close());
$('agentDialog').addEventListener('close',()=>{clearTimeout(agentView.timer);closeAgentStream();});
$('agentConversationTab').addEventListener('click',()=>agentSetTab('conversation'));
$('agentReportTab').addEventListener('click',()=>agentSetTab('report'));
$('agentJumpLatest').addEventListener('click',()=>{agentSetTab('conversation');$('agentActivityLog').scrollTop=$('agentActivityLog').scrollHeight;});
$('reloadReports').addEventListener('click',openAgentReports);
$('agentHistory').addEventListener('change',()=>{const session=agentView.sessions.find(s=>s.id===$('agentHistory').value);if(session)selectAgentSession(session);});
$('stopAgent').addEventListener('click',()=>postAgentAction('cancel',{}));
$('downloadReport').addEventListener('click',downloadAgentReport);
$('agentModelSettings').addEventListener('click',()=>{$('agentDialog').close();showPage('agent-settings');});
$('agentFollowup').addEventListener('submit',e=>{e.preventDefault();const message=$('agentQuestion').value.trim();if(message&&!agentBusy(agentView.session)&&!agentView.reading&&agentView.session?.snapshot_id)postAgentAction('messages',{message});});
document.querySelectorAll('[data-agent-question]').forEach(button=>button.addEventListener('click',()=>{$('agentQuestion').value=button.dataset.agentQuestion;$('agentQuestion').focus();}));
