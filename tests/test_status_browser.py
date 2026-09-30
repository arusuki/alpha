"""Member status page: identity, node states, synchronous creation and stale responses."""
import copy
import json
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
member = 'a' * 32
username = 'alice'
token = 'b' * 64
endpoint = '/api/status/' + username
nodes = []
for index, (name, online, state) in enumerate([
    ('已有账号', True, 'ready'), ('可申请节点', True, 'unallocated'),
    ('离线节点', False, 'unallocated'), ('<img src=x onerror=alert(1)>', True, 'failed'),
    ('管理员分配', True, 'unallocated'), ('等待分配', True, 'pending'),
]):
    nodes.append(dict(node_id=str(index + 1) * 32, node_name=name, online=online, state=state,
                      internal_ip='10.0.0.' + str(11 + index), host='compute-one', container_count=5,
                      observed_at='', scanning=False, containers=[], container_id='', name='',
                      port=0, error='默认镜像不可用' if state == 'failed' else ''))
nodes[0].update(container_id='c' * 64, name='alpha-existing', port=2222)
nodes[4]['containers'] = [dict(id='d' * 64, name='manual-alice', state='running', managed=True)]
writes = []
fail = True
hold = threading.Event()
started = threading.Event()
hold.set()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, code, value, content_type='application/json'):
        body = json.dumps(value).encode() if content_type == 'application/json' else value
        self.send_response(code)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def authorized(self):
        if self.headers.get('Authorization') != 'Bearer ' + token:
            self.send(401, dict(error='资源令牌无效'))
            return False
        if not self.path.startswith(endpoint):
            self.send(403, dict(error='资源令牌与页面使用者不匹配'))
            return False
        return True

    def do_GET(self):
        if self.path.startswith('/api/status/'):
            if self.authorized():
                self.send(200, dict(member_id=member, username='alice', control=dict(internal_ip='100.100.0.1', status_url='http://100.64.0.2:9765/status/alice'), access=dict(key_state='ready', invite_state='invited', share_host='100.64.0.2', share_ssh_port=2222, status_port=9765), nodes=copy.deepcopy(nodes), checked_at=1800000000))
            return
        filename = 'status.html' if self.path.startswith('/status/') else self.path.removeprefix('/')
        if filename not in ('status.html', 'status.js', 'status.css'):
            self.send(404, {})
            return
        mime = {'html': 'text/html', 'js': 'application/javascript', 'css': 'text/css'}[filename.split('.')[-1]]
        self.send(200, (repo / 'dist' / filename).read_bytes(), mime + '; charset=utf-8')

    def do_POST(self):
        if not self.authorized():
            return
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        writes.append(body)
        started.set()
        hold.wait(15)
        node = next(n for n in nodes if n['node_id'] == body['node_id'])
        if fail:
            node.update(state='failed', error='默认镜像不可用')
            self.send(409, dict(error='默认镜像不可用'))
            return
        node.update(state='ready', error='', container_id='e' * 64, name='alpha-new', port=2223)
        self.send(200, dict(member_id=member, nodes=copy.deepcopy(nodes)))


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
url = 'http://127.0.0.1:' + str(server.server_port)
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        page = browser.new_page(viewport=dict(width=1440, height=1080))
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.goto(url + '/status/' + username)
        expect(page.locator('#memberIdentity')).to_have_text('使用者 · alice')
        expect(page.locator('#tokenPanel')).to_be_visible()
        expect(page.locator('#statusContent')).to_be_hidden()
        page.locator('#resourceToken').fill('invalid')
        page.locator('#tokenSubmit').click()
        expect(page.locator('#statusError')).to_contain_text('资源令牌无效')
        page.locator('#resourceToken').fill(token)
        page.locator('#tokenSubmit').click()
        expect(page.locator('#totalNodes')).to_have_text('6')
        assert page.evaluate('sessionStorage.getItem("alpha.member-token.alice")') == token
        expect(page.locator('#onlineNodes')).to_have_text('5')
        expect(page.locator('#allocatedNodes')).to_have_text('2')
        expect(page.locator('#statusNodes')).to_contain_text('alpha-existing')
        expect(page.locator('#statusNodes')).to_contain_text('manual-alice')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 100.64.0.2')
        expect(page.locator('#sshConfig')).to_contain_text('User alpha-jump')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 10.0.0.11')
        expect(page.locator('#sshConfig')).to_contain_text('Port 2222')
        expect(page.locator('#sshConfig')).to_contain_text('ProxyJump alpha-jump')
        page.context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=url)
        page.locator('#copySSHConfig').click()
        expect(page.locator('#sshCopyStatus')).to_contain_text('SSH 配置已复制')
        assert page.evaluate('navigator.clipboard.readText()') == page.locator('#sshConfig').inner_text()
        expect(page.locator('#controlStatusAddress')).to_contain_text('/status/alice')
        assert 'node.test' not in page.locator('#statusNodes').inner_text()
        assert page.locator('#sshCommands img').count() == 0
        assert page.locator('#statusNodes img').count() == 0
        assert page.locator('[data-node="' + nodes[4]['node_id'] + '"] [data-apply]').count() == 0
        expect(page.locator('[data-apply="' + nodes[2]['node_id'] + '"]')).to_be_disabled()
        assert page.locator('[data-node="' + nodes[5]['node_id'] + '"] [data-apply]').count() == 0
        assert token not in page.url
        assert page.locator('#resourceToken').input_value() == ''
        page.reload()
        expect(page.locator('#statusContent')).to_be_visible()
        target = page.locator('[data-apply="' + nodes[1]['node_id'] + '"]')
        target.click()
        expect(page.locator('#statusError')).to_contain_text('默认镜像不可用')
        expect(target).to_be_enabled()
        fail = False
        hold.clear()
        started.clear()
        target.click()
        assert started.wait(5)
        expect(target).to_be_disabled()
        expect(page.locator('#statusMessage')).to_contain_text('正在为 可申请节点 创建容器')
        assert len(writes) == 2
        hold.set()
        expect(page.locator('#statusMessage')).to_contain_text('创建成功 · alpha-new')
        expect(page.locator('#allocatedNodes')).to_have_text('3')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 10.0.0.12')
        expect(page.locator('#sshConfig')).to_contain_text('Port 2223')
        assert target.count() == 0
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.screenshot(path='/tmp/alpha-status-mobile.png', full_page=True)
        page.set_viewport_size(dict(width=1440, height=1080))
        page.screenshot(path='/tmp/alpha-status-desktop.png', full_page=True)
        # Leaving while a creation response is outstanding cannot restore private data.
        hold.clear()
        started.clear()
        page.locator('[data-apply="' + nodes[3]['node_id'] + '"]').click()
        assert started.wait(5)
        page.locator('#forgetToken').click()
        hold.set()
        expect(page.locator('#statusContent')).to_be_hidden()
        assert page.locator('#statusNodes').inner_text() == ''
        assert page.locator('#sshConfig').inner_text() == ''
        assert page.evaluate('sessionStorage.length') == 0
        page.wait_for_timeout(200)
        expect(page.locator('#statusContent')).to_be_hidden()
        page.goto(url + '/status/bob')
        page.locator('#resourceToken').fill(token)
        page.locator('#tokenSubmit').click()
        expect(page.locator('#statusError')).to_contain_text('与页面使用者不匹配')
        expect(page.locator('#statusContent')).to_be_hidden()
        assert not errors, errors
        browser.close()
    print('Member status browser checks passed.')
finally:
    hold.set()
    server.shutdown()
    server.server_close()
