// Control SSH interactions without generating keys or contacting share nodes.
'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const html=fs.readFileSync('dist/index.html','utf8'),elements=new Map(),requests=[],downloads=[];
function element(id){
  assert(html.includes(`id="${id}"`),`Missing DOM element ${id}`);
  if(!elements.has(id))elements.set(id,{value:'',disabled:false,hidden:false,open:false,textContent:'',innerHTML:'',dataset:{},listeners:{},children:[],
    addEventListener(type,fn){this.listeners[type]=fn;},querySelectorAll(){return [...elements.values()].filter(e=>e.id?.startsWith('bastion'));},
    reset(){},close(){this.open=false;},appendChild(child){this.children.push(child);}});
  return elements.get(id);
}
let cfg={revision:1,identity_file:'',identities:[],key_directory:'/srv/data/control-ssh',public_key:'',fingerprint:'',error:''};
const publicKey='ssh-ed25519 public-key-placeholder',identity={identity_file:'/srv/data/control-ssh/control-123/id_ed25519',public_key:publicKey,fingerprint:'SHA256:example'};
let saveError=false,pending=null,lastBlob;
const sandbox={console,Blob,setTimeout(fn){fn();},URL:{createObjectURL(blob){lastBlob=blob;return 'blob:public-key';},revokeObjectURL(){}},
  window:{},$:element,platform:{user:{role:'admin'}},esc:value=>String(value).replaceAll('&','&amp;').replaceAll('"','&quot;').replaceAll('<','&lt;'),
  document:{body:{appendChild(){}},createElement(tag){return {tag,click(){downloads.push({filename:this.download,blob:lastBlob});},remove(){}};}},
  api:async(path,options={})=>{
    requests.push({path,options});
    if(pending?.path===path)return new Promise(resolve=>{pending.resolve=resolve;});
    if(path==='/api/bastion/resources')return {tailscale:[],assignments:[],key_pool:{keys:[],error:''}};
    if(path==='/api/tailscale/settings')return {revision:1,tailnet:'-',has_api_token:false};
    if(path==='/api/bastion/ssh'&&options.method==='PUT'){
      if(saveError)throw Error('分享节点校验失败，SSH 配置未保存');
      const value=JSON.parse(options.body);assert.equal(value.revision,cfg.revision);
      cfg={...cfg,revision:cfg.revision+1,identity_file:value.identity_file,public_key:value.identity_file?publicKey:'',fingerprint:value.identity_file?'SHA256:example':''};
      return cfg;
    }
    if(path==='/api/bastion/ssh')return cfg;
    if(path==='/api/bastion/ssh/generate'){cfg={...cfg,identities:[...cfg.identities,identity.identity_file]};return identity;}
    if(path.startsWith('/api/bastion/ssh/public-key?'))return {...identity,identity_file:decodeURIComponent(path.split('identity_file=')[1])};
    throw Error('Unexpected request '+path);
  }};
vm.createContext(sandbox);vm.runInContext(fs.readFileSync('dist/bastion.js','utf8'),sandbox);
const flush=()=>new Promise(resolve=>setImmediate(resolve)),event={preventDefault(){}};
const act=async(id,type='click',input=event)=>{element(id).listeners[type](input);await flush();};
(async()=>{
  await sandbox.window.BastionUI.open();
  assert(element('bastionSSHCurrent').textContent.includes('尚未启用'));
  assert(element('bastionSSHCreate').open);
  assert(element('bastionSSHInventory').innerHTML.includes('还没有总控密钥'));
  assert(!html.includes('id="bastionSSHIdentity"'));
  assert(element('bastionSSHPublic').hidden);
  const initialRequests=requests.length;
  await act('bastionSSHForm','submit');assert.equal(requests.length,initialRequests);
  element('bastionSSHName').value='control';await act('bastionSSHGenerateForm','submit');
  assert(element('bastionSSHInventory').innerHTML.includes('aria-pressed="true"'));
  assert(element('bastionSSHStatus').textContent.includes('已持久保存'));
  assert(element('bastionSSHStatus').textContent.includes('尚未启用'));
  assert.equal(cfg.identity_file,'','generation must not activate a key');
  assert.deepEqual(JSON.parse(requests.at(-1).options.body),{name:'control'});
  assert(element('bastionSSHInventory').innerHTML.includes(identity.identity_file));
  sandbox.window.BastionUI.reset();await sandbox.window.BastionUI.open();
  assert(element('bastionSSHPublic').hidden,'reload should retain the active identity');
  assert(element('bastionSSHInventory').innerHTML.includes(identity.identity_file),'generated identities remain discoverable after reload');
  assert(element('bastionSSHInventory').innerHTML.includes('未启用'));
  await act('bastionSSHInventory','click',{target:{closest(selector){return selector==='[data-select-identity]'?{dataset:{selectIdentity:identity.identity_file}}:null;}}});
  assert(element('bastionSSHInventory').innerHTML.includes('aria-pressed="true"'));
  assert.equal(element('bastionSSHPublicKey').textContent,publicKey);
  assert.equal(cfg.identity_file,'','selecting a saved key must not activate it');
  await act('bastionSSHDownload');assert.equal(downloads[0].filename,'control-service.pub');
  assert.equal(await downloads[0].blob.text(),publicKey+'\n');
  saveError=true;await act('bastionSSHForm','submit');assert(element('bastionError').textContent.includes('校验失败'));
  assert.equal(cfg.identity_file,'');assert.equal(element('bastionSSHPublicKey').textContent,publicKey);
  saveError=false;await act('bastionSSHForm','submit');assert.equal(cfg.identity_file,identity.identity_file);
  assert(element('bastionSSHCurrent').textContent.includes('版本 2'));
  assert(element('bastionSSHInventory').innerHTML.includes('当前使用'));
  assert.equal(element('bastionSSHSave').dataset.locked,'true');
  // Duplicate generation and a reply arriving after logout must not expose a stale key.
  pending={path:'/api/bastion/ssh/generate'};await act('bastionSSHGenerateForm','submit');
  const before=requests.length;await act('bastionSSHGenerateForm','submit');assert.equal(requests.length,before);
  sandbox.window.BastionUI.reset();pending.resolve(identity);await flush();pending=null;
  assert.equal(element('bastionSSHPublicKey').textContent,'');assert(element('bastionSSHPublic').hidden);
  assert.equal(element('bastionSSHStatus').textContent,'');assert(element('bastionSSHFields').disabled);
  assert.equal(element('bastionSSHInventory').innerHTML,'');
  pending={path:'/api/bastion/ssh'};const loading=sandbox.window.BastionUI.open();await flush();
  sandbox.window.BastionUI.reset();pending.resolve(cfg);await loading;pending=null;
  assert.equal(element('bastionSSHCurrent').textContent,'');
  sandbox.platform.user={role:'viewer'};const count=requests.length;await sandbox.window.BastionUI.open();
  await act('bastionSSHGenerateForm','submit');await act('bastionSSHForm','submit');assert.equal(requests.length,count);
  console.log('Bastion SSH UI checks passed: selection, public download, generation without activation, failed/successful save, empty selection rejection, duplicate writes, viewer access and stale logout responses.');
})().catch(error=>{console.error(error);process.exitCode=1;});
