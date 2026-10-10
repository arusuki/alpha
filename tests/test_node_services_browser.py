"""Node service controls, saved user selection, failures and mobile layout."""
import json
from urllib.parse import urlsplit
from browser_support import NodeHandler, NODE_PATH, PROXY_PATH, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

services=[]
for name in ['tetragon','dram-bw','rootless-docker']:
    rootless=name=='rootless-docker'
    services.append(dict(name=name,state='running',checked_at=1,config=dict(restart_policy='unless-stopped',stop_timeout=120 if rootless else 10,control_socket='/run/rootless-docker/control.sock' if rootless else '',socket_path='/var/run/docker.sock' if rootless else '',users=[])))
services[1]['config']['dram']=dict(backend='amd-rome',interval_us=100000,peak_gbps=0)
calls=[]
mode='normal'
class Handler(NodeHandler):
    def do_GET(self):
        if self.control_request():return
        path=urlsplit(self.path).path
        if path=='/api/session':return self.respond(dict(user=dict(id='admin',username='admin',role='admin'),csrf_token='token'))
        if path=='/api/state':return self.respond(dict(jobs=[],directory_jobs=[],latest_id=None,active=None,interval_minutes=0))
        if path=='/api/containers/services':
            if mode=='offline':return self.respond(dict(error='节点离线'),503)
            return self.respond(dict(services=services,candidates=[dict(owner='alice',name='<training>&alice',container_id='a'*64,initialized=True)],mounts=[dict(owner='alice',name='<training>&alice',container_id='a'*64,socket_path='/var/run/docker.sock',state='mounted',error='')] if 'alice' in services[2]['config']['users'] else [],external_mounts=[],mount_error=''))
        self.serve_asset()
    def mutate(self):
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        original=self.requestline.split()[1]
        assert original.startswith(PROXY_PATH+'/api/containers/services/'),original
        name,action=self.path.rsplit('/',2)[-2:]
        calls.append((name,action,body))
        if mode=='failure':return self.respond(dict(error='挂载撤销失败，原记录已保留'),409)
        service=next(s for s in services if s['name']==name)
        if action=='settings':
            service['config']=body
            if name=='dram-bw' and service['state']=='restarting':service['state']='running'
        elif action=='stop':service['state']='exited'
        elif action in ('start','restart'):service['state']='running'
        self.respond(dict(ok=True))
    do_POST=mutate
    do_PUT=mutate


def capture_preview(page, path):
    # Full-page screenshots retain sticky elements at the current scroll offset.
    # Form interactions scroll to the save button, so return to the page top first.
    page.evaluate('window.scrollTo(0, 0)')
    page.wait_for_function('window.scrollY === 0')
    page.screenshot(path=path, full_page=True)

