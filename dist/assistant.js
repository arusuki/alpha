'use strict';
(()=>{
let session='',messages=[],pending=null,epoch=0,selection=0,refreshSequence=0,healthSequence=0;
const el=id=>document.getElementById(id);
function status(text){el('assistantStatus').textContent=text;}
function busy(value){
  for(const id of ['assistantSend','assistantPrompt','assistantNew','assistantSessions','assistantMemoryClear'])el(id).disabled=value;
  el('assistantStop').hidden=!value||!pending;
}
function render(){
  const log=el('assistantMessages');log.replaceChildren();
  if(!messages.length){const p=document.createElement('p');p.textContent='可以问：“帮我检查各节点的运行状态”，或告诉我需要记住的运维偏好。';log.append(p);}
  for(const row of messages){
    const article=document.createElement('article');article.className='assistant-message';article.dataset.role=row.role;
    const title=document.createElement('strong');title.textContent=row.role==='user'?'你':'运维助理';
    const content=document.createElement('div');content.textContent=row.content;
    article.append(title,content);log.append(article);
  }
  log.scrollTop=log.scrollHeight;
}
async function refresh(){
  const generation=epoch,sequence=++refreshSequence;
  const [history,memory]=await Promise.all([api('/api/assistant/sessions'),api('/api/assistant/memories')]);
  if(generation!==epoch||sequence!==refreshSequence)return;
  const select=el('assistantSessions');select.replaceChildren(new Option('选择会话',''));
  for(const row of history.sessions)select.add(new Option(row.title,row.id));select.value=session;
  const list=el('assistantMemories');list.replaceChildren();
  for(const row of memory.memories){const li=document.createElement('li');li.textContent=row.memory;list.append(li);}
  if(!memory.memories.length){const li=document.createElement('li');li.textContent='暂无长期记忆';list.append(li);}
}
function healthText(health){
 return health?.status==='ready'?`Agno 服务就绪 · ${health.agno_version} · ${health.active_runs}/${health.capacity} 运行中`:(health?.detail||'Agno 状态未知');
}
async function checkHealth(){
 const generation=epoch,sequence=++healthSequence;
 el('assistantHealthRefresh').disabled=true;
 el('assistantHealth').textContent='正在检查 Agno 服务…';
 try{const health=await api('/api/assistant/health');if(generation!==epoch||sequence!==healthSequence)return;el('assistantHealth').textContent=healthText(health);el('assistantHealth').dataset.state=health.status;}
 catch(e){if(generation===epoch&&sequence===healthSequence){el('assistantHealth').textContent=e.message;el('assistantHealth').dataset.state='unavailable';}}
 finally{if(generation===epoch&&sequence===healthSequence)el('assistantHealthRefresh').disabled=false;}
}
async function open(){const generation=epoch;await Promise.all([checkHealth(),refresh().catch(e=>{if(generation===epoch)status(e.message);})]);}
function reset(){window.AssistantCLI?.reset();window.AssistantSettings?.reset();epoch++;selection++;healthSequence++;el('assistantHealth').textContent='Agno 状态待检查';delete el('assistantHealth').dataset.state;el('assistantHealthRefresh').disabled=false;pending?.abort();pending=null;session='';messages=[];el('assistantPrompt').value='';el('assistantSessions').replaceChildren(new Option('选择会话',''));el('assistantMemories').replaceChildren();busy(false);status('');render();}
el('assistantNew').addEventListener('click',()=>{selection++;session='';messages=[];el('assistantSessions').value='';status('');render();});
el('assistantSessions').addEventListener('change',async()=>{
 const id=el('assistantSessions').value;if(!id)return;
 const generation=epoch,seq=++selection;busy(true);
 try{const data=await api('/api/assistant/sessions/'+id);if(generation!==epoch||seq!==selection)return;session=id;messages=data.messages;status('');render();}
 catch(e){if(generation===epoch)status(e.message);}
 finally{if(generation===epoch&&seq===selection)busy(false);}
});
el('assistantForm').addEventListener('submit',async e=>{
 e.preventDefault();if(pending)return;
 const text=el('assistantPrompt').value.trim();if(!text)return;
 const controller=new AbortController(),generation=epoch;pending=controller;busy(true);status('正在查询和分析…');
 try{
  const path=session?'/api/assistant/sessions/'+session+'/messages':'/api/assistant/sessions';
  const data=await api(path,{method:'POST',body:JSON.stringify({message:text}),signal:controller.signal});
  if(generation!==epoch)return;
  session=data.id;messages=data.messages;el('assistantPrompt').value='';render();status('本轮已完成并保存。');await refresh();
 }catch(error){if(generation===epoch)status(error.name==='AbortError'?'已停止等待。若请求恰好完成，结果可能已保存；可从历史会话核对。':error.message);}
 finally{if(generation===epoch){pending=null;busy(false);}}
});
el('assistantHealthRefresh').addEventListener('click',checkHealth);
el('assistantStop').addEventListener('click',()=>pending?.abort());
el('assistantMemoryRefresh').addEventListener('click',open);
el('assistantMemoryClear').addEventListener('click',async()=>{
 if(!window.confirm('清空你的全部长期记忆？历史对话仍会保留。'))return;
 const generation=epoch;busy(true);
 try{await api('/api/assistant/memories',{method:'DELETE'});if(generation!==epoch)return;await refresh();status('长期记忆已清空。');}
 catch(e){if(generation===epoch)status(e.message);}
 finally{if(generation===epoch)busy(false);}
});
window.AssistantUI={open,reset,healthText};
})();

