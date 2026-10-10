'use strict';
const assert=require('assert'),fs=require('fs'),vm=require('vm');
const elements=new Map();
function element(id){
  if(!elements.has(id))elements.set(id,{value:'',dataset:{},parentElement:{dataset:{}},innerHTML:'',textContent:'',addEventListener(){},setAttribute(){},contains(){return false;},querySelectorAll(){return [];}});
  return elements.get(id);
}
const sandbox={console,AbortSignal,document:{activeElement:null,querySelectorAll(){return [];}},window:{},$:element,
  platform:{user:{role:'viewer'}},dateTime:n=>String(n),esc:s=>String(s??'').replace(/[&<>"']/g,c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])),
  GPUUI:{view:()=>({devices:[],unavailable:false,note:''})}};
vm.createContext(sandbox);
vm.runInContext(fs.readFileSync('dist/cluster.js','utf8'),sandbox);
let data;
sandbox.api=async()=>data;
const worker={id:'a'.repeat(32),kind:'worker',name:'worker',url:'http://worker',online:true,inventory:{containers:[],services:[
  {name:'tetragon',state:'running',detail:'Up 1 hour',checked_at:123},
  {name:'dram-bw',state:'exited',detail:'Exited (1) <script>bad()</script>',checked_at:123}, {name:'rootless-docker',state:'running',checked_at:123}]}};
function overview(node){return {nodes:[node],online:node.online?1:0,container_count:0,members:[],checked_at:123};}
(async()=>{
  data=overview(worker);await sandbox.window.ClusterUI.refresh();
  let html=element('clusterNodes').innerHTML;
  assert(html.includes('Tetragon')&&html.includes('DRAM 带宽')&&html.includes('Rootless Docker'));
  assert(html.includes('data-state="running"')&&html.includes('已停止'));
  assert(html.includes('&lt;script&gt;')&&!html.includes('<script>'));
  assert(html.includes('检查于 123'));
  sandbox.api=async()=>{throw Error('network down');};
  await sandbox.window.ClusterUI.refresh();
  html=element('clusterNodes').innerHTML;
  assert(html.includes('刷新失败，状态待确认'));
  assert(!html.includes('data-state="running"'),'stale service must not remain green');
  sandbox.api=async()=>data;
  data=overview({...worker,online:false,inventory:null});await sandbox.window.ClusterUI.refresh();
  html=element('clusterNodes').innerHTML;
  assert(html.includes('节点离线，状态未知'));
  assert(!html.includes('data-state="running"'));
  data=overview({...worker,inventory:{containers:[],services:[{name:'tetragon',state:'missing'},{name:'dram-bw',state:'unknown',detail:'permission denied'}]}});
  await sandbox.window.ClusterUI.refresh();
  html=element('clusterNodes').innerHTML;
  assert(html.includes('未部署')&&html.includes('状态未知')&&html.includes('permission denied'));
  data=overview(worker);await sandbox.window.ClusterUI.refresh();
  assert(element('clusterNodes').innerHTML.includes('data-state="running"'),'successful refresh restores live service state');
  console.log('service status UI tests passed');
})().catch(error=>{console.error(error);process.exitCode=1;});
