'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const elements=new Map();
function element(id){if(!elements.has(id))elements.set(id,{id,value:'',checked:false,hidden:false,disabled:false,innerHTML:'',textContent:'',listeners:{},addEventListener(type,fn){this.listeners[type]=fn;},reset(){},focus(){},showModal(){this.open=true;},close(){this.open=false;this.listeners.close?.();}});return elements.get(id);}
const calls=[];let failCreate=false,pending=null;
let permissions={username:'worker-user',uid:1000,endpoint:'unix:///var/run/docker.sock',base_dir:'/docker',group_member:false,group_active:false,group_error:'',restart_required:false,directory_writable:false,directory_error:'permission denied',docker_available:false,docker_error:'socket permission denied',sudo_error:''};
const managed={id:'a'.repeat(64),name:'alice',owner:'<unsafe>',origin:'adopt',state:'running',initialized:true,spec:{image:'train:test',port:2222,gpus:'all',network:'bridge'}};
const sandbox={console,$:element,window:{},document:{querySelectorAll(){return [...elements.values()];}},platform:{generation:1,user:{role:'admin'}},esc:s=>String(s).replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),api:async(path,options={})=>{
 calls.push({path,options:{...options}});if(pending)return new Promise(resolve=>pending.resolve=resolve);
 if(path==='/api/containers/permissions'){
  if(options.method==='POST'){
   const request=JSON.parse(options.body);
   if(request.action==='docker_group')permissions={...permissions,group_member:true,restart_required:true};
   else permissions={...permissions,directory_writable:true,directory_error:''};
  }
  return {...permissions};
 }
 if(path==='/api/containers/settings')return {endpoint:'unix:///var/run/docker.sock',image:'train:test',base_dir:'/docker',start_port:2222,ssh_host:'host.example',proxy_jump:''};
 if(path==='/api/containers'&&!options.method)return {managed:[managed],ssh_host:'host.example'};
 if(path==='/api/containers'&&options.method==='POST'){if(failCreate)throw Error('SSH 端口已被占用');return {name:'bob',port:2223,password:'new-secret'};}
 return {ok:true};
}};
vm.createContext(sandbox);vm.runInContext("let containerView='chart'; function renderContainers(){return 'storage';}",sandbox);vm.runInContext(fs.readFileSync('dist/containers.js','utf8'),sandbox);
const run=code=>vm.runInContext(code,sandbox),flush=()=>new Promise(resolve=>setImmediate(resolve));
async function submit(id){element(id).listeners.submit({preventDefault(){}});await flush();}
(async()=>{
 run('window.ContainersUI.open()');await flush();
 assert(element('managedContainerRows').innerHTML.includes('&lt;unsafe&gt;'));assert(element('managedContainerRows').innerHTML.includes('停止'));
 assert(![...elements.keys()].some(id=>id.startsWith('adopt')),'no web adoption controls');
 // Browser coverage exercises dialog rendering and failure/retry; check the payload here.
 element('containerGroupRepair').listeners.click();
 element('containerSudoPassword').value='sudo-secret';await submit('containerPermissionForm');
 const repair=calls.find(c=>c.path==='/api/containers/permissions'&&c.options.method==='POST');assert.deepEqual(JSON.parse(repair.options.body),{action:'docker_group',base_dir:'/docker',endpoint:'unix:///var/run/docker.sock',sudo_password:'sudo-secret'});
 element('newContainerName').value='bob';element('newContainerNetwork').value='bridge';element('newContainerGPUs').value='all';element('newContainerPassword').value='supplied-secret';
 await submit('createContainerForm');assert.equal(element('newContainerPassword').value,'');assert(element('containerCredentials').textContent.includes('new-secret'));assert(element('containerCredentialsDialog').open);
 element('containerCredentialsClose').listeners.click();assert.equal(element('containerCredentials').textContent,'');
 failCreate=true;await submit('createContainerForm');assert(element('containersError').textContent.includes('SSH 端口已被占用'));failCreate=false;
 const button={dataset:{id:managed.id,containerAction:'delete'}};element('managedContainerRows').listeners.click({target:{closest(){return button;}}});assert(!element('containerConfirmLabel').hidden);assert(element('containerActionHint').textContent.includes('可写层'));
 element('containerConfirmName').value='alice';await submit('containerActionForm');const removal=calls.find(c=>c.path.endsWith('/delete'));assert.equal(JSON.parse(removal.options.body).confirm,'alice');
 pending={};element('createContainerForm').listeners.submit({preventDefault(){}});const before=calls.length;element('createContainerForm').listeners.submit({preventDefault(){}});assert.equal(calls.length,before,'no duplicate mutation');
 sandbox.platform.generation++;run('window.ContainersUI.reset()');pending.resolve({name:'stale',password:'stale-secret',port:2222});await flush();pending=null;assert.equal(run('containerView'),'chart');assert.equal(run('renderContainers()'),'storage');assert.equal(element('containerCredentials').textContent,'');
 permissions={...permissions,directory_writable:false};run('window.ContainersUI.open()');await flush();element('containerDirectoryRepair').listeners.click();element('containerSudoPassword').value='pending-secret';pending={};element('containerPermissionForm').listeners.submit({preventDefault(){}});
 const beforeRepair=calls.length;element('containerPermissionForm').listeners.submit({preventDefault(){}});assert.equal(calls.length,beforeRepair,'no duplicate permission repair');
 sandbox.platform.generation++;run('window.ContainersUI.reset()');pending.resolve({...permissions});await flush();pending=null;assert.equal(element('containerSudoPassword').value,'');assert(!element('containerPermissionDialog').open);assert.equal(element('containerDirectoryStatus').textContent,'等待检查');
 sandbox.platform.user.role='viewer';const beforeViewer=calls.length;run('window.ContainersUI.open()');await flush();assert(!element('managedContainerRows').innerHTML.includes('data-container-action'));assert(!calls.slice(beforeViewer).some(c=>c.path==='/api/containers/permissions'));
 console.log('Container UI checks passed: permission payloads, duplicate writes, stale logout responses, viewer access and container actions.');
})().catch(e=>{console.error(e);process.exitCode=1;});