(()=>{
 const el=id=>document.getElementById(id);
 const labels={list_nodes:'节点列表',get_node_inventory:'节点概况',get_node_storage:'存储扫描',get_node_containers:'容器状态',get_node_processes:'容器进程',get_node_gpu:'GPU 状态'};
 let current=null,generation=0,sequence=0,loaded=false;
 const status=text=>{el('assistantSettingsStatus').textContent=text;};
 function field(label,type,value=''){
  const wrap=document.createElement('label');wrap.append(document.createTextNode(label));
  const input=document.createElement('input');input.type=type;input.value=value;wrap.append(input);return [wrap,input];
 }
 function addServer(server={id:'',url:'',enabled:false,tools:[]},hasToken=false){
  const row=document.createElement('fieldset');row.className='assistant-mcp';
  const legend=document.createElement('legend');legend.textContent='MCP 服务';row.append(legend);
  for(const [name,label,type,value] of [['id','唯一标识','text',server.id],['url','服务地址（如 http://127.0.0.1:8000/mcp）','url',server.url],['tools','允许的工具名称（用逗号分隔）','text',server.tools.join(', ')],['token',hasToken?'Bearer Token（已保存，留空保留）':'Bearer Token（可选）','password','']]){
   const [wrap,input]=field(label,type,value);input.dataset.field=name;input.autocomplete=name==='token'?'new-password':'off';
   if(name==='id'){input.required=true;input.pattern='[a-z][a-z0-9_]{0,15}';input.maxLength=16;input.readOnly=!!server.id;}
   if(name==='url'){input.required=true;input.maxLength=2048;}
   if(name==='token')input.maxLength=8192;
   row.append(wrap);
  }
  for(const [name,label,checked] of [['enabled','启用此服务',server.enabled],['clear','清除已保存的 Token',false]]){
   const [wrap,input]=field(label,'checkbox');wrap.className='assistant-check';input.dataset.field=name;input.checked=checked;row.append(wrap);
  }
  const note=document.createElement('p');note.textContent='修改服务地址会清除原 Token；如新地址需要认证，请重新填写。';row.append(note);
  const remove=document.createElement('button');remove.type='button';remove.textContent='移除服务';remove.addEventListener('click',()=>row.remove());row.append(remove);
  el('assistantMCPServers').append(row);
 }
 function render(data){
  current=data;el('assistantInstructions').value=data.value.instructions;
  const list=el('assistantBuiltinTools');list.replaceChildren();
  for(const name of data.available_tools){const [wrap,input]=field(labels[name]||name,'checkbox');wrap.className='assistant-check';input.value=name;input.checked=data.value.builtin_tools.includes(name);list.append(wrap);}
  el('assistantMCPServers').replaceChildren();for(const server of data.value.mcp_servers)addServer(server,!!data.has_tokens[server.id]);
 }
 async function load(){
  const gen=generation,seq=++sequence;el('assistantSettingsFields').disabled=true;status('正在读取助理配置…');
  try{const data=await api('/api/assistant/settings');if(gen!==generation||seq!==sequence)return;render(data);loaded=true;status('');}
  catch(e){if(gen===generation&&seq===sequence)status(e.message);}
  finally{if(gen===generation&&seq===sequence)el('assistantSettingsFields').disabled=!current;}
 }
 el('assistantSettingsPanel').addEventListener('toggle',()=>{if(el('assistantSettingsPanel').open&&!loaded)load();});
 el('assistantSettingsReload').addEventListener('click',load);
 el('assistantMCPAdd').addEventListener('click',()=>{if(el('assistantMCPServers').children.length>=8){status('MCP 服务最多 8 个');return;}addServer();});
 el('assistantSettingsForm').addEventListener('submit',async e=>{
  e.preventDefault();if(!current)return;
  const value={instructions:el('assistantInstructions').value,builtin_tools:Array.from(el('assistantBuiltinTools').querySelectorAll('input:checked'),i=>i.value),mcp_servers:[]},tokens={},clear_tokens=[];
  for(const row of el('assistantMCPServers').children){
   const input=name=>row.querySelector(`[data-field="${name}"]`);
   const id=input('id').value.trim();value.mcp_servers.push({id,url:input('url').value.trim(),enabled:input('enabled').checked,tools:input('tools').value.split(/[,，\s]+/).filter(Boolean)});
   if(input('token').value)tokens[id]=input('token').value;if(input('clear').checked)clear_tokens.push(id);
  }
  const gen=generation,seq=++sequence;el('assistantSettingsFields').disabled=true;status('正在保存…');
  try{const data=await api('/api/assistant/settings',{method:'PUT',body:JSON.stringify({revision:current.revision,value,tokens,clear_tokens})});if(gen!==generation||seq!==sequence)return;render(data);status('配置已保存，从下一轮对话生效。');}
  catch(e){if(gen===generation&&seq===sequence)status(e.message);}
  finally{if(gen===generation&&seq===sequence)el('assistantSettingsFields').disabled=false;}
 });
 window.AssistantSettings={reset(){generation++;sequence++;current=null;loaded=false;el('assistantSettingsPanel').open=false;el('assistantSettingsFields').disabled=true;el('assistantInstructions').value='';el('assistantBuiltinTools').replaceChildren();el('assistantMCPServers').replaceChildren();status('');}};
})();