server=start_server(Handler)
try:
    with sync_playwright() as p:
        browser=p.chromium.launch(**launch_options())
        page=browser.new_page(viewport=dict(width=1440,height=1100))
        errors=[];page.on('pageerror',lambda e:errors.append(str(e)))
        page.goto(f'http://127.0.0.1:{server.server_port}{NODE_PATH}#services')
        expect(page.locator('.service-card')).to_have_count(3)
        expect(page.locator('button[data-page=services]')).to_be_visible()
        expect(page.locator('.service-card:visible')).to_have_count(1)
        page.locator('[data-service=tetragon] input[name=stop_timeout]').fill('35')
        page.locator('[data-service-select="rootless-docker"]').click()
        root=page.locator('[data-service="rootless-docker"]')
        root.locator('input[name=users][value=alice]').check()
        root.locator('input[name=socket_path]').fill('/run/custom/docker.sock')
        root.locator('[data-default-path="socket_path"]').click()
        expect(root.locator('input[name=socket_path]')).to_have_value('/var/run/docker.sock')
        root.get_by_role('button',name='保存并应用配置').click()
        expect(page.locator('#servicesMessage')).to_have_text('操作已完成。')
        assert calls[-1][2]['users']==['alice']
        expect(page.locator('[data-service=tetragon] input[name=stop_timeout]')).to_have_value('35')
        expect(page.locator('#serviceMounts')).to_contain_text('<training>&alice')
        assert page.locator('#serviceMounts training').count()==0
        for action,label,state in [('stop','停止','已停止'),('start','启动','运行中'),('restart','重启','运行中'),('apply','重试应用挂载','运行中')]:
            with page.expect_response(lambda r:'/rootless-docker/'+action in r.url):root.get_by_role('button',name=label,exact=True).click()
            expect(page.locator('#servicesMessage')).to_have_text('操作已完成。')
            expect(root.locator('.service-state')).to_have_text(state)
        page.locator('[data-service-select="dram-bw"]').click()
        expect(page.locator('#serviceMountPanel')).to_be_hidden()
        dram=page.locator('[data-service="dram-bw"]')
        dram.locator('[name=interval_us]').fill('250000')
        expect(dram.locator('[data-frequency]')).to_contain_text('4 Hz')
        dram.locator('[name=backend]').select_option('auto')
        dram.locator('[name=peak_gbps]').fill('256')
        dram.get_by_role('button',name='保存并应用配置').click()
        expect(page.locator('#servicesMessage')).to_have_text('操作已完成。')
        assert calls[-1][2]['dram']==dict(backend='auto',interval_us=250000,peak_gbps=256)
        services[1]['state']='restarting'
        page.locator('#servicesRefresh').click()
        expect(dram.locator('.service-state')).to_have_text('重启中')
        expect(dram.get_by_role('button',name='停止',exact=True)).to_be_enabled()
        expect(dram.get_by_role('button',name='重启',exact=True)).to_be_enabled()
        expect(dram.get_by_role('button',name='启动',exact=True)).to_be_disabled()
        dram.locator('[name=backend]').select_option('mock')
        dram.get_by_role('button',name='保存并应用配置').click()
        expect(page.locator('#servicesMessage')).to_have_text('操作已完成。')
        expect(dram.locator('.service-state')).to_have_text('运行中')
        assert calls[-1][2]['dram']['backend']=='mock'
        services[1]['state']='restarting'
        page.locator('#servicesRefresh').click()
        expect(dram.locator('.service-state')).to_have_text('重启中')
        dram.get_by_role('button',name='停止',exact=True).click()
        expect(dram.locator('.service-state')).to_have_text('已停止')
        dram.get_by_role('button',name='启动',exact=True).click()
        expect(dram.locator('.service-state')).to_have_text('运行中')
        capture_preview(page, '/tmp/alpha-node-services-dram.png')
        page.locator('[data-service-select="rootless-docker"]').click()
        mode='failure' 
        root.get_by_role('button',name='停止',exact=True).click()
        expect(page.locator('#servicesError')).to_contain_text('挂载撤销失败')
        expect(root.locator('.service-state')).to_have_text('运行中')
        capture_preview(page, '/tmp/alpha-node-services-desktop.png')
        page.set_viewport_size(dict(width=390,height=844))
        assert page.evaluate('document.documentElement.scrollWidth<=innerWidth'),'mobile overflows'
        capture_preview(page, '/tmp/alpha-node-services-mobile.png')
        root.locator('input[name=socket_path]').fill('/run/draft.sock')
        mode='offline';page.locator('#servicesRefresh').click()
        expect(page.locator('#servicesError')).to_have_text('节点离线')
        expect(page.locator('.service-card')).to_have_count(0)
        mode='normal';page.locator('#servicesRefresh').click()
        expect(page.locator('.service-card')).to_have_count(3)
        expect(root.locator('input[name=socket_path]')).to_have_value('/run/draft.sock')
        expect(page.locator('.service-card:visible')).to_have_count(1)
        assert not errors,errors
        browser.close()
finally:server.shutdown()
print('Node service browser checks passed')
