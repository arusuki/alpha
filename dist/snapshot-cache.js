'use strict';
// Store snapshot JSON as a Blob. Metadata lives separately so listing
// cache sizes never deserializes large result files. Scope is the signed-in user.
const SnapshotCache = (() => {
  const database = 'project-alpha-snapshot-cache';
  function open() {
    return new Promise((resolve,reject) => {
      if (typeof indexedDB === 'undefined') { reject(Error('浏览器不支持本地缓存')); return; }
      const request = indexedDB.open(database,1);
      let blocked = false;
      request.onupgradeneeded = () => {
        const db = request.result;
        db.createObjectStore('records',{keyPath:['scope','url']}).createIndex('scope','scope');
        db.createObjectStore('files');
        db.createObjectStore('epochs');
      };
      request.onerror = () => reject(request.error);
      request.onblocked = () => { blocked=true; reject(Error('本地缓存被其他页面占用，请关闭其他页面后重试')); };
      request.onsuccess = () => {
        if (blocked) { request.result.close(); return; }
        request.result.onversionchange = () => request.result.close();
        resolve(request.result);
      };
    });
  }
  async function transaction(mode,work) {
    const db = await open();
    try {
      return await new Promise((resolve,reject) => {
        const tx = db.transaction(['records','files','epochs'],mode);
        let result;
        tx.oncomplete = () => resolve(result);
        tx.onabort = () => reject(tx.error || Error('本地缓存操作失败'));
        try { work(tx,value => { result=value; }); }
        catch (error) { tx.abort(); reject(error); }
      });
    } finally { db.close(); }
  }
  function read(scope,url) {
    return transaction('readonly',(tx,done) => {
      const key = [scope,url], state = {epoch:0,record:null,blob:null};
      tx.objectStore('epochs').get(scope).onsuccess = e => { state.epoch=e.target.result || 0; };
      tx.objectStore('records').get(key).onsuccess = e => { state.record=e.target.result; };
      tx.objectStore('files').get(key).onsuccess = e => { state.blob=e.target.result; };
      done(state);
    });
  }
  function save(scope,url,epoch,blob,data) {
    return transaction('readwrite',(tx,done) => {
      tx.objectStore('epochs').get(scope).onsuccess = e => {
        // A manual deletion also cancels writes from downloads already in flight,
        // including downloads in other tabs or Workers.
        if ((e.target.result || 0)!==epoch) { done(false); return; }
        tx.objectStore('records').put({scope,url,size:blob.size,jobId:data.job_id,host:data.host,finishedAt:data.finished_at,savedAt:Date.now()});
        tx.objectStore('files').put(blob,[scope,url]);
        done(true);
      };
    });
  }
  function list(scope) {
    return transaction('readonly',(tx,done) => {
      tx.objectStore('records').index('scope').getAll(scope).onsuccess = e => done(e.target.result);
    });
  }
  function remove(scope,url) {
    return transaction('readwrite',tx => {
      const epochs = tx.objectStore('epochs');
      epochs.get(scope).onsuccess = e => epochs.put((e.target.result || 0)+1,scope);
      const records = tx.objectStore('records'), files = tx.objectStore('files');
      if (url) { records.delete([scope,url]); files.delete([scope,url]); return; }
      records.index('scope').openCursor(scope).onsuccess = e => {
        const cursor = e.target.result;
        if (!cursor) return;
        files.delete(cursor.primaryKey); cursor.delete(); cursor.continue();
      };
    });
  }
  return {read,save,list,remove};
})();
