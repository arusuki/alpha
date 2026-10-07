"""Update settings navigation, remote targeting, secrets and error feedback."""
import copy
import json
from browser_support import BrowserHandler, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

node = 'e' * 32
writes = []
configs = {target: dict(revision=1, config=dict(command='/opt/alpha/bin/alpha-updater',
    http_proxy='', repo='arusuki/alpha', prerelease=False, automatic=False,
    has_webhook_secret=False), health=dict(mode=role, version='v0.3.2', healthy=True,
    update_state='idle'), webhook_path='/api/webhooks/github')
    for target, role in [('control', 'control'), (node, 'registry')]}

class Handler(BrowserHandler):
    def do_GET(self):
        if self.path == '/api/session':
            return self.respond(dict(user=dict(id='a'*32, username='admin', role='admin'), csrf='test', setup_required=False))
        if self.path == '/api/cluster/overview':
            return self.respond(dict(nodes=[], members=[], online=0, container_count=0, checked_at=1, partial=False))
        if self.path == '/api/updates/targets':
            return self.respond(dict(nodes=[dict(id=node, name='公网入口', kind='registry', url='https://registry.example')]))
        if self.path.startswith('/api/updates/'):
            return self.respond(configs[self.path.split('/')[3]])
        self.serve_asset()

    def do_PUT(self):
        assert self.headers['X-CSRF-Token'] == 'test'
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        target = self.path.split('/')[3]
        writes.append((target, copy.deepcopy(data)))
        current = configs[target]
        if data['revision'] != current['revision']:
            return self.respond(dict(error='设置已修改，请重新载入'), 409)
        config = data['config']
        secret = config.pop('webhook_secret')
        clear = config.pop('clear_webhook_secret')
        config['has_webhook_secret'] = False if clear else bool(secret) or current['config']['has_webhook_secret']
        current.update(revision=current['revision']+1, config=config)
        self.respond(current)

    def do_POST(self):
        assert self.headers['X-CSRF-Token'] == 'test'
        writes.append((self.path, {}))
        self.respond(dict(accepted=True), 202)

server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1100))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(f'http://127.0.0.1:{server.server_port}/#update-settings')
        expect(page.locator('#pageTitle')).to_have_text('更新设置')
        expect(page.locator('#updateHealth')).to_contain_text('control · v0.3.2')
        page.locator('#updateProxy').fill('http://127.0.0.1:7890')
        page.locator('#updateAutomatic').check()
        page.get_by_role('button', name='保存更新设置', exact=True).click()
        expect(page.locator('#updateStatus')).to_have_text('更新设置已保存')
        assert writes[-1][0] == 'control' and writes[-1][1]['config']['http_proxy'].endswith(':7890')
        page.locator('#updateTarget').select_option(node)
        expect(page.locator('#updateWebhook')).to_be_visible()
        expect(page.locator('#updateWebhookURL')).to_have_value('https://registry.example/api/webhooks/github')
        page.locator('#updateSecret').fill('s'*32)
        page.get_by_role('button', name='保存更新设置', exact=True).click()
        expect(page.locator('#updateSecretState')).to_contain_text('已保存')
        expect(page.locator('#updateSecret')).to_have_value('')
        page.get_by_role('button', name='保存更新设置', exact=True).click()
        expect(page.locator('#updateStatus')).to_have_text('更新设置已保存')
        page.locator('#updateNow').click()
        expect(page.locator('#updateStatus')).to_contain_text('更新已接受')
        assert writes[-1][0] == f'/api/updates/{node}/update'
        page.screenshot(path='/tmp/project-alpha-updates-desktop.png', full_page=True)
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile overflow'
        page.screenshot(path='/tmp/project-alpha-updates-mobile.png', full_page=True)
        assert not errors, errors
        browser.close()
    print('Update settings browser checks passed: target routing, proxy, automatic updates, write-only secret, update action and mobile layout.')
finally:
    server.shutdown()
    server.server_close()
