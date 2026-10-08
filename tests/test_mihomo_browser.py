"""Central proxy UI: inheritance, templates, lifecycle, groups and stale replies."""
import copy
import json
import threading
from browser_support import BrowserHandler, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

worker, registry = 'e' * 32, 'f' * 32
writes = []
default_template = 'proxies: []\nproxy-groups: [{name: PROXY, type: select, proxies: [DIRECT]}]'
config = dict(binary='mihomo', mixed_port=7890, refresh_minutes=720,
    subscriptions=[], filters=[], filter='', template=default_template)
root = dict(revision=1, base_revision=1, inherited=False, default_template=default_template, config=config)
configs = {key: copy.deepcopy(root) for key in ['control', worker, registry]}
for key in [worker, registry]:
    configs[key].update(revision=0, inherited=True)
nodes = [dict(id=id, name=name, role=role, online=True, inherited=id != 'control', pending=False,
    sync=dict(error='', refreshed_at=1, delivered=True), error='',
    service=dict(running=True, ready=True, enabled=True, pid=123, digest='applied', proxy_count=2, error='', groups=[
        dict(name='PROXY', type='select', proxies=['DIRECT', '美国 A', '自动'], now='美国 A', resolved='美国 A'),
        dict(name='自动', type='url-test', proxies=['美国 A'], now='美国 A', resolved='美国 A')]))
    for id, name, role in [('control', '本机总控', 'control'), (worker, '计算节点', 'worker'), (registry, '注册入口', 'registry')]]
delay_reply = threading.Event()
delay_next = False


