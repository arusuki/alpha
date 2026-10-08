"""GPU observatory: navigation, rolling replacement, zoom, stale/empty states and layout."""
import math
import time
from pathlib import Path
from urllib.parse import urlsplit, parse_qs
from browser_support import NodeHandler, NODE_PATH, PROXY_PATH, launch_options, start_server
from playwright.sync_api import sync_playwright

calls=[]
mode='normal'


def overview(query):
    now=time.time()
    hours=float(query.get('hours',['6'])[0])
    end=min(float(query.get('end',[now])[0]),now)
    start=max(now-72*3600,end-hours*3600)
    step=max(float(query.get('step',['60'])[0]),math.ceil(hours*3600/864/15)*15)
    devices=[]
    series=[]
    for i,name in enumerate(['NVIDIA RTX 4090','NVIDIA RTX 4090','NVIDIA A100 80GB','NVIDIA A100 80GB']):
        process=[] if i==3 else [dict(pid=18920+i,name='python train.py' if i!=2 else '<img src=x onerror=alert(1)>',kind='C',memory_mib=12480,container_id='a'*64,container='research-alice' if i==0 else 'finetune-bob',owner='alice' if i==0 else 'bob',started_at=now-7260)]
        if i==1:process.append(dict(pid=21900,name='python evaluate.py',kind='C',memory_mib=4320,container_id='b'*64,container='research-alice',owner='alice',started_at=now-425))
        devices.append(dict(uuid=f'GPU-test-{i}',index=i,name=name,utilization=[86,63,98,0][i],memory_used_mib=[18340,14840,61250,128][i],memory_total_mib=24576 if i<2 else 81920,temperature=[62,57,68,34][i],compute_mode='Default',mig=False,processes=process,process_count=len(process),owners=sorted(set(p['owner'] for p in process)),state='unknown' if mode in ('stale','expired') else 'busy' if process else 'idle'))
        points=[]
        for n in range(min(864,math.ceil((end-start)/step))):
            if 70<n<80:continue
            users={} if i==3 else {('alice' if n%100<55 else 'bob'):step}
            if 110<n<155 and i==1:users={'alice':step,'bob':step}
            points.append(dict(at=start+n*step,utilization=0 if i==3 else round(max(0,min(100,60+22*math.sin(n/9+i)+12*math.sin(n/3)))),observed_seconds=step,owners=users))
        series.append(dict(uuid=f'GPU-test-{i}',name=name,points=points))
    if mode=='empty':devices=[];series=[]
    return dict(now=now,sample_seconds=15,current=dict(at=now-100 if mode in ('stale','expired') else now,stale=mode in ('stale','expired'),devices=devices,error='nvidia-smi 暂时不可用' if mode=='stale' else '',warning=''),history=dict(from_=start,to=end,step=step,series=series,users=[] if mode=='empty' else [dict(owner='alice',seconds=41.25*3600),dict(owner='bob',seconds=29.78*3600),dict(owner='host:root',seconds=2.41*3600)],since=now-58*3600))

class Handler(NodeHandler):
    def do_GET(self):
        if self.control_request():return
        path=urlsplit(self.path).path
        if path=='/api/session':return self.respond(dict(user=dict(id='admin',username='admin',role='admin'),csrf='test',setup_required=False))
        if path=='/api/state':return self.respond(dict(jobs=[],directory_jobs=[],latest_id=None,active=None,interval_minutes=30))
        if path=='/api/gpu/overview':
            request_path=self.requestline.split()[1]
            calls.append(request_path)
            if urlsplit(request_path).path!=PROXY_PATH+'/api/gpu/overview':
                return self.respond(dict(error='接口不存在，请先选择节点'),404)
            if mode=='error':return self.respond(dict(error='节点离线'),503)
            data=overview(parse_qs(urlsplit(self.path).query));data['history']['from']=data['history'].pop('from_')
            return self.respond(data)
        self.serve_asset()

