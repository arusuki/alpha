import copy,json,threading,time,mimetypes
from pathlib import Path
from urllib.parse import urlparse,parse_qs
from browser_support import NodeHandler, NODE_PATH, launch_options, start_server, snapshot_view
from playwright.sync_api import sync_playwright
root=Path(__file__).resolve().parents[1]/'dist';g=1024**3
sample=json.loads((root.parent/'tests/fixtures/snapshot.json').read_text());id='a'*32
sample.update(job_id=id,revision=0,scan={'omitted_references':0})
def node(path,size,children=None,**extra):return dict(name=path.split('/')[-1],path=path,kind='directory',allocated=size,apparent=size,files=1,errors=0,children=children or [],**extra)
sample['tree']=node('@root',60*g,[node('/data',30*g,omitted_entries=100),node('/host-data',30*g,omitted_entries=100)])
sample['tree']['kind']='root';sample['tree']['name']='已扫描存储'
c=sample['containers'][0];c.update(upper_path='/data',mounts=[],log_path=None);c['writable_layer']=dict(allocated=30*g,apparent=30*g,status='complete',permission_denied=False);sample['containers']=[c];sample['resources']=[dict(path='/data',kinds=['writable'],containers=[c['id']])]
sample['filesystems']=[dict(device='1',mount='/',fs='ext4',total=100*g,used=70*g,available=30*g,scanned=60*g,unexplained=10*g)]
base=dict(id=id,status='completed',trigger='manual',created_at=1,finished_at=2,created_by='admin',allocated=60*g,config={},snapshot_revision=0)
# Keep the displayed scan summary when a directory task finishes later.
latest=dict(id='b'*32,status='completed',trigger='incremental',created_at=3,finished_at=4,created_by='admin',allocated=1,config={'base_job_id':id,'base_revision':0,'incremental_path':'/data'})
state=dict(jobs=[base],directory_jobs=[latest],active=None,latest_id=id,interval_minutes=0)
updates=[];requests=[];lock=threading.RLock()
def patch(previous,current,path):
 meta=copy.deepcopy(current);tree=meta.pop('tree');ancestor={k:v for k,v in tree.items() if k!='children'}
 return dict(job_id=id,base_revision=previous,revision=current['revision'],metadata=meta,replacements=[next(n for n in tree['children'] if n['path']==path)],ancestors=[ancestor])
def work(path,job):
 for stage in [1,2]:
  time.sleep(.8)
  with lock:
   previous=sample['revision'];parent=next(n for n in sample['tree']['children'] if n['path']==path)
   parent['children']=[node(path+'/MNIST-v2',20*g,omitted_entries=40)]
   parent['omitted_entries']=0
   if stage==1:parent.update(scanning=True)
   else:
    parent.update(scanning=False)
    parent['children'] += [node(path+'/images',5*g,omitted_entries=10),node(path+'/models',5*g,omitted_entries=8)]
    job['status']='completed';state['active']=None
   sample['revision']+=1;sample['updated_at']='2026-09-13T12:00:00Z'
   job['progress']=dict(entries=stage*100,path=path,allocated=stage*10*g,phase='directory')
   base['snapshot_revision']=sample['revision']
   updates.append(patch(previous,sample,path))
