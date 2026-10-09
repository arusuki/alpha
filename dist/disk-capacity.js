'use strict';
// Shared filesystem capacity presentation for control and member views.
const DiskCapacity = (() => {
  const virtualFilesystemTypes = new Set('proc sysfs devtmpfs devpts tmpfs cgroup cgroup2 securityfs debugfs tracefs pstore mqueue hugetlbfs configfs fusectl autofs binfmt_misc rpc_pipefs nsfs overlay squashfs'.split(' '));
  const filesystems = filesystems => filesystems.filter(d => !virtualFilesystemTypes.has(d.fs));
  const diskIdentityLabel = d => [d.block_device || (d.device != null ? `设备 ${d.device}` : ''), ...(d.physical_disks || []).map(p => `${p.device}${p.model ? ' · '+p.model : ''}`)].filter(Boolean).join(' · ');
  const percent = (value,total) => total > 0 ? Math.max(0,Math.min(100,value/total*100)) : 0;
  const percentLabel = (value,total) => total > 0 ? `${value > 0 && value/total < .001 ? '<0.1' : (value/total*100).toLocaleString('zh-CN',{maximumFractionDigits:1})}%` : '—';
  function summary(disks,fmt) {
    const capacity = disks.reduce((sum,d) => sum+d.total,0);
    const available = disks.reduce((sum,d) => sum+d.available,0);
    return `<div class="capacity-total"><span>${disks.length} 个文件系统 · 总容量合计</span><strong>${fmt(disks.length ? capacity : null)}</strong><span>剩余可用合计 ${fmt(disks.length ? available : null)} · 空闲空间按文件系统独立使用</span></div>`;
  }
  function cards(disks,{esc,fmt,highlight}) {
    return disks.map(d => {
      const reserved = Math.max(0,d.total-d.used-d.available), denominator = Math.max(d.total,d.used+d.available);
      return `<article class="filesystem-card"><div class="filesystem-heading"><div><span class="disk-icon" aria-hidden="true">▤</span><strong class="mono">${esc(d.mount)}${d === highlight ? ' · Docker' : ''}</strong><span class="sub">${esc(d.fs)} · 总容量 ${fmt(d.total)}</span></div><strong>${percentLabel(d.used,d.total)}<small>已用</small></strong></div>${diskIdentityLabel(d) ? `<p class="filesystem-device">${esc(diskIdentityLabel(d))}</p>` : ''}<div class="capacity-bar" role="img" aria-label="${esc(d.mount)}：已用 ${fmt(d.used)}，可用 ${fmt(d.available)}，保留 ${fmt(reserved)}"><span style="width:${percent(d.used,denominator)}%;background:#4361d8"></span><span class="reserved-fill" style="width:${percent(reserved,denominator)}%"></span></div><div class="capacity-labels"><span><i style="--swatch:#4361d8"></i>已用 ${fmt(d.used)}</span><span>可用 ${fmt(d.available)}${reserved ? ` · 保留 ${fmt(reserved)}` : ''}</span></div></article>`;
    }).join('') || '<p class="muted">暂无分区容量数据</p>';
  }
  return {filesystems,summary,cards};
})();
