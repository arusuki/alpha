'use strict';
const $ = id => document.getElementById(id);
const esc = value => String(value == null ? '' : value).replace(/[&<>"']/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c]));
const fmt = value => {
  if (value == null || !Number.isFinite(value)) return '未知';
  const units = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB'];
  let n = value, i = 0;
  while (n >= 1024 && i < units.length-1) { n /= 1024; i++; }
  return `${n.toLocaleString('zh-CN', {maximumFractionDigits:i ? 2 : 0})} ${units[i]}`;
};
const diskFilesystems = DiskCapacity.filesystems;
const filesystemForPath = (filesystems,path) => typeof path === 'string' ? filesystems.filter(d => path === d.mount || path.startsWith(d.mount.replace(/\/$/,'')+'/')).sort((a,b) => b.mount.length-a.mount.length)[0] : null;
const dockerFilesystem = data => filesystemForPath(diskFilesystems(data.filesystems),data.docker.root_canonical || data.docker.root);
const filesystemCapacityNote = filesystems => filesystems.length ? filesystems.map(d => `${d.mount} 可用 ${fmt(d.available)}`).join(' · ') + (filesystems.length > 1 ? '（各文件系统独立，空闲空间不可互用）' : '') : '文件系统可用容量未知';
const containerStates = {running:'运行中',exited:'已停止',created:'未启动',paused:'已暂停',restarting:'重启中',removing:'移除中',dead:'异常'};
let snapshot = null, usage = null, selected = null, query = '', ownerFilter = null, stateFilter = 'all', sortOrder = 'total-desc', tablePage = 0;
const pageSize = 25;
let containerView = 'chart', explorer = null;
const HOST = Symbol('host');
let containerColors = new Map();
const storageColors = ['#4361d8','#168c8a','#b57932','#9670ca','#518c4a','#ca6386','#487fac','#9d745d'];
const colorFor = key => { let hash = 0; for (const char of key) hash = (hash*31 + char.charCodeAt(0)) >>> 0; return storageColors[hash % storageColors.length]; };
const percent = (value,total) => total > 0 ? Math.max(0,Math.min(100,value/total*100)) : 0;
const percentLabel = (value,total) => total > 0 ? `${value > 0 && value/total < .001 ? '<0.1' : (value/total*100).toLocaleString('zh-CN',{maximumFractionDigits:1})}%` : '—';
function message(text) { $('message').textContent = text; $('message').hidden = !text; }
function validate(data) { return SnapshotData.validate(data); }

function load(data, source, preparedUsage, preserveExplorer=false) {
  if (!preparedUsage) validate(data);
  const nextUsage = preparedUsage || Usage.build(data);
  const sameHost = snapshot && snapshot.host === data.host;
  if (preparedUsage && (!snapshot || snapshot.job_id !== data.job_id)) selected = null;
  const previousExplorer = preserveExplorer && snapshot && snapshot.job_id === data.job_id && explorer ? {...explorer,trail:explorer.trail.slice(),focus:null,focusKey:explorer.focus == null ? null : (explorer.entries[explorer.focus] || {}).path || (explorer.entries[explorer.focus] || {}).kind} : null;
  snapshot = data; usage = nextUsage; explorer = previousExplorer;
  if (explorer) while (explorer.trail.length && !usage.inspect(explorer.trail[explorer.trail.length-1].path).known) explorer.trail.pop();
  containerColors = new Map([...usage.containers.keys()].sort().map((id,index) => [id,storageColors[index % storageColors.length]]));
  if (!sameHost) { query = ''; ownerFilter = null; stateFilter = 'all'; sortOrder = 'total-desc'; tablePage = 0; }
  $('search').value = query; $('stateFilter').value = stateFilter; $('sortOrder').value = sortOrder;
  if (ownerFilter !== null && !usage.owners.has(ownerFilter)) ownerFilter = null;
  if (selected && selected !== HOST) selected = data.containers.find(c => c.id === selected.id) || null;
  if (!selected && $('containerDialog').open) $('containerDialog').close();
  $('sourceBadge').textContent = source;
  $('hostInfo').textContent = `${data.host || '未知主机'} · 扫描完成于 ${new Date(data.finished_at).toLocaleString('zh-CN')}${data.updated_at ? ` · 明细更新于 ${new Date(data.updated_at).toLocaleString('zh-CN')}` : ''}`;
  const scroll = preserveExplorer ? $('containerDialog').scrollTop : null;
  const searchFocused = preserveExplorer && document.activeElement && document.activeElement.id === 'mapSearch';
  const caret = searchFocused ? document.activeElement.selectionStart : null;
  render();
  if (scroll != null) $('containerDialog').scrollTop = scroll;
  if (searchFocused && $('mapSearch')) { $('mapSearch').focus({preventScroll:true}); if(caret != null)$('mapSearch').setSelectionRange(caret,caret); }
  renderTask(platform.config && platform.config.value && platform.config.value.interval_minutes);
}
function amount(bytes, row) {
  return `<span class="amount">${fmt(row.known ? bytes : null)}</span>${row.partial ? '<span class="sub incomplete">部分统计</span>' : ''}`;
}
function writableSize(row) {
  const w = row.writable, labels = {partial:'部分统计',unreadable:'无法读取',excluded:'已排除',unavailable:'未能统计'};
  return `<span class="amount">${fmt(w.allocated)}</span>${w.status !== 'complete' ? `<span class="sub incomplete" title="${esc(w.reason)}">${w.permission_denied ? '权限不足' : labels[w.status] || '部分统计'}</span>` : ''}<span class="sub">Docker 逻辑 ${fmt(row.container.size_rw)}</span>`;
}
function render() {
  if (!snapshot) return;
  const all = [...usage.containers.values()], unassigned = all.filter(r => !r.owner), partial = all.filter(r => r.partial);
  $('containerCount').textContent = snapshot.containers.length;
  $('runningCount').textContent = `${all.filter(r => r.container.state === 'running').length} 个运行中 · ${all.filter(r => r.container.state !== 'running').length} 个非运行中`;
  const known = !all.length || all.some(r => r.known);
  $('exclusiveTotal').textContent = fmt(known ? usage.exclusive : null);
  $('sharedTotal').textContent = fmt(known ? usage.shared : null);
  $('unassignedFilter').textContent = unassigned.length;
  $('unassignedHint').textContent = unassigned.length ? '点击筛选，补全所属用户' : '所有容器均已标注所属用户';
  $('unassignedFilter').setAttribute('aria-pressed', String(ownerFilter === Usage.UNASSIGNED));
  $('coverageNotice').hidden = !partial.length && !snapshot.tree.errors;
  $('coverageNotice').textContent = `${partial.length ? `${partial.length} 个容器的用量不完整。` : ''}${snapshot.tree.errors ? `扫描有 ${snapshot.tree.errors} 处读取异常。` : ''}当前仅汇总已成功读取的空间，未知部分没有按 0 计入。`;
  if (usage.attributionLimited) $('coverageNotice').textContent = '部分目录或共享引用尚待分析，当前保留已知用量与历史统计。可逐层点击继续深入。';
  renderStorageOverview(); renderOwners(); renderContainers(); renderScope();
  if (selected) renderDetail();
}
function renderOwners() {
  const rows = [...usage.owners.values()].sort((a,b) => Number(b.known)-Number(a.known) || b.bytes-a.bytes || a.owner.localeCompare(b.owner,'zh-CN'));
  const max = rows.reduce((value, row) => Math.max(value, row.bytes), 1);
  $('clearOwner').hidden = ownerFilter === null;
  $('ownerRanking').innerHTML = rows.map(r => `<button class="owner-row ${ownerFilter === r.owner ? 'selected' : ''}" data-owner="${esc(r.owner)}" aria-pressed="${ownerFilter === r.owner}"><span class="owner-main"><span class="owner-name">${esc(r.owner || '待归属')}</span><span class="amount">${fmt(r.known ? r.bytes : null)}</span></span><span class="owner-bar" aria-hidden="true"><i style="width:${r.bytes/max*100}%"></i></span><span class="owner-meta"><span>${r.containers.length} 个容器${r.partial ? ' · 部分统计' : ''}</span><span>归属用量</span></span>${r.shared ? `<span class="owner-shared">另引用跨用户共享 ${fmt(r.shared)}</span>` : ''}</button>`).join('') || '<p class="empty">没有容器归属数据</p>';
  $('crossOwnerTotal').textContent = fmt(rows.some(r => r.known) || !rows.length ? usage.crossOwner : null);
}
function renderStorageOverview() {
  const all = [...usage.containers.values()].sort((a,b) => b.exclusive-a.exclusive);
  const total = snapshot.tree.allocated;
  const disks = diskFilesystems(snapshot.filesystems);
  const dockerDisk = dockerFilesystem(snapshot);
  const segments = all.filter(r => r.exclusive > 0).map(r => ({name:r.container.name,bytes:r.exclusive,color:containerColors.get(r.container.id),id:r.container.id}));
  segments.push({name:'容器共享',bytes:usage.shared,color:'#9472cd',className:'shared-fill'}, {name:'Host · 未关联容器',bytes:usage.unrelated,color:'#72849f',host:true});
  $('storageOverview').innerHTML = `<div class="panel-heading"><div><span class="eyebrow">STORAGE OVERVIEW</span><h2>磁盘空间分布</h2><p>${dockerDisk ? `Docker 数据目录位于 ${esc(dockerDisk.mount)} · 该文件系统可用 ${fmt(dockerDisk.available)}` : '按文件系统查看容量与剩余空间'}</p></div>${DiskCapacity.summary(disks,fmt)}</div>
    <div class="filesystem-grid">${DiskCapacity.cards(disks,{esc,fmt,highlight:dockerDisk})}</div>
    <div class="attribution"><div class="attribution-heading"><h3>已扫描空间 · 按容器归属</h3><strong>${fmt(total)}</strong></div><div class="attribution-bar" aria-label="已扫描空间分布">${segments.filter(s => s.bytes>0).map(s => {
      const title = `${s.name}${s.id ? ' · 独占' : ''}：${fmt(s.bytes)} · ${percentLabel(s.bytes,total)}${s.id || s.host ? '，点击查看明细' : ''}`;
      return s.id || s.host ? `<button ${s.host ? 'data-host' : `data-container="${esc(s.id)}"`} style="width:${percent(s.bytes,total)}%;--swatch:${s.color}" title="${esc(title)}" aria-label="${esc(title)}"></button>` : `<span class="${s.className || ''}" style="width:${percent(s.bytes,total)}%;--swatch:${s.color}" title="${esc(title)}" role="img" aria-label="${esc(title)}"></span>`;
    }).join('')}</div><div class="storage-legend">${segments.filter(s => s.bytes>0).map(s => s.id || s.host ? `<button ${s.host ? 'data-host' : `data-container="${esc(s.id)}"`} title="查看 ${esc(s.name)} 的存储明细"><i style="--swatch:${s.color}"></i><span>${esc(s.name)}</span><b>${fmt(s.bytes)}</b></button>` : `<span><i class="${s.className || ''}" style="--swatch:${s.color}"></i>${esc(s.name)}<b>${fmt(s.bytes)}</b></span>`).join('') || '<span>尚无已统计的空间</span>'}</div><p class="storage-note">彩色容器段为独占空间；共享数据全机只计一次。点击 Host 查看未关联容器的目录占用，整盘其余用量见下方对账。</p><button class="host-explorer-entry" data-host><span><b>Host · 宿主机占用</b><small>未关联容器的已扫描空间 · 查看目录与文件</small></span><strong>${fmt(usage.unrelated)} <span aria-hidden="true">→</span></strong></button></div>`;
}
function visibleContainers() {
  const rows = [...usage.containers.values()].filter(r => (ownerFilter === null || r.owner === ownerFilter) && (stateFilter === 'all' || (r.container.state === 'running') === (stateFilter === 'running')) && `${r.container.name} ${r.owner || '待归属'} ${r.container.image} ${r.container.id}`.toLowerCase().includes(query));
  rows.sort((a,b) => {
    if (sortOrder === 'name') return a.container.name.localeCompare(b.container.name,'zh-CN');
    if (['writable-desc','logical-desc'].includes(sortOrder)) {
      const av = sortOrder === 'writable-desc' ? a.writable.allocated : a.container.size_rw;
      const bv = sortOrder === 'writable-desc' ? b.writable.allocated : b.container.size_rw;
      return Number(bv != null)-Number(av != null) || (bv || 0)-(av || 0) || a.container.name.localeCompare(b.container.name,'zh-CN');
    }
    if (a.known !== b.known) return a.known ? -1 : 1;
    const delta = sortOrder === 'total-desc' ? (b.exclusive+b.shared)-(a.exclusive+a.shared) : sortOrder === 'shared-desc' ? b.shared-a.shared : sortOrder === 'exclusive-asc' ? a.exclusive-b.exclusive : b.exclusive-a.exclusive;
    return delta || a.container.name.localeCompare(b.container.name,'zh-CN');
  });
  return rows;
}
function renderContainers() {
  const rows = visibleContainers(), pages = Math.max(1, Math.ceil(rows.length/pageSize));
  tablePage = Math.max(0, Math.min(tablePage, pages-1));
  $('containersTitle').firstChild ? $('containersTitle').firstChild.textContent = ownerFilter === null ? '全部容器 ' : `${ownerFilter || '待归属'} 的容器 ` : null;
  $('filteredCount').textContent = rows.length;
  $('containerSubtitle').textContent = ownerFilter === null ? '点击容器查看存储明细与归属' : '已按用户筛选 · 点击左侧「全部」清除';
  $('tableBody').innerHTML = rows.slice(tablePage*pageSize,(tablePage+1)*pageSize).map(r => {
    const c = r.container;
    return `<tr><td><button class="container-name" data-container="${esc(c.id)}">${esc(c.name)}</button><span class="sub image-name" title="${esc(c.image)}">${esc(c.image || c.id.slice(0,12))}</span></td><td><button class="owner-tag ${!r.owner ? 'unassigned' : ''}" data-owner="${esc(r.owner)}">${esc(r.owner || '待归属')}</button></td><td><span class="pill ${c.state === 'running' ? 'running' : ''}">${esc(containerStates[c.state] || c.state || '未知')}</span></td><td class="numeric">${writableSize(r)}</td><td class="numeric">${amount(r.exclusive,r)}</td><td class="numeric shared-amount">${amount(r.shared,r)}</td><td><button class="row-open" data-container="${esc(c.id)}" aria-label="查看 ${esc(c.name)} 的详情">↗</button></td></tr>`;
  }).join('') || `<tr><td colspan="7" class="empty"><h3>${snapshot.containers.length ? '没有匹配的容器' : '本次扫描未发现容器'}</h3><p>${snapshot.containers.length ? '调整搜索、用户或状态筛选后重试。' : '检查扫描配置中的 Docker 自动发现，并确认扫描服务可以访问本机 Docker。'}</p>${snapshot.containers.length ? '<button data-clear-filters>清除筛选</button>' : ''}</td></tr>`;
  $('tableFoot').textContent = `${rows.length ? `${tablePage*pageSize+1}–${Math.min((tablePage+1)*pageSize,rows.length)} / ` : ''}${rows.length} 个容器`;
  $('pageNumber').textContent = `${tablePage+1} / ${pages}`;
  $('previousPage').disabled = tablePage === 0; $('nextPage').disabled = tablePage >= pages-1;
  renderContainerChart(rows);
}
function renderContainerChart(rows) {
  const max = rows.reduce((value,r) => Math.max(value,r.exclusive+r.shared),1);
  $('containerChart').hidden = containerView !== 'chart';
  $('containerTable').hidden = containerView !== 'table';
  document.querySelectorAll('[data-container-view]').forEach(button => button.setAttribute('aria-pressed',String(button.dataset.containerView === containerView)));
  $('containerChart').innerHTML = `<div class="chart-caption"><span>容器引用的已统计空间</span><span><i style="--swatch:#4361d8"></i>独占 <i class="shared-fill" style="--swatch:#9472cd"></i>共享引用</span></div>` + (rows.slice(tablePage*pageSize,(tablePage+1)*pageSize).map(r => {
    const c = r.container;
    return `<button class="container-chart-row" data-container="${esc(c.id)}"><span class="chart-row-heading"><span class="chart-container-label"><i class="container-dot" style="--swatch:${containerColors.get(c.id)}"></i><b>${esc(c.name)}</b><span class="pill ${c.state === 'running' ? 'running' : ''}">${esc(containerStates[c.state] || c.state || '未知')}</span></span><strong>${fmt(r.known ? r.exclusive+r.shared : null)} <span aria-hidden="true">↗</span></strong></span><span class="chart-track"><i style="width:${percent(r.exclusive,max)}%;--swatch:${containerColors.get(c.id)}"></i><i class="shared-fill" style="width:${percent(r.shared,max)}%;--swatch:#9472cd"></i></span><span class="chart-row-meta"><span>${esc(r.owner || '待归属')}${r.partial ? ' · 部分统计' : ''}</span><span>独占 ${fmt(r.known ? r.exclusive : null)}<span class="meta-divider">/</span>共享 ${fmt(r.known ? r.shared : null)}</span></span></button>`;
  }).join('') || `<div class="empty"><h3>${snapshot.containers.length ? '没有匹配的容器' : '本次扫描未发现容器'}</h3><p>${snapshot.containers.length ? '调整搜索、用户或状态筛选后重试。' : '启用 Docker 自动发现后重新扫描。'}</p>${snapshot.containers.length ? '<button data-clear-filters>清除筛选</button>' : ''}</div>`);
}
function renderScope() {
  const layers = [...usage.containers.values()], complete = layers.filter(r => r.writable.status === 'complete').length;
  const logical = layers.filter(r => r.container.size_rw != null);
  $('diskAccounting').innerHTML = `<div class="panel-heading"><div><h2>整盘占用对账</h2><p>扫描方式：${snapshot.scan && snapshot.scan.backend === 'docker' ? 'Docker 只读辅助容器' : '宿主机直接扫描'} · 可写层完整统计 ${complete} / ${layers.length} 个 · Docker 逻辑合计 ${fmt(logical.length ? logical.reduce((sum,r) => sum+r.container.size_rw,0) : null)}（${logical.length} 个有数据）</p></div></div><div class="table-scroll"><table><thead><tr><th>文件系统</th><th class="numeric">整盘已用</th><th class="numeric">已扫描实际占用</th><th class="numeric">尚未解释</th></tr></thead><tbody>${snapshot.filesystems.map(d => `<tr><td><span class="mono">${esc(d.mount)}</span><span class="sub">${esc(d.fs)} · 设备 ${esc(d.device)}</span></td><td class="numeric">${fmt(d.used)}</td><td class="numeric">${fmt(d.scanned)}</td><td class="numeric incomplete">${d.unexplained < 0 ? `扫描超出 ${fmt(-d.unexplained)}` : fmt(d.unexplained)}</td></tr>`).join('') || '<tr><td colspan="4" class="empty">未取得文件系统容量</td></tr>'}</tbody></table></div><p class="table-note">已扫描 ${fmt(snapshot.tree.allocated)} = 容器独占 ${fmt(usage.exclusive)} + 容器共享 ${fmt(usage.shared)} + 未关联容器 ${fmt(usage.unrelated)}。可写层已包含在这些用量中。</p><p class="table-note">尚未解释 = 同一文件系统已用 − 已扫描分配空间。要核对整盘，请将对应挂载点（如 /）加入宿主机目录，并启用完整 Docker 数据目录扫描；可使用 Docker 只读辅助容器读取宿主机目录。差额还可能包含已删除但仍打开的文件、文件系统元数据或扫描期间变化。Docker 逻辑大小不计入对账。</p>`;
  $('warningSummary').textContent = `扫描范围与说明${snapshot.warnings.length ? ` · ${snapshot.warnings.length} 条提示` : ''}`;
  $('scopeSummary').innerHTML = `<p>已扫描 ${fmt(snapshot.tree.allocated)}，其中容器独占 ${fmt(usage.exclusive)}、容器共享 ${fmt(usage.shared)}、未关联容器 ${fmt(usage.unrelated)}。未关联部分可能包括附加宿主机目录、空闲卷或镜像缓存。</p>` + snapshot.filesystems.map(d => `<p><span class="mono">${esc(d.mount)}</span> · 整盘已用 ${fmt(d.used)} / ${fmt(d.total)} · 可用 ${fmt(d.available)}</p>`).join('');
  const notes = ['用量为磁盘实际分配空间（st_blocks × 512），不是 CPU、内存或 GPU 使用率。扫描结果不代表实时状态。','父子挂载和硬链接按已扫描的物理数据去重；跨用户共享不按人数分摊，也不归给任一用户。归属表示容器引用关系，不代表文件创建者。','排除目录和读取失败的部分不参与完整用量；目录明细被折叠时，已统计的空间仍计入汇总。Docker SizeRw 是逻辑大小，单独展示。'];
  $('warningList').innerHTML = notes.map(n => `<li>${esc(n)}</li>`).join('') + snapshot.warnings.map(w => `<li><span class="mono">${esc(w.path)}</span> ${esc(w.message)}</li>`).join('');
}
function select(type, value) {
  if (!['container','host'].includes(type)) return;
  const next = type === 'host' ? HOST : value;
  if (selected !== next) explorer = null;
  selected = next; renderDetail();
  if (!$('containerDialog').open) {
    $('containerDialog').showModal();

  }
}
function explorerSources() {
  return selected === HOST ? Usage.hostSources(snapshot,usage) : Usage.sources(snapshot,usage,selected);
}
function hostOnly() { return selected === HOST && explorer.source === 0; }
function explorerSize(node) { return Usage.directorySize(usage,node,hostOnly()); }
function renderHostDetail() {
  $('detailTitle').textContent = `Host · ${snapshot.host || '宿主机'}`;
  $('closeContainer').setAttribute('aria-label','关闭宿主机详情');
  if (!explorer || explorer.type !== 'host') explorer = {type:'host',source:0,trail:[],page:0,focus:null,query:''};
  $('detail').innerHTML = `<div class="detail-summary"><div><div class="container-meta"><span class="pill">宿主机存储</span></div><p class="detail-image">浏览附加宿主机目录、未使用的数据卷、镜像与缓存等已扫描空间。</p></div><div class="detail-totals"><div><span>未关联容器</span><span class="amount">${fmt(usage.unrelated)}</span></div><div><span>全部已扫描</span><span class="amount">${fmt(snapshot.tree.allocated)}</span></div></div></div>
    <div class="storage-explorer"><aside class="source-sidebar"><h3>查看范围</h3><p>选择占用范围，逐层查看目录</p><div id="storageSources"></div><p class="source-footnote">未关联容器按已知引用关系扣除容器数据。全部已扫描包含容器占用，两个范围不可相加。</p></aside><section class="explorer-main" aria-label="宿主机目录空间分析"><div id="explorerContent"></div></section></div>
    <p class="storage-note">这里只展示本次扫描覆盖的路径。未扫描目录、已删除但仍打开的文件及文件系统元数据等差额，见首页整盘占用对账。要查看其他宿主机目录，请在扫描配置中添加路径后重新扫描。</p>`;
  renderExplorer();
}
function renderDetail() {
  if (selected === HOST) { renderHostDetail(); return; }
  const c = selected, row = usage.containers.get(c.id);
  if (!row) return;
  $('detailTitle').textContent = c.name;
  $('closeContainer').setAttribute('aria-label','关闭容器详情');
  const admin = platform.user && platform.user.role === 'admin';
  if (!explorer || explorer.id !== c.id) {
    const sources = Usage.sources(snapshot,usage,c);
    const initial = sources.findIndex(s => s.known);
    explorer = {id:c.id,source:Math.max(0,initial),trail:[],page:0,focus:null,query:''};
  }
  $('detail').innerHTML = `<div class="detail-summary"><div><div class="container-meta"><span class="pill ${c.state === 'running' ? 'running' : ''}">${esc(containerStates[c.state] || c.state || '未知')}</span><span>${esc(row.owner || '待归属')}</span><span class="mono">${esc(c.id.slice(0,12))}</span>${admin ? `<button class="text-button" data-edit-owner="${esc(c.id)}">修改归属</button>` : ''}</div><p class="detail-image mono">${esc(c.image)}</p></div><div class="detail-totals"><div><span>独占空间</span>${amount(row.exclusive,row)}</div><div><span>共享引用</span>${amount(row.shared,row)}</div></div></div>
    <div class="storage-explorer"><aside class="source-sidebar"><h3>存储来源</h3><p>选择来源，查看内部明细</p><div id="storageSources"></div><p class="source-footnote">来源可能重叠，不可相加。可写层不包含镜像只读层。</p></aside><section class="explorer-main" aria-label="目录空间分析"><div id="explorerContent"></div></section></div>
    <details class="logical-note"><summary>可写层统计与 Docker 逻辑大小</summary><p>可写层已统计实际占用：${writableSize(row)}</p>${row.writable.reason ? `<p>${esc(row.writable.reason)}</p>` : ''}<p>Docker SizeRw：<strong>${fmt(c.size_rw)}</strong>。此值未加到实际磁盘占用中。可写层与挂载按存储来源分别浏览，不是容器合并后的完整文件系统。</p></details>`;
  renderExplorer();
}
function renderExplorer() {
  const sources = explorerSources(), source = sources[explorer.source];
  if (!source) return;
  $('storageSources').innerHTML = sources.map((s,index) => `<button class="source-button ${index === explorer.source ? 'active' : ''}" data-storage-source="${index}" aria-pressed="${index === explorer.source}"><span><i aria-hidden="true">${s.type === 'writable' ? '▣' : s.type === 'logs' ? '≡' : '▱'}</i><b>${esc(s.label)}</b></span><strong>${s.accessOnly ? '仅访问' : fmt(s.known ? (s.bytes != null ? s.bytes : s.node.allocated) : null)}</strong><small>${esc(s.hint)}${s.partial ? ' · 部分统计' : ''}</small></button>`).join('');
  if (!source.known) {
    const reason = source.type === 'writable' ? usage.containers.get(selected.id).writable.reason : source.node && source.node.reason;
    $('explorerContent').innerHTML = `<div class="explorer-empty"><span aria-hidden="true">▱</span><h3>此来源暂无可读取的目录明细</h3><p>${esc(reason || (source.path ? '该路径未保留在扫描结果中，或存在权限不足、排除项。' : '未采集到该来源的持久磁盘路径。'))}</p><p>用量为未知；可选择其他来源，或调整扫描范围后重新扫描。</p>${source.path ? `<p class="mono">${esc(source.path)}</p>` : ''}</div>`;
    return;
  }
  if (!explorer.trail.length || !usage.nodes.has(explorer.trail[0].path)) explorer.trail = [{path:source.node.path, label:source.destination || '/', display:source.destination || '/'}];
  const current = explorer.trail[explorer.trail.length-1], status = usage.inspect(current.path);
  if (!status.known) { explorer.trail = []; renderExplorer(); return; }
  const node = status.node;
  if (usage.remote && !node.loaded) {
    explorer.entries = [];
    $('explorerContent').innerHTML = '<p class="explorer-empty" role="status">正在读取目录明细…</p>';
    loadDirectoryView(node.path);
    return;
  }
  explorer.entries = Usage.directoryEntries(usage,node,hostOnly());
  if (explorer.focusKey) {
    const index = explorer.entries.findIndex(e => (e.path || e.kind) === explorer.focusKey);
    explorer.focus = index < 0 ? null : index;
    explorer.focusKey = null;
  }
  const isDirectory = ['directory','root'].includes(node.kind);
  const title = source.accessOnly ? '宿主机访问范围 · 不计入容器用量' : source.type === 'host' ? '宿主机目录明细' : source.type === 'writable' ? '可写层目录明细' : source.type === 'logs' ? '日志目录明细' : isDirectory ? '挂载目录明细' : '挂载文件明细';
  $('explorerContent').innerHTML = `<div class="explorer-title"><div><h3>${title}</h3><p>面积代表实际占用 · 点击目录进入，点击灰块扫描</p></div><strong>${fmt(node.size_unknown ? null : explorerSize(node).allocated)}<small>${hostOnly() ? '当前范围 · 未关联容器' : node.kind === 'root' ? '全部已扫描' : isDirectory ? '当前目录' : '当前文件'}</small></strong></div>
    <nav class="explorer-breadcrumbs" aria-label="目录路径"><button data-storage-up ${explorer.trail.length < 2 ? 'disabled' : ''} aria-label="返回上级目录">↑</button>${explorer.trail.map((step,index) => `<span aria-hidden="true">/</span><button data-storage-crumb="${index}" ${index === explorer.trail.length-1 ? 'aria-current="location"' : ''}>${esc(index === 0 ? source.label : step.label)}</button>`).join('')}</nav>
    <p class="explorer-path mono" title="${esc(node.kind === 'root' ? '本次扫描覆盖的路径' : node.path)}">${['host','logs'].includes(source.type) ? '宿主机' : '容器内'}：${esc(current.display)}</p>
    ${hostOnly() ? `<p class="explorer-notice">仅展示未关联容器的已统计空间，已扣除容器引用的数据。${usage.attributionLimited ? '部分目录或共享引用未保留，归属可能随继续分析而更新。' : ''}可切换“全部已扫描目录”查看完整路径范围。</p>` : ''}
    ${node.errors || node.excluded_entries ? '<p class="explorer-notice">部分内容读取失败或已排除，保留已知占用。</p>' : ''}
    ${current.reference ? '<p class="explorer-notice">正在查看引用目标的明细。这份空间已在其他路径记账，不会重复计入空间用量。</p>' : ''}
    <div class="map-toolbar"><label><span class="sr-only">搜索图中区块</span><input id="mapSearch" type="search" placeholder="搜索图中区块" value="${esc(explorer.query)}"></label><div id="mapGroupNavigation"></div></div>
    <div id="directoryDepthControl" class="directory-depth" hidden><label for="directoryDepth">单次探索深度</label><select id="directoryDepth" aria-describedby="directoryDepthHint">${Array.from({length:32},(_,i)=>`<option value="${i+1}">${i+1} 层</option>`).join('')}</select><small id="directoryDepthHint">一次保留多层明细，减少下钻时重复扫描。</small></div>
    <div class="map-surface"><div id="directoryMap" class="directory-map" aria-label="当前目录空间地图"></div><div id="directorySelection" class="map-inspector" hidden></div></div>
    <div id="mapUnmeasured" class="map-unmeasured"></div>
    <div id="directoryStatus" class="map-status" role="status" aria-live="polite"></div>
    <p class="map-note">点击目录进入 · 点击灰块扫描 · 点击“其他”展开小项。已知占用按面积显示；未知和零占用另标状态。</p>
    ${node.kind === 'root' ? '' : `<details class="physical-path"><summary>查看宿主机记账路径</summary><p class="mono">${esc(node.path)}</p>${source.path !== source.node.path ? `<p>此来源通过引用指向同一份数据：<span class="mono">${esc(source.path)}</span>。以上大小为目标的已统计范围，不新增空间。</p>` : ''}</details>`}`;
  $('directoryDepth').value = String(platform.expandDepth || 3);
  $('directoryDepth').addEventListener('change',e => {
    const depth = Number(e.target.value);
    if (Number.isInteger(depth) && depth >= 1 && depth <= 32) platform.expandDepth = depth;
  });
  renderDirectoryMap(); refreshDirectoryScan();
  if (explorer.focus != null) showEntry(explorer.focus);
  $('mapSearch').addEventListener('input',e => { explorer.query = e.target.value; explorer.page = 0; explorer.unknownPage = 0; renderDirectoryMap(); });
}
let directoryViewLoad = null;
async function loadDirectoryView(path) {
  if (directoryViewLoad?.path === path && directoryViewLoad.base === snapshot) return;
  if (directoryViewLoad) directoryViewLoad.controller.abort();
  const pending={path,base:snapshot,controller:new AbortController(),generation:platform.generation,sequence:platform.loadSequence};
  directoryViewLoad=pending;
  const current=()=>directoryViewLoad===pending && !pending.controller.signal.aborted && snapshot===pending.base && platform.generation===pending.generation && platform.loadSequence===pending.sequence && explorer?.trail[explorer.trail.length-1]?.path===path;
  try {
    const prepared=await SnapshotLoader.read(viewURL(snapshot.job_id,true,path),{signal:pending.controller.signal});
    if (!current()) return;
    load(prepared.data,$('sourceBadge').textContent,prepared.usage,true);
  } catch (error) {
    if (!current() || error.name==='AbortError') return;
    $('explorerContent').innerHTML=`<div class="explorer-empty" role="status"><p>${esc(error.message)}</p><button data-retry-directory>重试读取目录</button></div>`;
  } finally {
    if (directoryViewLoad===pending) directoryViewLoad=null;
  }
}
function renderDirectoryMap() {
  const map = $('directoryMap');
  if (!map || !explorer || !explorer.entries) return;
  const entries = explorer.entries.map((entry,index) => ({...entry,index})).filter(e => e.name.toLowerCase().includes(explorer.query.trim().toLowerCase()));
  const positive = entries.filter(e => e.bytes > 0), unknown = entries.filter(e => !(e.bytes > 0));
  explorer.page = Math.max(0,Math.min(explorer.page,Math.max(0,Math.ceil(positive.length/47)-1)));
  const visible = Usage.mapEntries(positive.slice(explorer.page*47));
  const height = map.clientWidth > 0 ? 100*map.clientHeight/map.clientWidth : 60;
  const tiles = Usage.treemap(visible,100,height);
  $('mapGroupNavigation').innerHTML = explorer.page ? '<button data-map-reset>← 返回全部区块</button>' : `<span>${entries.length} 项</span>`;
  map.innerHTML = tiles.map(tile => {
    const small = tile.w < 14 || tile.h/height < .2, tiny = tile.w < 6 || tile.h/height < .1;
    const hint = tile.pending ? '点击扫描并拆分' : tile.kind === 'group' ? '点击展开小项' : tile.node && tile.node.kind === 'directory' ? '点击进入目录' : '点击查看详情';
    const title = `${tile.name} · ${fmt(tile.bytes)} · ${hint}`;
    return `<button class="map-tile ${tile.kind === 'residual' || tile.kind === 'group' ? 'residual-tile' : ''} ${small ? 'small-tile' : ''} ${tiny ? 'tiny-tile' : ''}" ${tile.kind === 'group' ? 'data-map-group' : `data-storage-entry="${tile.index}"`} style="left:${tile.x}%;top:${tile.y/height*100}%;width:${tile.w}%;height:${tile.h/height*100}%;--tile:${colorFor(tile.path || tile.kind)}" title="${esc(title)}" aria-label="${esc(title)}"><span>${esc(tile.name)}</span><strong>${fmt(tile.bytes)}</strong>${!small ? `<small>${hint} ↗</small>` : ''}</button>`;
  }).join('') || '<div class="map-empty">'+(unknown.length ? '此范围暂无已知占用，点击下方状态区块查看。' : explorer.query ? '没有匹配的区块' : '此目录为空')+'</div>';
  explorer.unknownPage = Math.max(0,Math.min(explorer.unknownPage || 0,Math.max(0,Math.ceil(unknown.length/48)-1)));
  $('mapUnmeasured').innerHTML = unknown.slice(explorer.unknownPage*48,(explorer.unknownPage+1)*48).map(e=>`<button class="map-state-tile" data-storage-entry="${e.index}"><b>${esc(e.name)}</b><span>${e.pending ? '点击扫描' : e.bytes === 0 ? '0 B · '+entryKind(e) : entryKind(e)}</span></button>`).join('') + (unknown.length>48 ? `<button data-map-unmeasured>更多状态区块 · ${explorer.unknownPage+1} / ${Math.ceil(unknown.length/48)}</button>` : '');
}

function entryKind(entry) {
  if (entry.node && entry.node.size_unknown) return '目录 / 文件 · 用量待分析';
  if (entry.kind === 'reference') return '引用 · 已在其他路径记账';
  if (entry.kind === 'residual') return '汇总 · 不含上方已列出的子项';
  if (!entry.known) return entry.kind === 'excluded' ? '已排除 · 大小未知' : '无法读取 · 大小未知';
  return `${entry.kind === 'directory' ? '目录' : entry.kind === 'symlink' ? '符号链接 · 不跟随' : '文件'}${entry.partial ? ' · 部分统计' : ''}`;
}
function showEntry(index) {
  const entry = explorer.entries[index];
  if (!entry) return;
  explorer.focus = index;
  const extra = entry.kind === 'reference' ? `引用目标：${entry.node ? entry.node.path : '目标未保留或存在循环'}。本路径已去重；查看目标不会新增空间。` : entry.pending ? '点击此占用块扫描当前目录；读到的子目录及其占用会实时拆分显示。' : entry.kind === 'residual' ? '这些空间包括目录自身和未展开的明细，已计入目录总量，不代表可回收空间。' : entry.node && entry.node.reason || '';
  $('directorySelection').hidden = false;
  $('directorySelection').innerHTML = `<button class="map-inspector-close" data-close-map-detail aria-label="关闭区块详情">×</button><div class="entry-inspector"><strong>${esc(entry.name)}</strong><span>${entryKind(entry)} · 实际占用 ${fmt(entry.bytes)}${entry.node && entry.kind !== 'reference' ? ` · 逻辑大小 ${fmt(entry.known ? explorerSize(entry.node).apparent : null)}` : ''}</span>${extra ? `<p>${esc(extra)}</p>` : ''}${entry.path ? `<p class="mono">${esc(entry.path)}</p>` : ''}</div>`;
  document.querySelectorAll('.map-tile[data-storage-entry], .map-state-tile[data-storage-entry]').forEach(el => el.classList.toggle('entry-selected',Number(el.dataset.storageEntry) === index));
}
function refreshDirectoryScan() {
  if (!selected || !explorer || !explorer.trail.length || !$('directoryStatus')) return;
  const p = platform, current = explorer.trail[explorer.trail.length-1].path;
  if (!p.user) { $('directoryStatus').innerHTML=''; return; }
  const job = p.jobs.find(j => j.trigger === 'incremental' && j.config && j.config.base_job_id === snapshot.job_id && (j.config.incremental_path === current || current.startsWith(j.config.incremental_path.replace(/\/$/,'')+'/')));
  const busy = job && ['queued','running','cancelling'].includes(job.status);
  const failed = job && ['failed','cancelled','interrupted'].includes(job.status);
  const progress = job && job.progress || {};
  const reading = p.changesLoad && p.changesLoad.id === snapshot.job_id;
  const readError = p.changesError && p.changesError.id === snapshot.job_id ? p.changesError.message : '';
  const error = p.expandError && p.expandError.job === snapshot.job_id && p.expandError.path === current ? p.expandError.message : failed ? job.error || '扫描已停止，已读取的明细已保留。' : '';
  const pending = explorer.entries.some(e=>e.pending);
  $('directoryDepthControl').hidden = p.user.role !== 'admin' || !pending;
  $('directoryDepth').disabled = !!(p.active || p.expandStarting || reading || readError);
  const text = busy ? `${job.status === 'cancelling' ? '正在停止扫描' : '正在扫描，区块实时更新'} · ${(progress.entries || 0).toLocaleString('zh-CN')} 项已遍历` : p.expandStarting === current ? '正在启动扫描…' : error ? `${error} 点击灰块重试。` : p.user.role !== 'admin' && pending ? '待分析区块需要管理员扫描。' : pending ? '点击灰块，读取并拆分这部分占用。' : '';
  $('directoryStatus').innerHTML = `<span>${esc(text)}</span>${busy && p.user.role === 'admin' ? `<button data-cancel-expansion="${esc(job.id)}" ${job.status === 'cancelling' ? 'disabled' : ''}>停止扫描</button>` : ''}${reading ? '<span>正在读取新增目录明细…</span>' : ''}${readError ? `<span class="error-text">${esc(readError)}</span><button data-retry-changes>重试读取明细</button>` : ''}`;
}
function focusBreadcrumb() {
  const content = $('explorerContent');
  const crumb = content.querySelector('[aria-current="location"]');
  if (crumb) crumb.focus({preventScroll:true});
}
function openDirectoryEntry(index) {
  if (!selected || !explorer || !explorer.entries) return;
  const entry = explorer.entries[index];
  if (!entry) return;
  if ((entry.known || entry.node && entry.node.size_unknown) && entry.node && entry.node.kind === 'directory') {
    if (explorer.trail.some(step => step.path === entry.node.path)) { showEntry(index); return; }
    const parent = explorer.trail[explorer.trail.length-1];
    explorer.trail.push({path:entry.node.path,label:entry.name,display:parent.path === snapshot.tree.path ? entry.path : `${parent.display.replace(/\/$/,'')}/${entry.name}`,reference:entry.kind === 'reference'});
    explorer.page = 0; explorer.focus = null; explorer.query = ''; renderExplorer();
    focusBreadcrumb();
  } else {
    if (entry.kind === 'residual' && entry.pending) {
      explorer.focus = null; $('directorySelection').hidden = true;
      if (platform.user && platform.user.role === 'admin') expandLeaf(explorer.trail[explorer.trail.length-1].path);
      refreshDirectoryScan();
    } else showEntry(index);
  }
}
function filterOwner(owner) { ownerFilter = owner; tablePage = 0; render(); }
function clearFilters() {
  query = ''; ownerFilter = null; stateFilter = 'all'; tablePage = 0;
  $('search').value = ''; $('stateFilter').value = 'all'; render();
}
document.addEventListener('click', e => {
  const button = e.target.closest('button');
  if (!button || !snapshot) return;
  if (button.dataset.host !== undefined) select('host');
  if (button.dataset.container) { const c = snapshot.containers.find(c => c.id === button.dataset.container); if (c) select('container',c); }
  if (button.dataset.owner !== undefined) filterOwner(ownerFilter === button.dataset.owner ? null : button.dataset.owner);
  if (button.dataset.clearFilters !== undefined) clearFilters();
  if (button.dataset.containerView) { containerView = button.dataset.containerView; renderContainers(); }
  if (!selected || !explorer) return;
  if (button.dataset.storageSource !== undefined) {
    const index = Number(button.dataset.storageSource);
    if (!explorerSources()[index]) return;
    explorer.source = index; explorer.trail = []; explorer.page = 0; explorer.focus = null; explorer.query = ''; renderExplorer();
    const sources = $('storageSources');
    sources.querySelector('[aria-pressed="true"]').focus({preventScroll:true});
  }
  if (button.dataset.retryDirectory !== undefined) renderExplorer();
  if (button.dataset.storageEntry !== undefined) openDirectoryEntry(Number(button.dataset.storageEntry));
  if (button.dataset.storageUp !== undefined || button.dataset.storageCrumb !== undefined) {
    const length = button.dataset.storageUp !== undefined ? explorer.trail.length-1 : Number(button.dataset.storageCrumb)+1;
    if (length > 0 && length <= explorer.trail.length) { explorer.trail = explorer.trail.slice(0,length); explorer.page = 0; explorer.focus = null; explorer.query = ''; renderExplorer(); focusBreadcrumb(); }
  }
  if (button.dataset.mapGroup !== undefined) { explorer.page++; renderDirectoryMap(); }
  if (button.dataset.mapReset !== undefined) { explorer.page=0; renderDirectoryMap(); }
  if (button.dataset.mapUnmeasured !== undefined) { explorer.unknownPage++; const count=explorer.entries.filter(e=>!(e.bytes>0) && e.name.toLowerCase().includes(explorer.query.trim().toLowerCase())).length; if(explorer.unknownPage*48>=count)explorer.unknownPage=0; renderDirectoryMap(); }
  if (button.dataset.closeMapDetail !== undefined) { explorer.focus=null; $('directorySelection').hidden=true; }

});
$('closeContainer').addEventListener('click', () => $('containerDialog').close());
$('containerDialog').addEventListener('close', () => {
  // Native close events are queued and can arrive after the dialog reopens.
  if (!$('containerDialog').open) { selected = null; explorer = null; }
});
$('clearOwner').addEventListener('click', () => filterOwner(null));
$('unassignedFilter').addEventListener('click', () => filterOwner(ownerFilter === Usage.UNASSIGNED ? null : Usage.UNASSIGNED));
$('stateFilter').addEventListener('change', e => { stateFilter = e.target.value; tablePage = 0; renderContainers(); });
$('sortOrder').addEventListener('change', e => { sortOrder = e.target.value; tablePage = 0; renderContainers(); });
$('search').addEventListener('input', e => { query = e.target.value.trim().toLowerCase(); tablePage = 0; if (snapshot) renderContainers(); });
$('previousPage').addEventListener('click', () => { tablePage--; renderContainers(); });
$('nextPage').addEventListener('click', () => { tablePage++; renderContainers(); });

let mapResizeTimer;
window.addEventListener('resize', () => {
  clearTimeout(mapResizeTimer);
  mapResizeTimer = setTimeout(() => { if (selected && explorer) { renderDirectoryMap(); if (explorer.focus != null) showEntry(explorer.focus); } },120);
});
