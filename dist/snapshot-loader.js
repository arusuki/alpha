'use strict';
const SnapshotLoader = (() => {
  const stages = [
    ['download','读取扫描结果'],['parse','解析 JSON 文档'],['validate','校验扫描数据'],['index','建立目录索引'],
    ['relations','解析挂载与共享引用'],['aggregate','汇总容器与用户用量'],['transfer','准备目录与统计视图'],['render','展示结果']
  ];
  function read(url,{signal,onProgress}) {
    return new Promise((resolve,reject) => {
      if (signal.aborted) { reject(new DOMException('已取消读取结果','AbortError')); return; }
      let worker, settled = false, data, model;
      const containerIndex = new Map();
      const finish = (error,result) => {
        if (settled) return;
        settled = true;
        if (worker) {
          worker.onmessage=worker.onerror=worker.onmessageerror=null;
          worker.terminate();
        }
        signal.removeEventListener('abort',abort);
        if (error) reject(error); else resolve(result);
      };
      const abort = () => finish(new DOMException('已取消读取结果','AbortError'));
      signal.addEventListener('abort',abort,{once:true});
      try { worker = new Worker('/snapshot-worker.js'); }
      catch (_) { finish(Error('无法启动后台结果解析，请使用支持 Web Worker 的浏览器并刷新重试。')); return; }
      worker.onerror = event => { event.preventDefault(); finish(Error('后台结果解析失败，请重试。')); };
      worker.onmessageerror = () => finish(Error('扫描结果传输失败，请重试。'));
      worker.onmessage = event => {
        if (settled) return;
        const message = event.data;
        try {
          if (message.type === 'progress') onProgress(message.value);
          else if (message.type === 'error') { const error=Error(message.message); error.status=message.status; finish(error); }
          else if (message.type === 'chunk') {
            const {section,value} = message;
            if (section === 'metadata') {
              data = {...value.metadata,tree:null,containers:[],resources:[],filesystems:[],warnings:[]};
              model = {...value.totals,nodes:new Map(),containers:new Map(),owners:new Map(),incomplete:new Set(),host:new Map()};
            } else if (section === 'nodes') {
              for (const item of value) {
                const node = {...item.value,children:[]};
                model.nodes.set(node.path,node);
                model.host.set(node.path,item.host);
                if (item.parent===null) data.tree=node;
                else model.nodes.get(item.parent).children[item.index]=node;
                if (item.incomplete) model.incomplete.add(node.path);
              }
            } else if (section === 'containerRows') {
              for (const [id,row] of value) model.containers.set(id,{...row,container:containerIndex.get(id)});
            } else if (section === 'ownerRows') {
              for (const [owner,row] of value) model.owners.set(owner,row);
            } else {
              for (const row of value) {
                data[section].push(row);
                if (section === 'containers') containerIndex.set(row.id,row);
              }
            }
            setTimeout(() => { if (!settled) worker.postMessage({type:'ack'}); },0);
          } else if (message.type === 'complete') finish(null,{data,usage:Usage.restore(model)});
        } catch (error) { finish(error); }
      };
      worker.postMessage({type:'start',url});
    });
  }
  return {read,stages};
})();