server=start_server(Handler)
with sync_playwright() as p:
    browser=p.chromium.launch(**launch_options())
    page=browser.new_page(viewport=dict(width=1440,height=1100),device_scale_factor=1)
    errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
    page.goto(f'http://127.0.0.1:{server.server_port}{NODE_PATH}#gpus')
    page.wait_for_selector('.gpu-card')
    assert page.locator('.gpu-card').count()==4
    assert page.locator('#gpuCount').inner_text()=='4'
    assert page.locator('#gpuBusy').inner_text()=='3'
    assert page.locator('.gpu-chart').count()==4
    assert page.locator('#gpuProcessRows tr').count()==1
    page.locator('[data-gpu-select="GPU-test-1"]').click()
    assert page.locator('#gpuProcessRows tr').count()==2
    page.locator('[data-gpu-select="GPU-test-2"]').click()
    assert '<img src=x onerror=alert(1)>' in page.locator('#gpuProcessRows').inner_text()
    assert page.locator('#gpuProcessRows img').count()==0
    page.locator('[data-gpu-select="GPU-test-0"]').click()
    screenshot=Path('/tmp/project-alpha-gpu-desktop.png')
    page.screenshot(path=str(screenshot),full_page=True)
    for _ in range(3):
        with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
        page.wait_for_function("!document.querySelector('#gpuRefresh').disabled")
    assert page.locator('.gpu-card').count()==4 and page.locator('.gpu-chart').count()==4
    with page.expect_response(lambda r:'hours=72' in r.url):page.locator('[data-gpu-hours="72"]').click()
    assert page.locator('#gpuPan').is_disabled()
    with page.expect_response(lambda r:'hours=36' in r.url):page.locator('#gpuZoomIn').click()
    assert not page.locator('#gpuPan').is_disabled()
    with page.expect_response(lambda r:'end=' in r.url):page.locator('#gpuPan').fill('18')
    page.wait_for_function("document.querySelector('#gpuLive').getAttribute('aria-pressed')==='false'")
    with page.expect_response(lambda r:'end=' not in r.url and '/api/gpu/overview?' in r.url):page.locator('#gpuLive').click()
    with page.expect_response(lambda r:'step=300' in r.url):page.locator('#gpuStep').select_option('300')
    page.set_viewport_size(dict(width=390,height=844))
    page.wait_for_timeout(250)
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile overflows'
    page.screenshot(path='/tmp/project-alpha-gpu-mobile.png',full_page=True)
    mode='stale'
    with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
    page.wait_for_function("document.querySelector('#gpuStatus').dataset.error==='true'")
    assert page.locator('.gpu-state').first.inner_text()=='状态未知'
    assert page.locator('.gpu-card[data-state=unknown]').count()==4
    assert page.locator('#gpuBusy').inner_text()=='—' and page.locator('#gpuUsers').inner_text()=='—'
    assert page.locator('.gpu-state').first.evaluate('(el) => getComputedStyle(el).color')=='rgb(119, 126, 122)'
    mode='expired'
    with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
    page.wait_for_function("document.querySelector('#gpuStatus').textContent.includes('GPU 数据已过期')")
    assert page.locator('.gpu-card[data-state=unknown]').count()==4
    mode='error'
    with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
    page.wait_for_function("document.querySelector('#gpuStatus').textContent.includes('节点离线')")
    assert page.locator('.gpu-card[data-state=unknown]').count()==4
    mode='normal'
    with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
    page.wait_for_function("document.querySelector('#gpuBusy').textContent==='3'")
    assert page.locator('.gpu-card[data-state=busy]').count()==3
    assert page.locator('.gpu-card[data-state=idle]').count()==1
    assert page.locator('.gpu-card[data-state=busy] .gpu-state').first.evaluate('(el) => getComputedStyle(el).color')=='rgb(173, 85, 29)'
    assert page.locator('.gpu-card[data-state=idle] .gpu-state').first.evaluate('(el) => getComputedStyle(el).color')=='rgb(40, 115, 67)'
    mode='empty'
    with page.expect_response(lambda r:'/api/gpu/overview?' in r.url):page.locator('#gpuRefresh').click()
    page.wait_for_selector('#gpuEmpty',state='visible')
    assert page.locator('.gpu-card').count()==0
    page.get_by_role('button',name='总面板',exact=True).click()
    page.wait_for_timeout(100)
    n=len(calls)
    # Manual refresh cannot leak a request after leaving the module.
    page.evaluate("document.querySelector('#gpuRefresh').click()")
    page.wait_for_timeout(100)
    assert len(calls)==n
    assert calls and all(urlsplit(path).path==PROXY_PATH+'/api/gpu/overview' for path in calls)
    assert not errors,errors
    browser.close()
server.shutdown()
print('GPU browser checks passed; screenshots:',screenshot,'/tmp/project-alpha-gpu-mobile.png')
