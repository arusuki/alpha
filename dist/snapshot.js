'use strict';
// Shared by the UI and the background snapshot worker.
const SnapshotData = (() => {
  function validate(data, report = () => {}) {
    if (!data || data.schema_version !== 2 || !data.tree || !Array.isArray(data.containers) || !Array.isArray(data.resources) || !Array.isArray(data.filesystems) || !Array.isArray(data.warnings)) throw Error('这不是受支持的 project alpha 快照。');
    if (!data.scan || !Number.isSafeInteger(data.scan.omitted_references) || data.scan.omitted_references < 0 || !Number.isSafeInteger(data.revision) || data.revision < 0) throw Error('扫描统计或版本格式无效。');
    report({stage:'validate',done:0,total:null,unit:'节点',detail:'校验目录树结构与空间汇总'});
    const stack = [[data.tree, 0]], paths = new Set(), ids = new Set();
    let count = 0;
    const bytes = x => typeof x === 'number' && Number.isFinite(x) && x >= 0;
    while (stack.length) {
      const [n, depth] = stack.pop();
      if (!n || typeof n.path !== 'string' || typeof n.name !== 'string' || !bytes(n.allocated) || !bytes(n.apparent) || !bytes(n.files) || !bytes(n.errors) || !Array.isArray(n.children) || paths.has(n.path) || depth > 256 || ++count > 250000) throw Error('目录树结构无效，或超过 25 万节点 / 256 层限制。');
      paths.add(n.path);
      if (count % 1024 === 0) report({stage:'validate',done:count,total:null,unit:'节点',detail:'校验目录树结构与空间汇总',current:n.path});
      if (n.children.reduce((sum, c) => sum + (c && c.allocated || 0), 0) > n.allocated || n.children.reduce((sum, c) => sum + (c && c.apparent || 0), 0) > n.apparent) throw Error('扫描汇总与子目录用量不一致，无法可靠计算容器归属。');
      for (const c of n.children) stack.push([c, depth+1]);
    }
    for (const r of data.resources) if (!r || typeof r.path !== 'string' || !Array.isArray(r.kinds) || !Array.isArray(r.containers)) throw Error('挂载源格式无效。');
    for (const c of data.containers) {
      if (!c || typeof c.id !== 'string' || ids.has(c.id) || typeof c.name !== 'string' || !Array.isArray(c.mounts) || (c.upper_path != null && typeof c.upper_path !== 'string') || (c.log_path != null && typeof c.log_path !== 'string') || (c.size_rw != null && !bytes(c.size_rw))) throw Error('容器格式无效。');
      ids.add(c.id);
      const w = c.writable_layer;
      if (!w || !['complete','partial','unreadable','excluded','unavailable'].includes(w.status) || (w.allocated !== null && !bytes(w.allocated)) || (w.apparent !== null && !bytes(w.apparent)) || typeof w.permission_denied !== 'boolean' || (w.reason != null && typeof w.reason !== 'string')) throw Error('可写层统计格式无效。');
      for (const m of c.mounts) if (!m || typeof m.type !== 'string' || typeof m.destination !== 'string' || (m.source != null && typeof m.source !== 'string')) throw Error('容器挂载格式无效。');
    }
    for (const d of data.filesystems) if (!d || !bytes(d.total) || !bytes(d.used) || !bytes(d.available) || (d.scanned !== null && !bytes(d.scanned)) || (d.unexplained !== null && !Number.isFinite(d.unexplained))) throw Error('文件系统容量格式无效。');
    for (const w of data.warnings) if (!w || typeof w.message !== 'string') throw Error('扫描警告格式无效。');
    report({stage:'validate',done:count,total:count,unit:'节点',detail:'目录、容器、挂载与容量校验完成'});
    return data;
  }
  function applyChanges(base, changes) {
    const invalid = () => { throw Error('目录更新与当前扫描记录不一致，请刷新后重试。'); };
    if (!base || !changes || changes.job_id !== base.job_id || changes.base_revision !== base.revision || !Number.isSafeInteger(changes.revision) || changes.revision < changes.base_revision || !Array.isArray(changes.replacements) || !Array.isArray(changes.ancestors) || !changes.metadata || changes.metadata.job_id !== base.job_id || changes.metadata.revision !== changes.revision || 'tree' in changes.metadata) invalid();
    const replacements = new Map(), ancestors = new Map();
    for (const [rows,index] of [[changes.replacements,replacements],[changes.ancestors,ancestors]]) {
      for (const node of rows) {
        if (!node || typeof node.path !== 'string' || replacements.has(node.path) || ancestors.has(node.path)) invalid();
        index.set(node.path,node);
      }
    }
    // Copy only the changed branches. Unrelated directories stay in memory;
    // a malformed or stale response never mutates the displayed snapshot.
    const merge = node => {
      if (replacements.has(node.path)) {
        const replacement = replacements.get(node.path);
        replacements.delete(node.path);
        return replacement;
      }
      if (!ancestors.has(node.path)) return node;
      const ancestor = ancestors.get(node.path);
      ancestors.delete(node.path);
      if ('children' in ancestor) invalid();
      return {...ancestor,children:node.children.map(merge)};
    };
    const tree = merge(base.tree);
    if (replacements.size || ancestors.size) invalid();
    return validate({...base,...changes.metadata,tree});
  }
  return {validate,applyChanges};
})();
if (typeof module !== 'undefined' && module.exports) module.exports = SnapshotData;
