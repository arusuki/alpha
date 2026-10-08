'use strict';
// The server owns parsing, reference resolution and accounting. The browser
// connects a small set of display nodes and never reconstructs the full scan.
const SnapshotLoader = (() => {
  const stages = [['download','读取展示数据'],['render','展示结果']];
  function restore(view) {
    if (view.view_version !== 1 || !view.metadata || !Number.isInteger(view.metadata.revision) || !Array.isArray(view.nodes) || !view.usage) throw Error('扫描展示数据格式无效，请刷新页面重试。');
    const data = {...view.metadata}, model = {...view.usage,nodes:new Map(),host:new Map(),incomplete:new Set(),containers:new Map(),owners:new Map(),remote:true};
    for (const row of view.nodes) {
      if (typeof row.path !== 'string' || !Array.isArray(row.children) || !row.host || model.nodes.has(row.path)) throw Error('扫描目录展示数据无效。');
      const {host,partial,...node} = row;
      model.nodes.set(row.path,{...node,children:[]});
      model.host.set(row.path,host);
      if (partial) model.incomplete.add(row.path);
    }
    for (const row of view.nodes) model.nodes.get(row.path).children = row.children.map(path => {
      const node=model.nodes.get(path);
      if (!node) throw Error('扫描目录展示数据缺少子项。');
      return node;
    });
    data.tree = model.nodes.get(view.root);
    if (!data.tree) throw Error('扫描目录展示数据缺少根节点。');
    for (const [owner,row] of Object.entries(view.usage.owners)) model.owners.set(owner,{...row,containers:[]});
    for (const container of data.containers) {
      const row=view.usage.containers[container.id], owner=Usage.ownerOf(container);
      if (!row || !model.owners.has(owner)) throw Error('扫描容器统计数据无效。');
      const layer=container.writable_layer;
      model.containers.set(container.id,{...row,container,owner,writable:{...layer,known:layer.allocated != null}});
      model.owners.get(owner).containers.push(container.id);
    }
    return {data,usage:Usage.restore(model)};
  }
  async function read(url,{signal,onProgress=()=>{}}={}) {
    onProgress({stage:'download',done:0,total:null,detail:'服务器正在准备概览与当前目录'});
    const response=await fetch(url,{credentials:'same-origin',cache:'no-store',signal});
    const view=await response.json();
    if (!response.ok) { const error=Error(view.error || `展示数据读取失败（${response.status}）`);error.status=response.status;throw error; }
    if (signal?.aborted) throw new DOMException('已取消读取结果','AbortError');
    const result=restore(view);
    onProgress({stage:'download',done:1,total:1,detail:'展示数据已就绪'});
    return result;
  }
  return {read,restore,stages};
})();