class Handler(BrowserHandler):
    def do_GET(self):
        global delay_next
        if self.path == '/api/session':
            return self.respond(dict(user=dict(id='a' * 32, username='admin', role='admin'), csrf='test', setup_required=False))
        if self.path == '/api/cluster/overview':
            return self.respond(dict(nodes=[], members=[], online=0, container_count=0, checked_at=1, partial=False))
        if self.path == '/api/mihomo/status':
            return self.respond(dict(nodes=nodes))
        if self.path.startswith('/api/mihomo/') and self.path.endswith('/settings'):
            value = copy.deepcopy(configs[self.path.split('/')[3]])
            if delay_next:
                delay_next = False
                delay_reply.wait(10)
            return self.respond(value)
        self.serve_asset()

    def mutate(self):
        assert self.headers['X-CSRF-Token'] == 'test'
        value = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        writes.append((self.path, copy.deepcopy(value)))
        parts = self.path.split('/')
        if self.path == '/api/mihomo/refresh':
            return self.respond(dict(queued=True), 202)
        id, action = parts[3:5]
        if action == 'settings':
            current = configs[id]
            if value['revision'] != current['revision']:
                return self.respond(dict(error='配置已修改，请重新载入'), 409)
            current.update(revision=current['revision'] + 1, inherited=value['inherit'], config=copy.deepcopy(configs['control']['config'] if value['inherit'] else value['config']))
            if id == 'control':
                current['base_revision'] = current['revision']
            return self.respond(current)
        if action == 'preview':
            if value['template'] == 'invalid':
                return self.respond(dict(error='策略组没有候选节点；请检查过滤规则'), 400)
            return self.respond(dict(proxy_count=2, groups=[{}], yaml=value['template']))
        if action == 'service':
            service = next(n for n in nodes if n['id'] == id)['service']
            service.update(running=value['action'] != 'stop', enabled=value['action'] != 'stop')
            return self.respond(service)
        if action == 'selection':
            service = next(n for n in nodes if n['id'] == id)['service']
            service['groups'][0].update(now=value['name'], resolved=value['name'])
            return self.respond(service)
        self.respond(dict(queued=True), 202)

    do_PUT = mutate
    do_POST = mutate


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1100))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(f'http://127.0.0.1:{server.server_port}/#mihomo')
        expect(page.locator('#pageTitle')).to_have_text('代理管理')
        expect(page.locator('#mihomoNodes tr')).to_have_count(3)
        expect(page.locator('#mihomoServiceStatus')).to_contain_text('代理运行中')
        page.locator('#mihomoAddSubscription').click()
        page.locator('[data-sub="name"]').fill('主订阅')
        page.locator('[data-sub="url"]').fill('https://subscription.invalid/?token=private')
        page.locator('#mihomoAddFilter').click()
        page.locator('[data-filter="name"]').fill('美国')
        page.locator('[data-filter="include"]').fill('美国|US')
        page.locator('[data-filter="exclude"]').fill(r'0\.0?1')
        page.locator('#mihomoPreview').click()
        expect(page.locator('#mihomoPreviewSummary')).to_contain_text('2 个代理节点')
        page.locator('#mihomoSave').click()
        expect(page.locator('#mihomoMessage')).to_contain_text('配置已保存')
        assert configs['control']['config']['filters'][0]['include'] == '美国|US'
        page.locator('#mihomoTarget').select_option(worker)
        expect(page.locator('#mihomoInherit')).to_be_checked()
        expect(page.locator('#mihomoBinary')).to_be_disabled()
        page.locator('#mihomoInherit').uncheck()
        page.locator('#mihomoPort').fill('7990')
        page.locator('#mihomoSave').click()
        expect(page.locator('#mihomoMessage')).to_contain_text('配置已保存')
        assert configs[worker]['config']['mixed_port'] == 7990 and not configs[worker]['inherited']
        page.locator('#mihomoInherit').check()
        expect(page.locator('#mihomoPort')).to_have_value('7890')
        page.locator('#mihomoSave').click()
        expect(page.locator('#mihomoMessage')).to_contain_text('配置已保存')
        assert configs[worker]['inherited']
        page.locator('#mihomoTarget').select_option(registry)
        expect(page.locator('#mihomoServiceStatus')).to_contain_text('registry')
        page.locator('[data-mihomo-service="stop"]').click()
        expect(page.locator('#mihomoServiceStatus')).to_contain_text('代理已停止')
        page.locator('[data-mihomo-group]').select_option('DIRECT')
        expect(page.locator('#mihomoMessage')).to_have_text('已保存 PROXY → DIRECT')
        assert writes[-1][0] == f'/api/mihomo/{registry}/selection'
        expect(page.locator('#mihomoGroups select')).to_have_count(1)
        page.locator('[data-mihomo-service="start"]').click()
        expect(page.locator('#mihomoServiceStatus')).to_contain_text('代理运行中')
        page.locator('#mihomoInherit').uncheck()
        page.locator('#mihomoTemplate').fill('invalid')
        page.locator('#mihomoPreview').click()
        expect(page.locator('#mihomoError')).to_contain_text('没有候选节点')
        page.locator('#mihomoDefaultTemplate').click()
        expect(page.locator('#mihomoTemplate')).to_have_value(default_template)
        # A delayed response from a previous node cannot replace the editor.
        delay_next = True
        page.locator('#mihomoTarget').select_option(worker)
        page.wait_for_timeout(100)
        page.locator('#mihomoTarget').select_option('control')
        expect(page.locator('#mihomoConfigTitle')).to_have_text('总控默认配置')
        delay_reply.set()
        page.wait_for_timeout(150)
        expect(page.locator('#mihomoConfigTitle')).to_have_text('总控默认配置')
        expect(page.locator('#mihomoInheritLabel')).to_be_hidden()
        page.screenshot(path='/tmp/project-alpha-mihomo-desktop.png', full_page=True)
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile overflow'
        page.screenshot(path='/tmp/project-alpha-mihomo-mobile.png', full_page=True)
        assert not errors, errors
        browser.close()
    print('Mihomo browser checks passed: centralized navigation, subscriptions, filters, preview, inheritance, per-role lifecycle, groups, stale replies and mobile layout.')
finally:
    delay_reply.set()
    server.shutdown()
    server.server_close()
