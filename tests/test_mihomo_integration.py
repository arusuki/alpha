"""Three real roles and a real mihomo core, using only temporary local services.

PROJECT_ALPHA_TEST_MIHOMO=/absolute/path/to/mihomo python3 tests/test_mihomo_integration.py
Optionally set PROJECT_ALPHA_TEST_BINARY to an already built project-alpha.
No subscriptions, binaries or traffic are downloaded by this test.
"""
import copy
import http.client
import http.cookiejar
import json
import os
from pathlib import Path
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

ROOT = Path(__file__).resolve().parents[1]
CORE = os.environ.get('PROJECT_ALPHA_TEST_MIHOMO')
if not CORE:
    print('Skipped real mihomo integration: set PROJECT_ALPHA_TEST_MIHOMO to an existing core.')
    raise SystemExit(0)
assert Path(CORE).is_absolute() and os.access(CORE, os.X_OK)


def port():
    with socket.socket() as s:
        s.bind(('127.0.0.1', 0))
        return s.getsockname()[1]


def wait_for(fn, seconds=30):
    deadline = time.monotonic() + seconds
    last = None
    while time.monotonic() < deadline:
        try:
            result = fn()
            if result:
                return result
        except (OSError, urllib.error.URLError, http.client.HTTPException) as e:
            last = e
        time.sleep(.15)
    raise AssertionError(f'timed out: {last}')


class API:
    def __init__(self, base):
        self.base, self.csrf = base, ''
        self.opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))

    def call(self, path, method='GET', data=None, expected=200):
        req = urllib.request.Request(self.base + path, method=method,
            data=json.dumps(data).encode() if data is not None else None,
            headers={'Content-Type': 'application/json', 'Origin': self.base, 'X-CSRF-Token': self.csrf})
        try:
            response = self.opener.open(req, timeout=45)
        except urllib.error.HTTPError as e:
            response = e
        with response:
            body = json.loads(response.read())
            assert response.status == expected, (path, response.status, body)
            return body