(()=>{
 'use strict';
 const el=id=>document.getElementById(id);
 let revision=0,generation=0,sequence=0,loaded=false;
 const status=text=>{el('assistantCLIStatus').textContent=text;};
 function render(data){
  revision=data.settings.revision;el('assistantCLIServer').value=data.settings.server;el('assistantCLIPath').value=data.settings.command_path;
  const list=el('assistantCLIDeliveries');list.replaceChildren();
  for(const row of data.deliveries||[]){const li=document.createElement('li');li.textContent=`${row.username} · ${row.node_name}：${({pending:'等待下发',ready:'已同步',failed:'下发失败'})[row.state]||row.state}${row.error?' — '+row.error:''}`;list.append(li);}
 }
 async function load(){
  const gen=generation,seq=++sequence;el('assistantCLIFields').disabled=true;status('正在读取…');
  try{const data=await api('/api/assistant/cli');if(gen!==generation||seq!==sequence)return;render(data);loaded=true;status('');}
  catch(e){if(gen===generation&&seq===sequence)status(e.message);}
  finally{if(gen===generation&&seq===sequence)el('assistantCLIFields').disabled=!revision;}
 }
 async function submit(sync){
  const gen=generation,seq=++sequence;el('assistantCLIFields').disabled=true;status('正在提交…');
  const body=sync?{}:{revision,server:el('assistantCLIServer').value.trim().replace(/\/$/,''),command_path:el('assistantCLIPath').value.trim()};
  try{const data=await api('/api/assistant/cli'+(sync?'/sync':''),{method:sync?'POST':'PUT',body:JSON.stringify(body)});if(gen!==generation||seq!==sequence)return;revision=data.settings.revision;status('已加入下发队列，可刷新查看逐节点结果。');}
  catch(e){if(gen===generation&&seq===sequence)status(e.message);}
  finally{if(gen===generation&&seq===sequence)el('assistantCLIFields').disabled=false;}
 }
 el('assistantCLIPanel').addEventListener('toggle',()=>{if(el('assistantCLIPanel').open&&!loaded)load();});
 el('assistantCLIForm').addEventListener('submit',e=>{e.preventDefault();submit(false);});
 el('assistantCLISync').addEventListener('click',()=>submit(true));
 el('assistantCLIReload').addEventListener('click',load);
 window.AssistantCLI={reset(){generation++;sequence++;revision=0;loaded=false;el('assistantCLIPanel').open=false;el('assistantCLIFields').disabled=true;el('assistantCLIServer').value='';el('assistantCLIPath').value='/usr/local/bin/alpha-chat';el('assistantCLIDeliveries').replaceChildren();status('');}};
})();