class Handler(NodeHandler):
 def do_GET(self):
  if self.control_request():return
  parsed=urlparse(self.path);path=parsed.path;requests.append(path)
  if path.endswith('/events'):
   cursor=int(self.headers.get('Last-Event-ID') or parse_qs(parsed.query).get('revision',['0'])[0]);self.send_response(200);self.send_header('Content-Type','text/event-stream');self.send_header('Cache-Control','no-store');self.end_headers()
   try:
    for _ in range(120):
     with lock:batch=copy.deepcopy([u for u in updates if u['revision']>cursor])
     for u in batch:self.wfile.write(('id: '+str(u['revision'])+'\nevent: changes\ndata: '+json.dumps(dict(job_id=id,revision=u['revision']))+'\n\n').encode());cursor=u['revision']
     self.wfile.write(b': heartbeat\n\n');self.wfile.flush();time.sleep(.05)
   except (BrokenPipeError,ConnectionResetError):pass
   return
  with lock:
   if path=='/api/session':value=dict(user=dict(id='admin',username='admin',role='admin'),csrf='test')
   elif path=='/api/state':value=state
   elif path.endswith('/view'):value=snapshot_view(sample,parse_qs(parsed.query).get('path',[''])[0])
   elif path.endswith('/changes'):
    revision=int(parse_qs(parsed.query)['revision'][0]);changes=[u for u in updates if u['revision']>revision]
    if changes:value=patch(revision,sample,changes[-1]['replacements'][0]['path'])
    else:
     value=dict(job_id=id,base_revision=revision,revision=revision,metadata={k:v for k,v in sample.items() if k!='tree'},replacements=[],ancestors=[])
   else:value=None
   if value is not None:body=json.dumps(value).encode();typ='application/json'
  if value is None:
   file=root/('index.html' if path=='/' else path.lstrip('/'))
   if not file.is_file():self.send_error(404);return
   body=file.read_bytes();typ=mimetypes.guess_type(str(file))[0] or 'text/plain'
  self.send_response(200);self.send_header('Content-Type',typ);self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
 def do_POST(self):
  data=json.loads(self.rfile.read(int(self.headers.get('Content-Length','0'))))
  if not self.path.endswith('/expand'):self.send_error(404);return
  with lock:
   job=dict(id=latest['id'],status='running',trigger='incremental',created_at=time.time(),created_by='admin',config=dict(base_job_id=id,base_revision=data['revision'],incremental_path=data['path']),progress=dict(entries=0,phase='directory'))
   state.update(active=job,directory_jobs=[job]);body=json.dumps(job).encode()
  self.send_response(202);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(body)));self.end_headers();self.wfile.write(body)
  threading.Thread(target=work,args=(data['path'],job),daemon=True).start()
server=start_server(Handler)
with sync_playwright() as p:
 browser=p.chromium.launch(**launch_options())
 page=browser.new_page(viewport=dict(width=1440,height=1000));errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
 page.goto('http://127.0.0.1:'+str(server.server_port)+NODE_PATH);page.locator('.platform-nav [data-page=overview]').click();page.wait_for_function('platform.loaded!==null')
 assert page.locator('#taskStatus').inner_text()=='累计扫描结果'
 assert page.locator('#taskScanned').inner_text()=='60 GiB'
 assert '30 GiB' in page.locator('#taskCapacityNote').inner_text()
 assert page.locator('#jobsBody tr').count()==1
 page.locator('#containerChart [data-container]').first.click();page.wait_for_function('explorer && explorer.entries.some(e=>e.pending)')
 gray=page.locator('.map-tile').filter(has_text='子目录待分析的历史占用')
 gray.click();page.wait_for_function('snapshot.revision===1')
 assert page.locator('.map-tile').filter(has_text='MNIST-v2').count()==1
 assert '10 GiB' in gray.inner_text()
 assert page.evaluate('explorer.trail[explorer.trail.length-1].path')=='/data'
 page.locator('#mapSearch').fill('MNIST');page.wait_for_function('snapshot.revision===2')
 assert page.locator('#mapSearch').input_value()=='MNIST'
 assert page.locator('#mapSearch').evaluate('(e)=>e===document.activeElement')
 page.locator('#mapSearch').fill('')
 assert page.locator('.map-tile').count()==3
 page.locator('.map-tile').filter(has_text='MNIST-v2').click()
 assert page.evaluate('explorer.trail[explorer.trail.length-1].path')=='/data/MNIST-v2'
 assert '20 GiB' in page.locator('.map-tile').inner_text()
 page.locator('[data-storage-up]').click();page.keyboard.press('Escape')
 page.locator('.host-explorer-entry').click();page.locator('.map-tile').filter(has_text='/host-data').click()
 page.locator('.map-tile').filter(has_text='子目录待分析的历史占用').click();page.wait_for_function('snapshot.revision===3')
 assert page.evaluate('selected===HOST')
 assert 'MNIST-v2' in page.locator('#directoryMap').inner_text()
 page.wait_for_function('snapshot.revision===4')
 page.set_viewport_size(dict(width=390,height=844))
 assert page.locator('#containerDialog').evaluate('(e)=>e.scrollWidth<=e.clientWidth')
 # State tiles and the map stay within the mobile dialog.
 assert not any(r.endswith('/snapshot') or r.endswith('/changes') for r in requests),requests
 assert not errors,errors
 assert len([r for r in requests if r.endswith('/events')])>=1
 print('Chromium live map passed: cumulative history, direct gray click, SSE split 30 → 20+10 → 20+5+5, search focus preserved, container/Host drilldown, mobile layout, display views only, no page errors.')
 browser.close()
server.shutdown()
