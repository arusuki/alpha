'use strict';
(()=>{
const state={epoch:0,busy:false,nodes:[],results:[],plan:null};
const targets=()=>[...$('scanPlanTargets').querySelectorAll('input')];
function controls(){
  $('scanPlanFields').disabled=state.busy;
  $('scanPlanClose').disabled=state.busy;
  $('scanPlanSubmit').disabled=state.busy||!targets().some(el=>el.checked);
  $('scanPlanRetry').disabled=state.busy;
  $('scanPlanRetry').hidden=!state.plan||!state.results.some(r=>!r.ok);
}
function modeControls(){
  const calendar=$('planMode').value==='calendar',interval=$('planMode').value==='interval';
  $('planCalendarFields').hidden=!calendar;$('planIntervalFields').hidden=!interval;
  $('planTimes').disabled=!calendar;$('planTimezone').disabled=!calendar;$('planInterval').disabled=!interval;
}
function clearResults(){
  state.results=[];state.plan=null;
  $('scanPlanResults').innerHTML='';$('scanPlanStatus').textContent='';$('scanPlanError').textContent='';controls();
}
async function open(){
  if(platform.user?.role!=='admin'||state.busy)return;
  const epoch=++state.epoch;state.nodes=[];clearResults();state.busy=true;controls();
  $('scanPlanTargets').innerHTML='';$('scanPlanStatus').textContent='正在读取 worker…';$('scanPlanDialog').showModal();
  try{
    const data=await api('/api/cluster/nodes');if(epoch!==state.epoch)return;
    state.nodes=data.nodes.filter(n=>n.kind==='worker');
    $('scanPlanTargets').innerHTML=state.nodes.map(n=>`<label class="checkbox-label"><input type="checkbox" value="${esc(n.id)}" checked>${esc(n.name)}</label>`).join('');
    $('scanPlanStatus').textContent=state.nodes.length?`已选择全部 ${state.nodes.length} 个 worker；下发后逐台显示结果。`:'暂无 worker，请先添加计算节点。';
  }catch(error){if(epoch===state.epoch)$('scanPlanError').textContent=error.message;}
  finally{if(epoch===state.epoch){state.busy=false;controls();}}
}
function readPlan(){
  return {auto_agent_analyze:$('planAutoAgentAnalyze').checked,schedule_mode:$('planMode').value,interval_minutes:$('planMode').value==='interval'?Number($('planInterval').value):0,
    schedule_times:$('planTimes').value.split('\n').map(s=>s.trim()).filter(Boolean),
    schedule_weekdays:[0,1,2,3,4,5,6].filter(day=>$('planWeekday'+day).checked),
    schedule_timezone:$('planTimezone').value.trim(),retain_records:Number($('planRetain').value)};
}
async function distribute(retry=false){
  if(state.busy||platform.user?.role!=='admin')return;
  const ids=retry?state.results.filter(r=>!r.ok).map(r=>r.id):targets().filter(el=>el.checked).map(el=>el.value);
  if(!ids.length)return;
  const plan=retry?state.plan:readPlan(),epoch=state.epoch;
  if(!retry)state.results=[];
  state.plan=plan;state.busy=true;controls();$('scanPlanError').textContent='';
  $('scanPlanStatus').textContent=`正在向 ${ids.length} 个 worker 下发计划…`;
  try{
    const data=await api('/api/cluster/scan-schedule',{method:'POST',body:JSON.stringify({node_ids:ids,schedule:plan})});
    if(epoch!==state.epoch)return;
    const updates=new Map(data.results.map(r=>[r.id,r]));
    state.results=retry?state.results.map(r=>updates.get(r.id)||r):data.results;
    const failed=state.results.filter(r=>!r.ok).length;
    $('scanPlanStatus').textContent=`下发完成：成功 ${state.results.length-failed} 个，失败 ${failed} 个。`;
    $('scanPlanResults').innerHTML=state.results.map(r=>`<p class="${r.ok?'form-note':'error-text'}"><strong>${esc(r.name)}</strong> · ${r.ok?'已下发':esc(r.error)}</p>`).join('');
  }catch(error){
    if(epoch===state.epoch){$('scanPlanError').textContent=error.message;$('scanPlanStatus').textContent='未取得完整下发结果，请核实节点配置后重新下发。';}
  }finally{if(epoch===state.epoch){state.busy=false;controls();}}
}
window.ScanScheduleUI={reset(){
  state.epoch++;state.busy=false;state.nodes=[];$('scanPlanForm').reset();$('scanPlanTargets').innerHTML='';
  $('planTimezone').value=Intl.DateTimeFormat().resolvedOptions().timeZone||'UTC';
  clearResults();modeControls();if($('scanPlanDialog').open)$('scanPlanDialog').close();
}};
$('clusterScanSchedule').addEventListener('click',open);
$('scanPlanForm').addEventListener('submit',e=>{e.preventDefault();distribute();});
$('scanPlanRetry').addEventListener('click',()=>distribute(true));
$('scanPlanClose').addEventListener('click',()=>{if(!state.busy)$('scanPlanDialog').close();});
$('scanPlanDialog').addEventListener('cancel',e=>{if(state.busy)e.preventDefault();});
$('scanPlanFields').addEventListener('input',clearResults);
$('scanPlanTargets').addEventListener('change',controls);
$('planMode').addEventListener('change',()=>{modeControls();clearResults();});
for(const [id,checked] of [['scanPlanSelectAll',true],['scanPlanSelectNone',false]])$(id).addEventListener('click',()=>{targets().forEach(el=>el.checked=checked);clearResults();});
window.ScanScheduleUI.reset();
})();
