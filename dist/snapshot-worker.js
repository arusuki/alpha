'use strict';
importScripts('/snapshot.js','/usage.js','/snapshot-cache.js');

let acknowledge, started = false, lastReport = 0, lastStage = '', lastDetail = '', nodeCount = null;
function report(value) {
  if (value.stage === 'validate' && value.total != null) nodeCount = value.total;
  if (value.stage === 'index' && value.total == null) value.total = nodeCount;
  const now = performance.now();
  if (value.stage !== lastStage || value.detail !== lastDetail || now-lastReport >= 60 || value.total != null && value.done === value.total) {
    postMessage({type:'progress',value});
    lastReport = now; lastStage = value.stage; lastDetail = value.detail;
  }
}
function sendChunk(section, value) {
  // One outstanding batch bounds main-thread deserialization and lets input
  // events (including Cancel) run between batches of directory nodes.
  return new Promise(resolve => { acknowledge = resolve; postMessage({type:'chunk',section,value}); });
}
async function readSnapshot(url,scope,changesURL,retry=false) {
  let cached = null, cacheError = '';
  if (scope) {
    try { cached = await SnapshotCache.read(scope,url); }
    catch (_) { cacheError = '本地缓存不可用，本次结果未缓存。'; }
  }
  const hit = !retry && changesURL && cached?.record && cached.blob instanceof Blob && cached.blob.size === cached.record.size;
  report({stage:'download',done:0,total:null,unit:'bytes',detail:hit?'读取本地缓存':'等待服务器读取扫描结果'});
  const response = hit ? null : await fetch(url,{credentials:'same-origin',cache:'no-store'});
  if (response && !response.ok) {
    const body = await response.json().catch(() => ({}));
    const error = Error(body.error || `扫描结果读取失败（${response.status}）`);
    error.status = response.status; throw error;
  }
  const length = Number(response?.headers.get('Content-Length'));
  const total = hit ? cached.blob.size : length > 0 && !response.headers.get('Content-Encoding') ? length : null;
  let received = 0;
  const reader = (hit ? cached.blob.stream() : response.body).getReader(), decoder = new TextDecoder(), chunks = [], bytes = [];
  for (;;) {
    const {value,done} = await reader.read();
    if (done) break;
    received += value.byteLength;
    chunks.push(decoder.decode(value,{stream:true}));
    if (!hit && cached) bytes.push(value);
    report({stage:'download',done:received,total,unit:'bytes',detail:hit?'读取本地缓存':'下载扫描结果'});
  }
  chunks.push(decoder.decode());
  let text = chunks.join('');
  report({stage:'download',done:received,total:received,unit:'bytes',detail:hit?'本地缓存读取完成，无需下载':'扫描结果下载完成'});
  // Native JSON.parse has no incremental counter. Keep this stage explicitly
  // indeterminate; it runs off the UI thread and can be terminated immediately.
  report({stage:'parse',done:0,total:null,detail:'解析 JSON 文档',bytes:received});
  let data;
  try {
    try { data = JSON.parse(text); }
    catch (_) { throw Error('扫描结果不是有效的 JSON，无法解析。'); }
    text = null;
    report({stage:'parse',done:1,total:1,unit:'份文档',detail:'JSON 文档解析完成',bytes:received});
    SnapshotData.validate(data,report);
  } catch (error) {
    if (!hit) throw error;
    // A damaged local file must never prevent a fresh authenticated download.
    try { await SnapshotCache.remove(scope,url); } catch (_) {}
    return readSnapshot(url,scope,changesURL,true);
  }
  let updated = false;
  if (hit) {
    // Directory exploration changes the server revision after the original
    // download. Catch up from that cached revision instead of downloading the
    // entire snapshot again. This request also revalidates access and ownership.
    report({stage:'validate',done:0,total:null,detail:'正在校验缓存版本并读取目录更新'});
    const response = await fetch(`${changesURL}?revision=${data.revision}`,{credentials:'same-origin',cache:'no-store'});
    const changes = await response.json();
    if (!response.ok) {
      const error = Error(changes.error || `目录更新读取失败（${response.status}）`);
      error.status = response.status; throw error;
    }
    const {tree,...metadata} = data;
    updated = changes.revision !== data.revision || JSON.stringify(changes.metadata) !== JSON.stringify(metadata);
    data = SnapshotData.applyChanges(data,changes);
    report({stage:'validate',done:1,total:1,detail:updated?'本地缓存已合并最新目录与归属':'本地缓存已是最新版本'});
  }
  if (cached && (!hit || updated)) {
    try { await SnapshotCache.save(scope,url,cached.epoch,new Blob(hit?[JSON.stringify(data)]:bytes,{type:'application/json'}),data); }
    catch (_) { cacheError = '本地缓存写入失败（可能空间不足），本次结果仍可正常查看。'; }
  }
  return {data,cacheError};
}
async function run(url,scope,changesURL) {
  const {data,cacheError} = await readSnapshot(url,scope,changesURL);
  const model = Usage.build(data,report);
  const {tree,containers,resources,filesystems,warnings,...metadata} = data;
  const {exclusive,shared,crossOwner,unrelated,attributionLimited} = model;
  const sections = {containers,resources,filesystems,warnings};
  const total = model.nodes.size + containers.length + resources.length + filesystems.length + warnings.length + model.containers.size + model.owners.size;
  let done = 0;
  const transferProgress = current => report({stage:'transfer',done,total,unit:'条记录',detail:'准备目录明细与统计视图',current});
  transferProgress();
  await sendChunk('metadata',{metadata,totals:{exclusive,shared,crossOwner,unrelated,attributionLimited}});
  for (const [section,rows] of Object.entries(sections)) {
    for (let i=0;i<rows.length;i+=500) {
      const batch = rows.slice(i,i+500);
      await sendChunk(section,batch); done += batch.length; transferProgress();
    }
  }
  const stack = [{node:tree,parent:null,index:0}];
  while (stack.length) {
    const batch = [];
    while (stack.length && batch.length<500) {
      const {node,parent,index} = stack.pop(), {children,...value} = node;
      batch.push({value,parent,index,incomplete:model.incomplete.has(node.path),host:model.host.get(node.path)});
      for (let i=children.length-1;i>=0;i--) stack.push({node:children[i],parent:node.path,index:i});
    }
    await sendChunk('nodes',batch); done += batch.length; transferProgress(batch[batch.length-1].value.path);
  }
  for (const [section,rows] of [['containerRows',model.containers],['ownerRows',model.owners]]) {
    let batch = [];
    for (const [key,row] of rows) {
      const {container,...value} = row;
      batch.push([key,value]);
      if (batch.length===500) { await sendChunk(section,batch); done += batch.length; transferProgress(); batch=[]; }
    }
    if (batch.length) { await sendChunk(section,batch); done += batch.length; transferProgress(); }
  }
  postMessage({type:'complete',cacheError});
}
self.onmessage = event => {
  if (event.data.type === 'ack') {
    const resolve = acknowledge; acknowledge = null;
    if (resolve) resolve();
  } else if (event.data.type === 'start' && !started) {
    started = true;
    run(event.data.url,event.data.scope,event.data.changesURL).catch(error => postMessage({type:'error',message:error.message,status:error.status}));
  }
};
