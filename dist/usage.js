'use strict';
// Partition the already inode-deduplicated scan into disjoint byte ranges.
// A range belongs to every container referencing an enclosing resource, including
// references through directory aliases/hard links. Shared bytes are never split.
const Usage = (() => {
  const UNASSIGNED = '';
  const ownerOf = c => typeof c.owner === 'string' && c.owner.trim() && c.owner.trim() !== '未标注' ? c.owner.trim() : UNASSIGNED;
  const parentPath = path => path === '/' ? null : path.slice(0, path.lastIndexOf('/')) || '/';
  function restore(model) {
    model.inspect = path => {
      const node = resolveNode(model.nodes,path);
      return {node, known:!!node && !['excluded','unreadable'].includes(node.kind), partial:!node || model.incomplete.has(node.path)};
    };
    return model;
  }
  function resolveNode(nodes,path) {
    let node = nodes.get(path);
    const seen = new Set();
    while (node && node.kind === 'reference') {
      if (seen.has(node.path)) return null;
      seen.add(node.path); node = nodes.get(node.reference);
    }
    return node;
  }
  function build(data, report = () => {}) {
    const nodes = new Map(), resources = new Map(), containers = new Map(), owners = new Map();
    report({stage:'index',done:0,total:null,unit:'节点',detail:'建立目录与容器索引'});
    for (const c of data.containers) {
      const owner = ownerOf(c);
      const row = {container:c, owner, exclusive:0, shared:0, known:false, partial:false};
      containers.set(c.id, row);
      if (!owners.has(owner)) owners.set(owner, {owner, bytes:0, shared:0, containers:[], partial:false, known:false});
      owners.get(owner).containers.push(c.id);
    }
    const stack = [data.tree];
    while (stack.length) {
      const node = stack.pop(); nodes.set(node.path, node);
      for (const child of node.children) stack.push(child);
      if (nodes.size % 1024 === 0) report({stage:'index',done:nodes.size,total:null,unit:'节点',detail:'建立目录与容器索引',current:node.path});
    }
    report({stage:'index',done:nodes.size,total:nodes.size,unit:'节点',detail:'目录与容器索引已建立'});
    report({stage:'relations',done:0,total:nodes.size,unit:'节点',detail:'解析容器挂载与共享关系'});
    function addResource(path, ids) {
      if (!path) return;
      if (!resources.has(path)) resources.set(path, new Set());
      for (const id of ids) if (containers.has(id)) resources.get(path).add(id);
    }
    for (const r of data.resources) addResource(r.path, r.containers);
    const membership = new Map();
    function members(path) {
      if (!path || path === '@root') return new Set();
      if (membership.has(path)) return membership.get(path);
      const ids = new Set(members(parentPath(path)));
      for (const id of resources.get(path) || []) ids.add(id);
      membership.set(path, ids);
      return ids;
    }
    const resolve = path => resolveNode(nodes,path);
    const claims = new Map(), references = [];
    for (const node of nodes.values()) {
      claims.set(node.path, members(node.path));
      if (node.kind === 'reference') references.push(node);
      if (claims.size % 1024 === 0) report({stage:'relations',done:claims.size,total:nodes.size,unit:'节点',detail:'解析容器挂载与共享关系',current:node.path});
    }
    // Propagate alias membership through the target subtree and any hard-link
    // references inside it. Requeue changed references until no claims change.
    let referenceWork = 0;
    report({stage:'relations',done:0,total:null,unit:'项引用',detail:'核对硬链接与目录别名'});
    while (references.length) {
      const reference = references.pop(), target = resolve(reference.path);
      if (!target) continue;
      const incoming = claims.get(reference.path), pending = [target];
      while (pending.length) {
        const node = pending.pop(), existing = claims.get(node.path);
        if (++referenceWork % 1024 === 0) report({stage:'relations',done:referenceWork,total:null,unit:'项引用',detail:'核对硬链接与目录别名',current:node.path});
        if ([...incoming].every(id => existing.has(id))) continue;
        claims.set(node.path, new Set([...existing, ...incoming]));
        if (node.kind === 'reference') references.push(node);
        for (const child of node.children) pending.push(child);
      }
    }
    const incomplete = new Set();
    let checked = 0;
    // Reverse DFS order ensures children are visited before parents.
    for (const node of [...nodes.values()].reverse()) {
      if (node.scanning || node.size_unknown || node.errors || node.excluded_entries || ['excluded','unreadable'].includes(node.kind) || (node.kind === 'reference' && !resolve(node.path)) || node.children.some(child => incomplete.has(child.path))) incomplete.add(node.path);
      if (++checked % 1024 === 0) report({stage:'relations',done:checked,total:nodes.size,unit:'节点',detail:'核对读取异常与统计完整性',current:node.path});
    }
    function inspect(path) {
      const node = resolve(path);
      return {node, known:!!node && !['excluded','unreadable'].includes(node.kind), partial:!node || incomplete.has(node.path)};
    }
    for (const [path, ids] of resources) {
      const status = inspect(path);
      for (const id of ids) {
        const row = containers.get(id);
        row.known = row.known || status.known;
        row.partial = row.partial || status.partial;
      }
    }
    for (const row of containers.values()) {
      if (!row.container.upper_path || row.container.mounts.some(m => ['bind','volume'].includes(m.type) && !m.source)) row.partial = true;
      const layer = row.container.writable_layer;
      row.writable = {...layer, known:layer.allocated != null};
      if (row.writable.status !== 'complete') row.partial = true;
    }
    const attributionLimited = data.scan.lazy_accounting_limited || data.scan.omitted_references > 0;
    if (attributionLimited) for (const row of containers.values()) row.partial = true;
    report({stage:'relations',done:nodes.size,total:nodes.size,unit:'节点',detail:'挂载、引用与完整性核对完成'});
    let exclusive = 0, shared = 0, crossOwner = 0, unrelated = 0;
    const host = new Map();
    let aggregated = 0;
    report({stage:'aggregate',done:0,total:nodes.size,unit:'节点',detail:'计算容器独占、共享与用户用量'});
    for (const node of [...nodes.values()].reverse()) {
      const ids = claims.get(node.path);
      // Residual includes directory metadata and all omitted descendants.
      const bytes = Math.max(0, node.allocated - node.children.reduce((sum, child) => sum + child.allocated, 0));
      // Use the same resolved claims as the totals, including container aliases
      // into host paths. Parents retain only their unclaimed bytes and children.
      host.set(node.path, {
        allocated: (ids.size ? 0 : bytes) + node.children.reduce((sum, child) => sum + host.get(child.path).allocated, 0),
        apparent: (ids.size ? 0 : Math.max(0,node.apparent-node.children.reduce((sum, child) => sum+child.apparent,0))) + node.children.reduce((sum, child) => sum+host.get(child.path).apparent,0),
        visible: !ids.size || node.children.some(child => host.get(child.path).visible)
      });
      if (!ids.size) unrelated += bytes;
      else {
        const isShared = ids.size > 1;
        if (isShared) shared += bytes; else exclusive += bytes;
        const ownerIDs = new Set();
        for (const id of ids) {
          const row = containers.get(id);
          row[isShared ? 'shared' : 'exclusive'] += bytes;
          ownerIDs.add(row.owner);
        }
        // Unlabelled containers are not assumed to belong to the same person.
        if (ownerIDs.size === 1 && (!ownerIDs.has(UNASSIGNED) || ids.size === 1)) owners.get([...ownerIDs][0]).bytes += bytes;
        else {
          crossOwner += bytes;
          for (const owner of ownerIDs) owners.get(owner).shared += bytes;
        }
      }
      if (++aggregated % 1024 === 0) report({stage:'aggregate',done:aggregated,total:nodes.size,unit:'节点',detail:'计算容器独占、共享与用户用量',current:node.path});
    }
    for (const row of containers.values()) {
      const owner = owners.get(row.owner);
      owner.partial = owner.partial || row.partial;
      owner.known = owner.known || row.known;
    }
    report({stage:'aggregate',done:nodes.size,total:nodes.size,unit:'节点',detail:`已汇总 ${containers.size} 个容器、${owners.size} 个用户`});
    return {nodes, containers, owners, exclusive, shared, crossOwner, unrelated, attributionLimited, incomplete, host, inspect};
  }
  // Sources are reference ranges, not additive slices of container usage.
  function sources(data, model, container) {
    const result = [{label:'可写层', path:container.upper_path, destination:'/', type:'writable', hint:'容器内未挂载的写入'}];
    for (const m of container.mounts) result.push({label:m.destination, path:['bind','volume'].includes(m.type) ? m.source : null, destination:m.destination, type:m.type, hint:`${m.type} · ${m.rw ? '读写' : '只读'}`});
    const logs = new Set(data.resources.filter(r => r.kinds.includes('container-data') && r.containers.includes(container.id)).map(r => r.path));
    if (container.log_path) logs.add(parentPath(container.log_path));
    for (const path of logs) result.push({label:'日志与元数据', path, destination:path, type:'logs', hint:'宿主机路径 · 包括轮转日志'});
    return result.map(source => ({...source, ...model.inspect(source.path)}));
  }
  function hostSources(data, model) {
    return [
      {label:'Host · 未关联容器',type:'host',hostOnly:true,hint:'仅展示未被容器引用的空间'},
      {label:'全部已扫描目录',type:'host',hostOnly:false,hint:'包含容器数据，按物理路径浏览'}
    ].map(source => ({...source,path:data.tree.path,destination:'扫描范围',bytes:source.hostOnly ? model.unrelated : data.tree.allocated,...model.inspect(data.tree.path)}));
  }
  function directorySize(model, node, hostOnly = false) {
    return hostOnly ? model.host.get(node.path) : node;
  }
  function directoryEntries(model, node, hostOnly = false) {
    const size = n => directorySize(model,n,hostOnly);
    // Bind mounts can point at one file rather than a directory.
    if (!['directory','root'].includes(node.kind)) return [{name:node.name,path:node.path,bytes:size(node).allocated,kind:node.kind,...model.inspect(node.path)}];
    const entries = node.children.filter(child => !hostOnly || size(child).visible).map(child => {
      const status = model.inspect(child.path);
      const prefix = node.path.replace(/\/$/,'') + '/';
      return {name:node.kind === 'root' ? child.path : child.path.startsWith(prefix) ? child.path.slice(prefix.length) : child.name, path:child.path, bytes:status.known && !child.size_unknown ? size(child).allocated : null, kind:child.kind, ...status,known:status.known && !child.size_unknown};
    });
    const residual = Math.max(0, size(node).allocated - node.children.reduce((sum, child) => sum + (child.size_unknown ? 0 : size(child).allocated), 0));
    const pending = !!(node.omitted_entries || node.size_unknown || node.scanning);
    if (residual || pending) entries.push({name:pending ? '子目录待分析的历史占用' : '目录自身及其他已统计空间', bytes:node.size_unknown ? null : residual, kind:'residual', known:!node.size_unknown, partial:pending, pending, node:null});
    return entries.sort((a,b) => Number(b.known)-Number(a.known) || (b.bytes || 0)-(a.bytes || 0) || a.name.localeCompare(b.name,'zh-CN'));
  }
  // Bound each chart view; its group tile opens the remaining small entries.
  function mapEntries(entries, limit = 48) {
    const positive = entries.map((entry,index) => ({...entry,index:entry.index == null ? index : entry.index})).filter(e => e.bytes > 0);
    if (positive.length <= limit) return positive;
    return [...positive.slice(0,limit-1), {name:`其他 ${positive.length-limit+1} 项`, kind:'group', bytes:positive.slice(limit-1).reduce((sum,e) => sum+e.bytes,0), index:-1}];
  }
  // Balanced binary partition: area always represents allocated bytes.
  function treemap(entries, width = 100, height = 60) {
    const result = [];
    function split(items,x,y,w,h) {
      if (!items.length) return;
      if (items.length === 1) { result.push({...items[0],x,y,w,h}); return; }
      const total = items.reduce((sum,e) => sum+e.bytes,0);
      let pivot = 1, left = items[0].bytes;
      while (pivot < items.length-1 && Math.abs(left+items[pivot].bytes-total/2) < Math.abs(left-total/2)) left += items[pivot++].bytes;
      const ratio = left/total;
      if (w >= h) { split(items.slice(0,pivot),x,y,w*ratio,h); split(items.slice(pivot),x+w*ratio,y,w*(1-ratio),h); }
      else { split(items.slice(0,pivot),x,y,w,h*ratio); split(items.slice(pivot),x,y+h*ratio,w,h*(1-ratio)); }
    }
    split(entries.filter(e => e.bytes > 0),0,0,width,height);
    return result;
  }
  return {build, restore, ownerOf, UNASSIGNED, sources, hostSources, directorySize, directoryEntries, mapEntries, treemap};
})();
if (typeof module !== 'undefined' && module.exports) module.exports = Usage;
