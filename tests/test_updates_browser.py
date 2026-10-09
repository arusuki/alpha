"""Update settings navigation, remote targeting, secrets and error feedback."""
import copy
import json
from browser_support import BrowserHandler, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

node = 'e' * 32
writes = []
shared_token = ''

def token_settings():
    return dict(revision=configs['control']['revision'], has_token=bool(shared_token), has_environment_token=False)
configs = {target: dict(revision=1, config=dict(command='/opt/alpha/bin/alpha-updater',
    http_proxy='', repo='arusuki/alpha', prerelease=False, automatic=False,
    has_webhook_secret=False, has_github_token=False), health=dict(mode=role, version='v0.3.2', healthy=True,
    update_state='idle'), webhook_path='/api/webhooks/github')
    for target, role in [('control', 'control'), (node, 'registry')]}

class Handler(BrowserHandler):
    def do_GET(self):
        if self.path == '/api/session':
            return self.respond(dict(user=dict(id='a'*32, username='admin', role='admin'), csrf='test', setup_required=False))
        if self.path == '/api/cluster/overview':
            return self.respond(dict(nodes=[], members=[], online=0, container_count=0, checked_at=1, partial=False))
        if self.path == '/api/updates/github-token':
            return self.respond(token_settings())
        if self.path == '/api/updates/targets':
            return self.respond(dict(nodes=[dict(id=node, name='公网入口', kind='registry', url='https://registry.example')]))
        if self.path.startswith('/api/updates/'):
            return self.respond(configs[self.path.split('/')[3]])
        self.serve_asset()

    def do_PUT(self):
        global shared_token
        assert self.headers['X-CSRF-Token'] == 'test'
        data = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path == '/api/updates/github-token':
            writes.append(('shared-token', copy.deepcopy(data)))
            if data['revision'] != configs['control']['revision']:
                return self.respond(dict(error='设置已修改，请重新载入'), 409)
            shared_token = '' if data['clear'] else data['token'] or shared_token
            configs['control']['revision'] += 1
            return self.respond(dict(settings=token_settings(), pending_nodes=[]))
        target = self.path.split('/')[3]
        writes.append((target, copy.deepcopy(data)))
        current = configs[target]
        if data['revision'] != current['revision']:
            return self.respond(dict(error='设置已修改，请重新载入'), 409)
        config = data['config']
        secret = config.pop('webhook_secret')
        clear = config.pop('clear_webhook_secret')
        config['has_webhook_secret'] = False if clear else bool(secret) or current['config']['has_webhook_secret']
        assert 'github_token' not in config and 'clear_github_token' not in config
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
        page.locator('#updateToken').fill('shared-test-token')
        page.get_by_role('button', name='保存统一 Token', exact=True).click()
        expect(page.locator('#updateTokenFields')).to_be_enabled()
        expect(page.locator('#updateToken')).to_have_value('')
        expect(page.locator('#updateTokenState')).to_contain_text('已保存统一 Token')
        assert writes[-1][0] == 'shared-token' and shared_token == 'shared-test-token'
        page.get_by_role('button', name='保存统一 Token', exact=True).click()
        expect(page.locator('#updateTokenFields')).to_be_enabled()
        assert writes[-1][1]['token'] == '' and shared_token == 'shared-test-token'
        page.locator('#updateTarget').select_option(node)
        expect(page.locator('#updateWebhook')).to_be_visible()
        expect(page.locator('#updateWebhookURL')).to_have_value('https://registry.example/api/webhooks/github')
        expect(page.locator('#updateTokenState')).to_contain_text('已保存统一 Token')
        page.locator('#updateSecret').fill('s'*32)
        page.get_by_role('button', name='保存更新设置', exact=True).click()
        expect(page.locator('#updateSecretState')).to_contain_text('已保存')
        expect(page.locator('#updateSecret')).to_have_value('')
        expect(page.locator('#updateToken')).to_have_value('')
        expect(page.locator('#updateTokenState')).to_contain_text('已保存统一 Token')
        page.get_by_role('button', name='保存更新设置', exact=True).click()
        expect(page.locator('#updateStatus')).to_have_text('更新设置已保存')
        page.locator('#updateToken').fill('rotated-test-token')
        page.get_by_role('button', name='保存统一 Token', exact=True).click()
        expect(page.locator('#updateTokenFields')).to_be_enabled()
        assert writes[-1][0] == 'shared-token' and shared_token == 'rotated-test-token'
        page.locator('#updateClearToken').check()
        page.get_by_role('button', name='保存统一 Token', exact=True).click()
        expect(page.locator('#updateTokenState')).to_contain_text('尚未配置统一 Token')
        expect(page.locator('#updateClearToken')).not_to_be_checked()
        assert shared_token == ''
        assert configs[node]['config']['has_webhook_secret']
        page.locator('#updateNow').click()
        expect(page.locator('#updateStatus')).to_contain_text('更新已接受')
        assert writes[-1][0] == f'/api/updates/{node}/update'
        page.screenshot(path='/tmp/project-alpha-updates-desktop.png', full_page=True)
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'mobile overflow'
        page.screenshot(path='/tmp/project-alpha-updates-mobile.png', full_page=True)
        assert not errors, errors
        browser.close()
    print('Update settings browser checks passed: target routing, proxy, automatic updates, write-only shared token and secret, central token rotation and clearing, update action and mobile layout.')
finally:
    server.shutdown()
    server.server_close()