class Proxy(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        body = b'via local test proxy'
        self.send_response(200)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_CONNECT(self):
        self.send_response(200)
        self.end_headers()
        self.connection.settimeout(5)
        # The real core tunnels HTTP through an HTTP upstream with CONNECT.
        # Terminate the test request here; never connect to the requested host.
        while self.rfile.readline().strip():
            pass
        body = b'via local test proxy'
        self.wfile.write(b'HTTP/1.1 200 OK\r\nContent-Length: ' + str(len(body)).encode() + b'\r\nConnection: close\r\n\r\n' + body)


proxy = ThreadingHTTPServer(('127.0.0.1', 0), Proxy)
threading.Thread(target=proxy.serve_forever, daemon=True).start()
processes, logs = {}, []
with tempfile.TemporaryDirectory(prefix='alpha-mh-e2e-') as temp:
    root = Path(temp)
    binary = os.environ.get('PROJECT_ALPHA_TEST_BINARY', str(root / 'project-alpha'))
    if not os.environ.get('PROJECT_ALPHA_TEST_BINARY'):
        subprocess.run(['go', 'build', '-o', binary, './cmd/project-alpha'], cwd=ROOT, check=True)
    ports = {role: port() for role in ['control', 'worker', 'registry']}
    mixed = {role: port() for role in ports}
    token = 'local-mihomo-integration-token-123456'

    def launch(role):
        log = (root / (role + '.log')).open('ab')
        logs.append(log)
        env = dict(os.environ, PROJECT_ALPHA_WORKER_TOKEN=token, PROJECT_ALPHA_REGISTRY_TOKEN=token, REG_PASS='Testing1')
        # Keep tests independent of any configured egress proxy.
        for key in list(env):
            if key.lower().endswith('_proxy'):
                env.pop(key)
        processes[role] = subprocess.Popen([binary, '--' + role, '--data-dir', str(root / role), '--port', str(ports[role])], cwd=root, env=env, stdout=log, stderr=log)
        wait_for(lambda: socket.create_connection(('127.0.0.1', ports[role]), timeout=.3).close() is None)

    def stop(role, kill=False):
        proc = processes[role]
        proc.kill() if kill else proc.terminate()
        proc.wait(timeout=15)

    try:
        for role in ports:
            launch(role)
        api = API(f'http://127.0.0.1:{ports["control"]}')
        session = api.call('/api/setup', 'POST', dict(username='admin', password='Test-password-12345'))
        api.csrf = session['csrf']
        ids = {'control': 'control'}
        for role in ['worker', 'registry']:
            api.call('/api/cluster/nodes', 'POST', dict(kind=role, name='local ' + role,
                url=f'http://127.0.0.1:{ports[role]}', token=token,
                internal_ip='10.1.2.3' if role == 'worker' else ''), expected=201)
        for node in api.call('/api/cluster/nodes')['nodes']:
            ids[node['kind']] = node['id']
        status = lambda: api.call('/api/mihomo/status')['nodes']
        assert len(status()) == 3
        settings = api.call('/api/mihomo/control/settings')
        config = settings['config']
        config.update(binary=CORE, refresh_minutes=0, subscriptions=[dict(name='local', url='', prefix='', content=f'''proxies:
  - {{name: 美国 A, type: http, server: 127.0.0.1, port: {proxy.server_port}}}
  - {{name: 日本 A, type: http, server: 127.0.0.1, port: {proxy.server_port}}}
''')], filters=[dict(name='美国', include='美国', exclude='', types=['http'], sources=['local'])])
        config['template'] = '''mixed-port: {{ if eq .Node.Role "control" }}%d{{ else if eq .Node.Role "worker" }}%d{{ else }}%d{{ end }}
allow-lan: false
mode: rule
log-level: silent
proxies:
{{ yaml .Proxies | indent 2 }}
proxy-groups:
  - name: 出口 / 主
    type: select
    proxies:
      - DIRECT
{{ range (names "美国") }}      - {{ quote . }}
{{ end }}rules:
  - MATCH,出口 / 主
''' % (mixed['control'], mixed['worker'], mixed['registry'])

        def save(id, value, inherit=False):
            current = api.call(f'/api/mihomo/{id}/settings')
            return api.call(f'/api/mihomo/{id}/settings', 'PUT', dict(revision=current['revision'], base_revision=current['base_revision'], inherit=inherit, config=value))

        preview = api.call('/api/mihomo/control/preview', 'POST', config)
        assert preview['proxy_count'] == 2 and preview['groups'][0]['proxies'] == ['DIRECT', '美国 A']
        save('control', config)
        wait_for(lambda: all(n['sync']['delivered'] and n['service']['digest'] for n in status()))
        for role, id in ids.items():
            api.call(f'/api/mihomo/{id}/service', 'POST', dict(action='start'))
            selected = api.call(f'/api/mihomo/{id}/selection', 'PUT', dict(group='出口 / 主', name='美国 A'))
            assert selected['running'] and selected['groups'][0]['now'] == '美国 A'
            conn = http.client.HTTPConnection('127.0.0.1', mixed[role], timeout=5)
            conn.request('GET', 'http://local-test.invalid/example')
            response = conn.getresponse()
            body = response.read()
            assert response.status == 200 and body == b'via local test proxy', (role, response.status, body[:500], selected)
            conn.close()
        old = {n['id']: n['service']['digest'] for n in status()}
        broken = copy.deepcopy(config)
        broken['template'] = broken['template'].replace('allow-lan: false', 'allow-lan: [invalid]')
        save('control', broken)
        wait_for(lambda: all(n['sync']['error'] for n in status()))
        assert all(n['service']['running'] and n['service']['digest'] == old[n['id']] for n in status())
        save('control', config)
        wait_for(lambda: all(n['sync']['delivered'] and not n['sync']['error'] for n in status()))
        override = copy.deepcopy(config)
        override['template'] += 'ipv6: true\n'
        save(ids['worker'], override)
        wait_for(lambda: next(n for n in status() if n['id'] == ids['worker'])['sync']['delivered'])
        assert not api.call(f'/api/mihomo/{ids["worker"]}/settings')['inherited']
        save(ids['worker'], {}, inherit=True)
        wait_for(lambda: all(not n['pending'] for n in status()))
        # Parent crash must stop the core and restart must recover its choice.
        pid = next(n for n in status() if n['id'] == ids['worker'])['service']['pid']
        stop('worker', kill=True)
        wait_for(lambda: not Path(f'/proc/{pid}').exists() or ') Z ' in Path(f'/proc/{pid}/stat').read_text())
        launch('worker')
        wait_for(lambda: next(n for n in status() if n['id'] == ids['worker'])['service']['running'])
        recovered = next(n for n in status() if n['id'] == ids['worker'])
        assert recovered['service']['groups'][0]['now'] == '美国 A'
        # Stop persists independently of inherited configuration.
        api.call(f'/api/mihomo/{ids["registry"]}/service', 'POST', dict(action='stop'))
        stop('registry')
        launch('registry')
        stopped = next(n for n in status() if n['id'] == ids['registry'])
        assert not stopped['service']['running'] and not stopped['service']['enabled']
        # The entire management surface, including reads, is admin-only.
        api.call('/api/users', 'POST', dict(username='viewer', password='Test-password-12345', role='viewer'), expected=201)
        viewer = API(api.base)
        viewer.csrf = viewer.call('/api/login', 'POST', dict(username='viewer', password='Test-password-12345'))['csrf']
        viewer.call('/api/mihomo/status', expected=403)
        viewer.call('/api/mihomo/control/settings', expected=403)
        viewer.call('/api/mihomo/control/service', 'POST', dict(action='stop'), expected=403)
        API(f'http://127.0.0.1:{ports["worker"]}').call('/api/node-mihomo/status', expected=401)
        API(f'http://127.0.0.1:{ports["registry"]}').call('/api/node-mihomo/status', expected=401)
        print('Real mihomo integration passed: all three roles, inherited templates, actual proxy traffic, selection, failed reload preservation, override reset, parent crash, recovery and authorization.')
    finally:
        for proc in processes.values():
            if proc.poll() is None:
                proc.terminate()
                try:
                    proc.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    proc.kill()
                    proc.wait()
        for log in logs:
            log.close()
        proxy.shutdown()
        proxy.server_close()
