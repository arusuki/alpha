'use strict';
let dashboardRecentHTML='';
function renderDashboard() {
  if(!platform.user)return;
  $('welcomeUser').textContent=platform.user.username;
  $('workspaceDate').textContent=new Date().toLocaleDateString('zh-CN',{year:'numeric',month:'long',day:'numeric',weekday:'long'});
  const latest=platform.history.find(job=>job.id===platform.latest);
  $('dashboardStorageStatus').textContent=platform.active?'扫描进行中':latest?'已有扫描结果':'等待首次扫描';
  $('dashboardAllocated').textContent=latest?fmt(latest.allocated):'—';
  $('dashboardTask').textContent=platform.active?(statusNames[platform.active.status]||platform.active.status):'空闲';
  $('dashboardSchedule').textContent=platform.interval?`每 ${platform.interval} 分钟`:'手动扫描';
  $('dashboardUpdated').textContent=latest?`扫描完成于 ${dateTime(latest.finished_at)}`:'完成首次扫描后，查看主机空间概况。';
  const recent=platform.history.slice(0,3);
  const html=recent.map(job=>`<button class="recent-row" ${job.status==='completed'?`data-open-job="${esc(job.id)}"`:'data-page="history"'}><span class="recent-icon"><svg class="ui-icon" aria-hidden="true"><use href="#icon-storage"/></svg></span><span class="recent-copy"><strong>${esc(triggerNames[job.trigger]||'存储')}扫描</strong><small>${esc(dateTime(job.created_at))} · ${esc(job.created_by)}</small></span><span class="pill status-${esc(job.status)}">${esc(statusNames[job.status]||job.status)}</span><span class="recent-arrow" aria-hidden="true">↗</span></button>`).join('')||'<div class="dashboard-empty"><strong>还没有扫描记录</strong><p>从存储模块开始，建立主机的第一份空间档案。</p><button data-page="overview">前往存储 →</button></div>';
  if(dashboardRecentHTML!==html){$('dashboardRecent').innerHTML=html;dashboardRecentHTML=html;}
}
window.DashboardUI={render:renderDashboard};
$('dashboardRefresh').addEventListener('click',()=>act(async()=>{await syncState();message('工作台状态已更新。');},$('dashboardRefresh')));
