'use strict';
const agentView={reportScope:'container',nextConcurrency:{host:3,container:3},groups:new Map(),concurrency:null,scope:'main',followActive:true,scopeCounts:new Map(),scrollPositions:new Map(),epoch:0,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:'',stream:null,tab:'conversation',requests:new Map(),tools:new Map(),seen:new Set()};
const agentStatusNames={queued:'等待分析',scanning:'补查目录中',running:'正在生成分析',cancelling:'正在停止',completed:'分析完成',failed:'分析失败',cancelled:'分析已停止',interrupted:'分析已中断'};
const agentToolNames={get_overview:'核对整体占用',list_containers:'查看容器排行',list_owners:'查看用户归属',get_container:'核对容器存储来源',get_directory:'读取目录明细',scan_directory:'补查大目录与文件',get_host_directory:'读取 Host 目录明细'};
const agentBusy=session=>session && ['queued','scanning','running','cancelling'].includes(session.status);
const agentAllowed=()=>platform.user?.role==='admin';
const agentScopeName=scope=>scope==='host'?'Host':'容器';
const agentScopeSessions=()=>agentView.sessions.filter(session=>session.report_scope===agentView.reportScope);
const hostReportReady=()=>!!snapshot?.resources?.some(resource=>resource.kinds?.includes('host'));
const agentFollowupExamples={
  host:[
    ['重点目录','继续梳理当前 Host 扫描范围内占用最大的目录和文件，按实际占用降序列出，并说明未扫描或归属不明的部分。'],
    ['按时间整理','补查重点 Host 目录的文件日期，按 90 天以内、90–180 天、180 天及以上 mtime 分组，各组内按实际占用降序；不要把 mtime 或 ctime 当创建时间。'],
    ['清理候选清单','整理 Host 范围内可重建缓存、日志和临时文件候选，列出精确路径、候选占用、证据和需要核实的条件，不执行清理。']
  ],
  container:[
    ['数据集与模型','继续梳理可写层的数据集和模型，按大小降序列出；明确已覆盖范围与未知项。'],
    ['按时间整理','补查重点数据资产的文件日期，按 90 天以内、90–180 天、180 天及以上 mtime 分组，各组内按实际占用降序；不要把 mtime 或 ctime 当创建时间。'],
    ['清理候选清单','整理可重建缓存、日志和编辑器检查点候选，列出负责人、精确路径、候选占用、证据和需要核实的条件，不执行清理。']
  ]
};

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
  const hostReady=hostReportReady();
  $('agentConcurrency').value=agentView.nextConcurrency[agentView.reportScope];
  $('agentConcurrency').disabled=pending||busy;
  $('reportConcurrency').disabled=pending||busy;
  $('hostReportConcurrency').disabled=pending||busy;
  $('startAgentReport').disabled=busy||pending||!snapshot||!platform.loaded||!!platform.resultLoad||!!platform.changesLoad||(agentView.reportScope==='host'&&!hostReady);
  $('generateReport').disabled=!agentAllowed()||!snapshot||!platform.loaded||!!platform.resultLoad||!!platform.changesLoad||agentView.posting;
  $('generateHostReport').disabled=$('generateReport').disabled||!hostReady;
  const source=snapshot&&platform.loaded?`基于当前${platform.followLatest?'':'历史'}扫描 · ${snapshot.host} · ${snapshot.finished_at}；补查结果同步到空间用量。`:'';
  $('agentSourceHint').textContent=source||'完成扫描后，即可基于当前记录生成容器报告。';
  $('hostAgentSourceHint').textContent=source&&!hostReady?'当前扫描未包含 Host 目录，请在扫描配置中添加宿主机目录并重新扫描。':source||'完成扫描后，即可基于当前记录生成 Host 报告。';
  $('agentHostScope').setAttribute('aria-pressed',String(agentView.reportScope==='host'));
  $('agentContainerScope').setAttribute('aria-pressed',String(agentView.reportScope==='container'));
  $('agentTitle').textContent=agentScopeName(agentView.reportScope)+'空间诊断';
  $('agentGroupLabel').textContent=agentView.reportScope==='host'?'分组 Agent · Host 目录':'分组 Agent · 每组最多 4 个容器';
  $('agentRunHint').textContent=agentView.reportScope==='host'?'按 Host 目录分组分析，空位自动补充下一组。':'每组独立分析，空位自动补充下一组。';
  $('agentQuestion').placeholder=agentView.reportScope==='host'?'例如：继续查看 Host 扫描范围内最大的目录有哪些文件':'例如：继续查看最大的可写层中有哪些数据集';
  document.querySelectorAll('[data-agent-question]').forEach((button,index)=>{
    const [label,prompt]=agentFollowupExamples[agentView.reportScope][index];
    button.textContent=label;button.dataset.agentQuestion=prompt;
  });
  $('agentHistory').disabled=pending;$('reloadReports').disabled=pending;
  $('stopAgent').hidden=!busy;$('stopAgent').disabled=agentView.posting||agentView.session?.status==='cancelling';
  $('downloadReport').disabled=agentView.reading||!agentView.messages.some(m=>m.role==='assistant');
  $('agentFollowup').hidden=!agentView.session||(agentView.tab==='conversation'&&agentView.scope!=='main');
  $('sendAgentQuestion').disabled=pending||busy||!agentView.session?.snapshot_id;
  $('agentQuestion').disabled=pending||busy||!agentView.session?.snapshot_id;
  document.querySelectorAll('[data-agent-question]').forEach(b=>b.disabled=$('agentQuestion').disabled);
  renderAgentRetry();
}
function agentRetryRequests(){
  const latest=new Map();
  for(const request of agentView.requests.values())latest.set(request.scope,request);
  return [...agentView.groups.values()].filter(group=>group.status==='failed').map(group=>latest.get(group.id)).filter(request=>request?.status==='failed');
}
function renderAgentRetry(){
  const requests=agentRetryRequests(),button=$('agentRetry');
  button.hidden=!requests.length;
  button.disabled=!requests.length||!agentAllowed()||agentView.posting||agentView.reading||agentBusy(agentView.session)||!agentView.session?.snapshot_id;
  button.textContent=requests.length>1?`重试失败 Agent（${requests.length}）`:'重试失败请求';
  button.title=requests.length?`按原并行上限 ${agentView.concurrency} 恢复 ${requests.length} 个失败 Agent，各自从最近失败请求继续${requests.length===1?`（第 ${requests[0].round} 轮）`:''}，保留已完成的工具结果和分组`:'';
}
function retryAgentGroups(){
  const requests=agentRetryRequests();
  if(!requests.length||$('agentRetry').disabled)return;
  return postAgentAction('retry',{requests:requests.map(request=>({group_id:request.scope,request_id:request.request_id}))});
}
function agentError(error){agentView.error=error?error.message||String(error):'';$('agentError').textContent=agentView.error;}
function renderAgentSession(job){
  const session=agentView.session;
  $('agentRunStatus').textContent=session?(agentStatusNames[session.status]||session.status):'尚未生成报告';
  if(agentBusy(session)){
    const tool=[...agentView.tools.values()].filter(t=>!t.status).at(-1),request=[...agentView.requests.values()].at(-1);
    if(agentView.groups.size){$('agentRunStatus').textContent+=` · ${agentRunningGroups().length} 个 Agent 分析中`;}
    else if(tool){const args=agentJSON(tool.arguments);$('agentRunStatus').textContent+=` · ${agentToolNames[tool.name]||tool.name}${args.path?' · '+args.path:args.container?' · '+args.container:''}`;}
    else if(request&&!request.status){$('agentRunStatus').textContent+=` · ${request.drafts.size?'正在准备查询':request.text&&!agentProse(request.text)?'正在整理结果':request.text?'正在回复':'正在思考'}`;}
  }
  if(job?.progress){const p=job.progress;$('agentRunStatus').textContent+=` · 已遍历 ${(p.entries||0).toLocaleString('zh-CN')} 项${p.path?' · '+p.path:''}`;}
  $('agentReportSource').textContent=session?`模型 ${session.model} · ${dateTime(session.created_at)} · ${session.snapshot_id?'扫描记录 '+session.snapshot_id:'扫描记录已不存在，请重新生成报告'}${session.snapshot_id&&session.snapshot_id!==platform.loaded?' · 与当前页面所查看的记录不同':''}`:'';
  $('agentEmpty').hidden=agentView.messages.length>0;
  renderAgentPending();
  renderAgentSidebar();
  if(session?.error)agentError(session.error);
  agentControls();
}
function renderAgentHistory(){
  const options=agentScopeSessions().map(s=>`<option value="${esc(s.id)}">${esc(dateTime(s.created_at)+' · '+s.title+' · '+(agentStatusNames[s.status]||s.status))}</option>`).join('')||'<option value="">暂无分析记录</option>';
  if($('agentHistory').innerHTML!==options)$('agentHistory').innerHTML=options;
  $('agentHistory').value=agentView.session?.id||'';
}
function rememberAgentSession(session){
  if(session.report_scope!==agentView.reportScope)throw Error('报告类型与当前分析栏目不匹配');
  agentView.session=session;
  agentView.sessions=[session,...agentView.sessions.filter(s=>s.id!==session.id)].sort((a,b)=>b.updated_at-a.updated_at);
  renderAgentHistory();
}
function agentJSON(text){try{return JSON.parse(text)||{};}catch{return {};}}
function agentStepStatus(status){
  if(status)return status;
  if(agentBusy(agentView.session))return 'running';
  return ['failed','cancelled'].includes(agentView.session?.status)?agentView.session.status:'interrupted';
}
function agentDuration(ms){return ms<1000?`${ms} ms`:`${(ms/1000).toFixed(1)} 秒`;}
// Structured group results are published as readable group_report messages only
// after validation. Never expose partial JSON (including an unfinished fence).
function agentProse(text){
  const value=text.trim();
  if(value.startsWith('{')||/^\[\s*(?:[\[{"]|$)/.test(value))return '';
  if(/^```(?:json\b|\s*[\[{])/i.test(value)||['`','``','```','```j','```js','```jso','```json'].includes(value.toLowerCase()))return '';
  return text;
}
function agentBubble(id,time,body,kind='assistant',scope='main'){
  return `<article ${id?`id="${id}"`:''} class="agent-message agent-${kind}-message"><div class="agent-avatar" aria-hidden="true">${kind==='user'?'你':'A'}</div><div class="agent-bubble"><header><strong>${kind==='user'?'你':scope!=='main'?'Agent '+esc(agentView.groups.get(scope).number):'Agent'}</strong><time>${esc(time)}</time></header>${body}</div></article>`;
}
function agentRunningGroups(){return [...agentView.groups.values()].filter(group=>group.status==='running');}
function followRunningAgent(){
  if(!agentView.followActive||agentView.groups.get(agentView.scope)?.status==='running')return;
  const group=agentRunningGroups()[0];if(group)selectAgentScope(group.id,true);
}
function agentGroupStatus(group){
  if(group.status==='queued')return agentBusy(agentView.session)?'queued':'not_started';
  if(group.status==='running')return agentStepStatus();
  return group.status;
}
const agentGroupStatusNames={queued:'等待中',running:'分析中',completed:'已完成',failed:'失败',cancelled:'已停止',interrupted:'已中断',not_started:'未开始'};
function agentGroupTargets(group){
  const targets=agentView.reportScope==='host'?group.directories:group.containers;
  return Array.isArray(targets)?targets.map(item=>typeof item==='string'?item:item.path||item.name||item.id):[];
}
function renderAgentSidebar(){
  const groups=[...agentView.groups.values()];
  const html=groups.map(group=>{
    const status=agentGroupStatus(group),selected=agentView.tab==='conversation'&&agentView.scope===group.id;
    const targets=agentGroupTargets(group);
    return `<button type="button" class="agent-group-button" data-agent-group="${esc(group.id)}" aria-pressed="${selected}"><span class="agent-group-heading"><strong>Agent ${group.number}</strong><span class="agent-group-status" data-status="${status}">${agentGroupStatusNames[status]}</span></span><small>${targets.length} 个${agentView.reportScope==='host'?'目录':'容器'}</small><span class="agent-group-containers">${targets.map(target=>`<span title="${esc(target)}">${esc(target)}</span>`).join('')}</span></button>`;
  }).join('');
  if($('agentGroupList').innerHTML!==html)$('agentGroupList').innerHTML=html;
  $('agentGroupSection').hidden=!groups.length;
  $('agentConcurrencyStatus').textContent=groups.length?`并行上限 ${agentView.concurrency} · 分析中 ${agentBusy(agentView.session)?agentRunningGroups().length:0} · 已完成 ${groups.filter(g=>g.status==='completed').length}/${groups.length}`:'';
  $('agentFollowCurrent').hidden=!groups.length||!agentBusy(agentView.session);
  $('agentFollowCurrent').setAttribute('aria-pressed',String(agentView.followActive));
  $('agentConversationTab').setAttribute('aria-pressed',String(agentView.tab==='conversation'&&agentView.scope==='main'));
  $('agentReportTab').setAttribute('aria-pressed',String(agentView.tab==='report'));
  $('agentReportReady').textContent=agentView.messages.some(m=>m.role==='assistant')?'可查看':agentBusy(agentView.session)?'待生成':'未生成';
  const group=agentView.groups.get(agentView.scope);
  $('agentScopeTitle').textContent=agentView.tab==='report'?'完整报告':group?`Agent ${group.number}`:'总览与追问';
  $('agentScopeHint').textContent=agentView.tab==='report'?'各组分析结果汇总':group?`${agentGroupStatusNames[agentGroupStatus(group)]} · ${agentGroupTargets(group).join('、')}`:'查看任务、最终回复，或继续追问';
  const tools=[...agentView.tools.values()].filter(t=>t.scope===agentView.scope).length;
  $('agentActivityCount').textContent=tools?`${tools} 次工具调用`:'';
  const hasContent=agentView.scopeCounts.get(agentView.scope)>0;
  $('agentScopeEmpty').hidden=agentView.tab==='report'?agentView.messages.some(m=>m.role==='assistant'):hasContent;
  $('agentScopeEmpty').textContent=agentView.tab==='report'?(agentBusy(agentView.session)?'分析完成后，完整报告会显示在这里。':'未能生成完整报告，可在左侧查看已完成的分组结果。'):group?(agentGroupStatus(group)==='queued'?'等待空闲名额，任一 Agent 结束后自动开始。':agentGroupStatus(group)==='not_started'?'本次分析已结束，这个 Agent 尚未开始。':agentGroupStatus(group)==='running'?`正在准备分析这些${agentView.reportScope==='host'?'目录':'容器'}…`:`此 Agent ${agentGroupStatusNames[agentGroupStatus(group)]}，尚未收到可展示的回复。`):'准备开始诊断…';
  $('agentWorkspace').hidden=!agentView.session;
  renderAgentRetry();
}
function agentFilterConversation(){
  document.querySelectorAll('#agentActivityLog > [data-agent-scope]').forEach(node=>{node.hidden=node.dataset.agentScope!==agentView.scope;});
}
function selectAgentScope(scope,follow=false){
  if(agentView.tab==='conversation')agentView.scrollPositions.set(agentView.scope,$('agentActivityLog').scrollTop);
  agentView.scope=scope;agentView.followActive=follow;
  agentSetTab('conversation');
  $('agentActivityLog').scrollTop=agentView.scrollPositions.get(scope)??$('agentActivityLog').scrollHeight;
}
function resetAgentGroups(){
  Object.assign(agentView,{groups:new Map(),concurrency:null,scope:'main',followActive:true,scopeCounts:new Map(),scrollPositions:new Map()});
  $('agentGroupList').innerHTML='';
}
function agentSetTab(tab){
  if(tab==='report'&&agentView.tab==='conversation')agentView.scrollPositions.set(agentView.scope,$('agentActivityLog').scrollTop);
  agentView.tab=tab;$('agentReports').hidden=tab!=='report';$('agentActivity').hidden=tab!=='conversation'||!agentView.messages.length;
  agentFilterConversation();renderAgentSidebar();agentControls();
}
function renderAgentRequest(request){
  const status=agentStepStatus(request.status),text=agentProse(request.text),thinking=request.protocol==='completions'?request.reasoning:request.summary;
  const state=$('agentRequestState-'+request.node);
  const names=[...request.drafts.values()].map(d=>agentToolNames[d.name]||'工具');
  state.textContent=({running:names.length?'准备'+[...new Set(names)].join('、'):request.text&&!text?'正在整理分析结果…':text?'正在回复…':thinking?'正在思考…':'正在分析…',completed:'',failed:'回复失败 · 内容未完成',cancelled:'已停止 · 内容未完成',interrupted:'已中断 · 内容未完成'}[status]??status);
  state.hidden=!state.textContent;
  state.setAttribute('data-running',String(status==='running'));
  // Update only the text, preserving the summary disclosure and scroll position.
  if(request.renderedText!==text){$('agentRequestOutput-'+request.node).innerHTML=reportMarkdown(text);request.renderedText=text;}
  if(request.renderedThinking!==thinking){$('agentRequestSummaryText-'+request.node).innerHTML=reportMarkdown(thinking);request.renderedThinking=thinking;}
  $('agentRequestSummary-'+request.node).hidden=!thinking;
  $('agentRequestError-'+request.node).textContent=request.error||'';
  $('agentRequest-'+request.node).hidden=status==='completed'&&!text&&!thinking;
}
function renderAgentTool(tool){
  const status=agentStepStatus(tool.status);
  $('agentTool-'+tool.node).setAttribute('data-status',status);
  $('agentToolIcon-'+tool.node).textContent=({running:'◌',completed:'✓',failed:'!',cancelled:'−',interrupted:'−'}[status]||'·');
  $('agentToolState-'+tool.node).textContent=({running:'执行中',completed:'完成',failed:'失败',cancelled:'已停止',interrupted:'已中断'}[status]||status)+(tool.duration_ms!=null?' · '+agentDuration(tool.duration_ms):'');
  const error=tool.result!=null?agentJSON(tool.result).error:'';
  $('agentToolError-'+tool.node).textContent=typeof error==='string'?error:'';
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
  const previousScope=agentView.scope,timeline=$('agentActivityLog'),follow=timeline.scrollHeight-timeline.scrollTop-timeline.clientHeight<80;
  for(const m of messages){
    if(agentView.seen.has(m.id))continue;
    agentView.seen.add(m.id);agentView.messages.push(m);
    const time=m.created_at?dateTime(m.created_at):'',data=agentJSON(m.content);
    if(m.role==='report_plan'){
      agentView.concurrency=data.concurrency;
      for(const group of data.groups)agentView.groups.set(group.id,{...group,status:'queued'});
      continue;
    }
    if(m.role==='group_state'){
      const group=agentView.groups.get(data.id);if(!group)continue;
      Object.assign(group,{status:data.status,error:data.error});
      if(data.error){
        timeline.insertAdjacentHTML('beforeend',`<div data-agent-scope="${esc(data.id)}"><p class="agent-system-event error-text">${esc(data.error)}</p></div>`);
        agentView.scopeCounts.set(data.id,(agentView.scopeCounts.get(data.id)||0)+1);
      }
      followRunningAgent();
      continue;
    }
    if(m.role==='user'){
      // A new turn cannot revive an unfinished group from a previous run.
      for(const group of agentView.groups.values()){
        if(group.status==='running')group.status='interrupted';
        else if(group.status==='queued')group.status='not_started';
      }
    }
    const scope=data.group_id||'main';
    if(scope!=='main'&&!agentView.groups.has(scope))continue;
    const log={insertAdjacentHTML(_,html){
      timeline.insertAdjacentHTML('beforeend',`<div data-agent-scope="${esc(scope)}"${scope!==agentView.scope?' hidden':''}>${html}</div>`);
      agentView.scopeCounts.set(scope,(agentView.scopeCounts.get(scope)||0)+1);
    }};
    if(m.role==='model_request'){
      const request={...data,scope,node:m.id,text:'',summary:'',reasoning:'',drafts:new Map()};agentView.requests.set(data.request_id,request);
      log.insertAdjacentHTML('beforeend',agentBubble('agentRequest-'+m.id,time,`<details id="agentRequestSummary-${m.id}" class="agent-summary" hidden><summary>${request.protocol==='completions'?'完整思考':'思考摘要'}</summary><div id="agentRequestSummaryText-${m.id}" class="agent-model-text"></div></details><div id="agentRequestOutput-${m.id}" class="agent-model-text"></div><p id="agentRequestState-${m.id}" class="agent-reply-state"></p><p id="agentRequestError-${m.id}" class="error-text"></p>`,'assistant',scope));
      renderAgentRequest(request);
    }else if(m.role==='model_delta'){
      const request=agentView.requests.get(data.request_id);if(!request||request.scope!==scope)continue;
      for(const delta of data.deltas){if(delta.kind==='text')request.text+=delta.text||'';else if(delta.kind==='summary')request.summary+=delta.text||'';else if(delta.kind==='reasoning')request.reasoning+=delta.text||'';else if(delta.kind==='tool')request.drafts.set(delta.index,delta);}
      renderAgentRequest(request);
    }else if(m.role==='model_response'){
      const request=agentView.requests.get(data.request_id);if(!request||request.scope!==scope)continue;
      Object.assign(request,{status:data.status,error:data.error,usage:data.usage,duration_ms:data.duration_ms});
      if(data.text)request.text=data.text;if(data.summary)request.summary=data.summary;if(data.reasoning)request.reasoning=data.reasoning;renderAgentRequest(request);
    }else if(m.role==='tool_start'){
      const tool={...data,scope,node:m.id,name:m.tool_name};agentView.tools.set(scope+':'+data.call_id,tool);
      const args=agentJSON(data.arguments),target=args.path||args.container||args.query||'';
      log.insertAdjacentHTML('beforeend',`<div id="agentTool-${m.id}" class="agent-tool"><span id="agentToolIcon-${m.id}" class="agent-tool-icon" aria-hidden="true"></span><div class="agent-tool-description"><span>${esc(agentToolNames[m.tool_name]||'查询数据')}</span>${target?`<code title="${esc(target)}">${esc(target)}</code>`:''}<p id="agentToolError-${m.id}" class="error-text"></p></div><span id="agentToolState-${m.id}" class="agent-tool-state"></span></div>`);renderAgentTool(tool);
    }else if(m.role==='tool_end'){
      const tool=agentView.tools.get(scope+':'+data.call_id);if(tool){Object.assign(tool,data);renderAgentTool(tool);}
    }else if(m.role==='group_report'){
      log.insertAdjacentHTML('beforeend',agentBubble('',time,`<details class="agent-group-result"><summary>${esc(data.text.split('\n')[0].replace(/^#+\s*/,''))} · 查看结果</summary><div class="agent-model-text">${reportMarkdown(data.text)}</div></details>`,'assistant',scope));
    }else if(m.role==='assistant'){
      $('agentReports').insertAdjacentHTML('beforeend',`<article class="agent-report"><div class="agent-answer-time">${esc(time)}</div>${reportMarkdown(m.content)}</article>`);
      const last=[...agentView.requests.values()].at(-1);
      // Final assistant messages often persist the same reply just streamed.
      // Keep one bubble while retaining the completed report for export.
      if(last&&last.scope===scope&&!last.final&&last.status==='completed'&&last.text===m.content&&agentProse(last.text))last.final=true;
      else log.insertAdjacentHTML('beforeend',agentBubble('',time,`<div class="agent-model-text">${reportMarkdown(m.content)}</div>`));
    }else if(m.role==='user'){
      interruptAgentPending();
      for(const request of agentView.requests.values())request.final=true;
      log.insertAdjacentHTML('beforeend',agentBubble('',time,`<p class="agent-question-text">${esc(m.content)}</p>`,'user'));
      if(agentView.messages.some(v=>v.role==='assistant'))$('agentReports').insertAdjacentHTML('beforeend',`<p class="agent-user-question">${esc(m.content)}</p>`);
    }else if(m.role==='status'||m.role==='group_status'){
      const message=m.role==='group_status'?data.text:m.content;
      const text=message.startsWith('结果校验失败：')?'正在核对并修正分析结果…':message;
      log.insertAdjacentHTML('beforeend',`<p class="agent-system-event">${esc(text)}</p>`);
    }
    // Context snapshots, preloaded evidence and duplicate notes are transport
    // data. They do not create chat messages or expose tool JSON in the DOM.
  }
  agentSetTab(agentView.tab);
  if(follow&&previousScope===agentView.scope)timeline.scrollTop=timeline.scrollHeight;
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
  const source=new EventSource(apiURL(`/api/agent/sessions/${agentView.session.id}/events?after=${agentView.cursor}`));agentView.stream=source;
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
function clearAgentSelection(){
  clearTimeout(agentView.timer);closeAgentStream();agentView.selection++;agentView.cursor=0;agentView.messages=[];agentView.reading=false;agentView.requests.clear();agentView.tools.clear();agentView.seen.clear();agentView.tab='conversation';resetAgentGroups();
  $('agentReports').innerHTML='';$('agentActivityLog').innerHTML='';$('agentActivity').hidden=true;$('agentQuestion').value='';agentError('');
  agentView.session=null;renderAgentHistory();renderAgentSession();
}
async function selectAgentSession(session){
  if(session.report_scope!==agentView.reportScope)throw Error('报告类型与当前分析栏目不匹配');
  clearAgentSelection();
  rememberAgentSession(session);renderAgentSession();await readAgentSession();
}
async function selectAgentReportScope(scope){
  if(!['host','container'].includes(scope)||agentView.posting)return;
  if(scope===agentView.reportScope)return;
  agentView.reportScope=scope;clearAgentSelection();
  const sessions=agentScopeSessions(),selected=sessions.find(agentBusy)||sessions[0];
  if(selected)await selectAgentSession(selected);
}
async function loadAgentHistory(){
  const epoch=agentView.epoch;
  const result=await api('/api/agent/sessions');if(epoch!==agentView.epoch)return;
  if(!Array.isArray(result.sessions)||result.sessions.some(session=>!['','host','container'].includes(session.report_scope)))throw Error('分析记录格式无效');
  agentView.sessions=result.sessions;renderAgentHistory();
}
async function openAgentReports(scope=agentView.reportScope){
  if(!agentAllowed()||agentView.posting)return;
  if(!$('agentDialog').open)$('agentDialog').showModal();
  const epoch=agentView.epoch;agentError('');
  try{
    await selectAgentReportScope(scope);if(epoch!==agentView.epoch)return;
    await loadAgentHistory();if(epoch!==agentView.epoch)return;
    if(agentView.session)await readAgentSession();
    else if(agentScopeSessions().length)await selectAgentSession(agentScopeSessions().find(agentBusy)||agentScopeSessions()[0]);
    else renderAgentSession();
  }catch(error){if(epoch===agentView.epoch)agentError(error);}
}
async function generateDiskReport(scope='container'){
  if(!agentAllowed()||!snapshot||!platform.loaded||agentView.posting||platform.resultLoad||platform.changesLoad)return;
  if(scope==='host'&&!hostReportReady()){if(!$('agentDialog').open)$('agentDialog').showModal();agentError('当前扫描未包含 Host 目录，请在扫描配置中添加宿主机目录并重新扫描。');return;}
  const concurrency=Number(agentView.nextConcurrency[scope]);
  if(!Number.isInteger(concurrency)||concurrency<1||concurrency>16){if(!$('agentDialog').open)$('agentDialog').showModal();agentError('最大并行 Agent 数必须为 1–16 的整数');return;}
  const source={snapshot_id:platform.loaded,revision:snapshot.revision,concurrency,scope},epoch=agentView.epoch;
  agentView.posting=true;agentError('');agentControls();
  if(!$('agentDialog').open)$('agentDialog').showModal();
  try{
    agentView.reportScope=scope;clearAgentSelection();
    await loadAgentHistory();if(epoch!==agentView.epoch)return;
    const active=agentView.sessions.find(agentBusy);
    if(active){
      if(['host','container'].includes(active.report_scope)){
        agentView.reportScope=active.report_scope;
        await selectAgentSession(active);
        if(active.report_scope!==scope)agentError(`已有${agentScopeName(active.report_scope)}报告正在分析，请等待完成或停止后再生成${agentScopeName(scope)}报告。`);
      }else agentError('已有其他 Agent 任务正在运行，请等待任务结束后再生成报告。');
      return;
    }
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
    if(action==='messages'){rememberAgentSession(result);$('agentQuestion').value='';selectAgentScope('main');}
    else if(action==='retry'){rememberAgentSession(result);selectAgentScope(body.requests.some(r=>r.group_id===agentView.scope)?agentView.scope:body.requests[0].group_id);}
    else agentView.session.status='cancelling';
    renderAgentSession();await readAgentSession();
  }catch(error){if(current())agentError(error);}
  finally{if(current()){agentView.posting=false;agentControls();scheduleAgentPoll();}}
}
function downloadAgentReport(){
  const s=agentView.session;if(!s||!agentView.messages.some(m=>m.role==='assistant'))return;
  const parts=[`# ${agentScopeName(s.report_scope)}空间分析记录\n\n生成时间：${dateTime(s.created_at)}\n\n模型：${s.model}\n\n扫描记录：${s.snapshot_id||'已删除'}\n\n状态：${agentStatusNames[s.status]||s.status}`];
  let answered=false;
  for(const m of agentView.messages){
    if(m.role==='assistant'){parts.push(m.content);answered=true;}
    else if(m.role==='user'&&answered)parts.push('## 追问\n\n'+m.content);
    else if(m.role==='status')parts.push(m.content);
  }
  const url=URL.createObjectURL(new Blob([parts.join('\n\n---\n\n')+'\n'],{type:'text/markdown;charset=utf-8'}));
  const link=document.createElement('a');link.href=url;link.download=`${agentScopeName(s.report_scope)}空间报告-${s.id.slice(0,8)}.md`;link.click();setTimeout(()=>URL.revokeObjectURL(url),1000);
}
window.AgentUI={controls:agentControls,reset(){
  clearTimeout(agentView.timer);closeAgentStream();
  Object.assign(agentView,{reportScope:'container',epoch:agentView.epoch+1,selection:0,sessions:[],session:null,messages:[],cursor:0,timer:null,posting:false,reading:false,error:'',stream:null,tab:'conversation',requests:new Map(),tools:new Map(),seen:new Set()});
  resetAgentGroups();setAgentConcurrency(3,'host');setAgentConcurrency(3,'container');
  if($('agentDialog').open)$('agentDialog').close();
  $('agentReports').innerHTML='';$('agentActivityLog').innerHTML='';$('agentActivity').hidden=true;$('agentQuestion').value='';
  agentError('');renderAgentHistory();renderAgentSession();
}};
$('generateReport').addEventListener('click',()=>generateDiskReport('container'));
$('generateHostReport').addEventListener('click',()=>generateDiskReport('host'));
$('viewReports').addEventListener('click',()=>openAgentReports('container'));
$('viewHostReports').addEventListener('click',()=>openAgentReports('host'));
$('agentHostScope').addEventListener('click',()=>selectAgentReportScope('host').catch(agentError));
$('agentContainerScope').addEventListener('click',()=>selectAgentReportScope('container').catch(agentError));
$('closeAgent').addEventListener('click',()=>$('agentDialog').close());
$('agentDialog').addEventListener('close',()=>{clearTimeout(agentView.timer);closeAgentStream();});
$('agentConversationTab').addEventListener('click',()=>selectAgentScope('main'));
$('agentGroupList').addEventListener('click',event=>{const button=event.target.closest('[data-agent-group]');if(button)selectAgentScope(button.dataset.agentGroup);});
$('agentFollowCurrent').addEventListener('click',()=>{agentView.followActive=true;followRunningAgent();renderAgentSidebar();});
$('agentReportTab').addEventListener('click',()=>{agentView.followActive=false;agentSetTab('report');});
$('agentJumpLatest').addEventListener('click',()=>{agentSetTab('conversation');$('agentActivityLog').scrollTop=$('agentActivityLog').scrollHeight;});
$('reloadReports').addEventListener('click',()=>openAgentReports(agentView.reportScope));
$('agentHistory').addEventListener('change',()=>{const session=agentView.sessions.find(s=>s.id===$('agentHistory').value);if(session)selectAgentSession(session);});
$('stopAgent').addEventListener('click',()=>postAgentAction('cancel',{}));
$('agentRetry').addEventListener('click',retryAgentGroups);
$('downloadReport').addEventListener('click',downloadAgentReport);
$('agentModelSettings').addEventListener('click',()=>{$('agentDialog').close();showPage('agent-settings');});
$('agentFollowup').addEventListener('submit',e=>{e.preventDefault();const message=$('agentQuestion').value.trim();if(message&&!agentBusy(agentView.session)&&!agentView.reading&&agentView.session?.snapshot_id)postAgentAction('messages',{message});});
document.querySelectorAll('[data-agent-question]').forEach(button=>button.addEventListener('click',()=>{$('agentQuestion').value=button.dataset.agentQuestion;$('agentQuestion').focus();}));

// This limit belongs to the next diagnosis; running plans keep their saved limit.
function setAgentConcurrency(value,scope=agentView.reportScope){
  agentView.nextConcurrency[scope]=value;
  $(scope==='host'?'hostReportConcurrency':'reportConcurrency').value=value;
  if(agentView.reportScope===scope)$('agentConcurrency').value=value;
}
$('agentConcurrency').addEventListener('input',event=>setAgentConcurrency(event.target.value));
$('reportConcurrency').addEventListener('input',event=>setAgentConcurrency(event.target.value,'container'));
$('hostReportConcurrency').addEventListener('input',event=>setAgentConcurrency(event.target.value,'host'));
$('startAgentReport').addEventListener('click',()=>generateDiskReport(agentView.reportScope));
setAgentConcurrency(3,'host');setAgentConcurrency(3,'container');
