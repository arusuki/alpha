"""Node-scoped permission checks and one-shot sudo dialogs, with mocked host actions."""
import json
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright
from browser_support import NodeHandler, NODE_PATH, launch_options, start_server

user = dict(id='admin', username='admin', role='admin')
config = dict(endpoint='unix:///var/run/docker.sock', base_dir='/docker', image='training:test',
              start_port=2222, ssh_host='', proxy_jump='')
permissions = dict(username='node-service', uid=1001, endpoint=config['endpoint'], base_dir=config['base_dir'],
                   group_member=False, group_active=False, group_error='', restart_required=False,
                   directory_writable=False, directory_error='permission denied', docker_available=False,
                   docker_error='socket permission denied', sudo_error='')
actions = []


class Handler(NodeHandler):
    def do_GET(self):
        if self.control_request():
            return
        path = urlparse(self.path).path
        if path == '/api/session':
            return self.respond(dict(user=user, csrf='test', setup_required=False))
        if path == '/api/state':
            return self.respond(dict(jobs=[], directory_jobs=[], latest_id=None, active=None, interval_minutes=0))
        if path == '/api/containers':
            return self.respond(dict(managed=[], error='' if permissions['docker_available'] else 'socket permission denied'))
        if path == '/api/containers/settings':
            return self.respond(config)
        if path == '/api/containers/permissions':
            return self.respond(permissions)
        self.serve_asset()

    def do_POST(self):
        global user
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert self.headers.get('X-CSRF-Token') == 'test'
        if self.path == '/api/containers/permissions':
            password = body.pop('sudo_password')
            assert body['base_dir'] == config['base_dir'] and body['endpoint'] == config['endpoint']
            actions.append(body['action'])
            if password == 'incorrect':
                return self.respond(dict(error='sudo 认证失败'), 403)
            if body['action'] == 'docker_group':
                permissions.update(group_member=True, restart_required=True)
            else:
                permissions.update(directory_writable=True, directory_error='')
            return self.respond(permissions)
        if self.path == '/api/logout':
            user = None
            return self.respond(dict(ok=True))
        self.send_error(404)


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1050))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto('http://127.0.0.1:' + str(server.server_port) + NODE_PATH + '#containers')
        page.wait_for_function('!document.getElementById("containerGroupRepair").disabled')
        assert 'node-service' in page.locator('#containerPermissionAccount').inner_text()
        assert 'permission denied' in page.locator('#containerDirectoryStatus').inner_text()
        page.locator('#containerGroupRepair').click()
        assert 'node-service' in page.locator('#containerPermissionHint').inner_text()
        page.locator('#containerSudoPassword').fill('cancel-this')
        page.locator('#containerPermissionClose').click()
        assert page.locator('#containerSudoPassword').input_value() == '' and not actions
        page.locator('#containerGroupRepair').click()
        page.locator('#containerSudoPassword').fill('incorrect')
        page.locator('#containerPermissionForm button[type=submit]').click()
        page.wait_for_function('document.getElementById("containerPermissionDialogError").textContent.includes("sudo 认证失败")')
        assert page.locator('#containerSudoPassword').input_value() == ''
        assert page.locator('#containerPermissionDialog').is_visible()
        page.locator('#containerSudoPassword').fill('test-only-browser-secret')
        page.locator('#containerPermissionForm button[type=submit]').click()
        page.wait_for_function('!document.getElementById("containerPermissionDialog").open')
        assert page.locator('#containerGroupRepair').is_disabled()
        assert '重启 worker' in page.locator('#containerGroupStatus').inner_text()
        page.locator('#containerDirectoryRepair').click()
        assert '/docker' in page.locator('#containerPermissionHint').inner_text()
        # Empty password is supported for NOPASSWD; no secret enters the fixture log.
        page.locator('#containerPermissionForm button[type=submit]').click()
        page.wait_for_function('!document.getElementById("containerPermissionDialog").open')
        assert '可写' in page.locator('#containerDirectoryStatus').inner_text()
        assert page.locator('#containerDirectoryRepair').is_disabled()
        page.screenshot(path='/tmp/project-alpha-container-permissions.png', full_page=True)
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
        assert actions == ['docker_group', 'docker_group', 'directory']
        assert not errors, errors
        browser.close()
        print('Container permissions browser checks passed: node scope, password cleanup, failure retry, group restart and ACL repair.')
finally:
    server.shutdown()
