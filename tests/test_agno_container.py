"""Real Compose Agno + Go control; local model, no paid API calls.
Requires Docker and ALPHA_AGNO_TEST_IMAGE pointing to a built test image.
"""
import http.cookiejar
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import re
import socket
import subprocess
import tempfile
import threading
import time
import urllib.error
import urllib.request
import uuid

REPO = Path(__file__).resolve().parents[1]
image = os.environ['ALPHA_AGNO_TEST_IMAGE']
requests = []
mcp_calls = []
blocking = threading.Event()
release_model = threading.Event()


class Model(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_GET(self):
        self.send_error(405)

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path == '/mcp':
            assert self.headers.get('Authorization') == 'Bearer fixture-mcp-token'
            method=body['method']
            if method=='notifications/initialized':
                self.send_response(202);self.end_headers();return
            if method=='initialize':
                result=dict(protocolVersion=body['params']['protocolVersion'],capabilities=dict(tools={}),serverInfo=dict(name='fixture',version='1'))
            elif method=='tools/list':
                result=dict(tools=[dict(name=name,description=name,inputSchema=dict(type='object',properties={})) for name in ['status','delete']])
            elif method=='tools/call':
                assert body['params']['name']=='status'
                mcp_calls.append(body['params'])
                result=dict(content=[dict(type='text',text='MCP fixture healthy')],isError=False)
            else:
                self.send_error(400);return
            raw=json.dumps(dict(jsonrpc='2.0',id=body['id'],result=result)).encode()
            self.send_response(200);self.send_header('Content-Type','application/json');self.send_header('Content-Length',str(len(raw)));self.end_headers();self.wfile.write(raw);return
        requests.append(body)
        if len(requests) > 30:
            self.send_error(500, 'Unexpected model loop')
            return
        if any(m.get('content') == 'block for cancellation' for m in body['messages']):
            blocking.set()
            release_model.wait(20)
            return
        assert self.headers.get('Authorization') == 'Bearer fixture-model-token'
        names = {t['function']['name'] for t in body.get('tools', [])}
        call_names = {call['id']: call['function']['name'] for m in body['messages']
                      for call in m.get('tool_calls', [])}
        used = {call_names.get(m.get('tool_call_id')) for m in body['messages'] if m['role'] == 'tool'}
        name, args = None, {}
        if 'mcp_metrics_status' in names and 'mcp_metrics_status' not in used:
            assert 'mcp_metrics_delete' not in names and 'get_node_gpu' not in names
            assert '先检查容器 MCP' in json.dumps(body,ensure_ascii=False)
            name = 'mcp_metrics_status'
        elif 'add_memory' in names:
            if 'add_memory' not in used:
                name, args = 'add_memory', dict(memory='巡检先检查 GPU', topics=['operations'])
        elif 'list_nodes' not in used:
            name = 'list_nodes'
        elif 'update_user_memory' not in used:
            name, args = 'update_user_memory', dict(task='记住巡检先检查 GPU')
        message = dict(role='assistant', content='检查完成，已记住先看 GPU。')
        if name:
            message.update(content=None, tool_calls=[dict(id='call_' + uuid.uuid4().hex, type='function', function=dict(name=name, arguments=json.dumps(args)))])
        raw = json.dumps(dict(id='completion', object='chat.completion', created=1, model='test',
            choices=[dict(index=0, message=message, finish_reason='tool_calls' if name else 'stop')])).encode()
        self.send_response(200)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


model = ThreadingHTTPServer(('127.0.0.1', 0), Model)
threading.Thread(target=model.serve_forever, daemon=True).start()
try:
    with tempfile.TemporaryDirectory(prefix='agno-container-') as temporary:
        root = Path(temporary)
        sockets = root / 'socket'
        sockets.mkdir(mode=0o700)
        env = dict(os.environ, AGNO_UID=str(os.getuid()), AGNO_GID=str(os.getgid()),
                   AGNO_SOCKET_DIR=str(sockets), AGNO_IMAGE=image,
                   ALPHA_AGNO_SOCKET=str(sockets / 'agno.sock'))
        project = 'agno-check-' + uuid.uuid4().hex[:10]
        compose = ['docker', 'compose', '-p', project, '-f', str(REPO / 'deploy/control-services.yaml')]
        control = None
        log = (root / 'control.log').open('w+')

        def docker(*args):
            return subprocess.check_output(compose + list(args), env=env, text=True).strip()

        def socket_health():
            with socket.socket(socket.AF_UNIX) as client:
                client.settimeout(3)
                client.connect(str(sockets / 'agno.sock'))
                client.sendall(b'{"protocol":2,"operation":"health"}\n')
                with client.makefile('rb') as reader:
                    return json.loads(reader.readline())

        def wait_ready():
            until = time.monotonic() + 30
            while time.monotonic() < until:
                try:
                    if socket_health()['ready']:
                        return
                except (OSError, ValueError, KeyError):
                    pass
                time.sleep(.1)
            raise AssertionError('container did not become ready: ' + docker('logs', '--no-color', 'agno'))

        try:
            docker('up', '-d', '--no-build')
            wait_ready()
            info = json.loads(subprocess.check_output(['docker', 'inspect', docker('ps', '-q', 'agno')], text=True))[0]
            assert info['Config']['User'] == env['AGNO_UID'] + ':' + env['AGNO_GID']
            assert info['HostConfig']['ReadonlyRootfs'] and info['HostConfig']['CapDrop'] == ['ALL']
            assert not info['Config'].get('Healthcheck'), 'unexpected periodic health check'
            assert not info['HostConfig'].get('PortBindings')
            assert [m['Destination'] for m in info['Mounts'] if m['Type'] == 'bind'] == ['/run/agno']
            assert (sockets / 'agno.sock').stat().st_mode & 0o777 == 0o600
            binary = root / 'project-alpha'
            subprocess.run(['go', 'generate', './internal/platform'], cwd=REPO, env=env, check=True, stdout=subprocess.DEVNULL)
            subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=REPO, env=env, check=True)
            control = subprocess.Popen([str(binary), '--control', '--data-dir', str(root / 'data'), '--port', '0'],
                                       env=env, stdout=log, stderr=log)
            url = None
            for _ in range(100):
                log.seek(0)
                match = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', log.read())
                if match:
                    url = match[1]
                    break
                if control.poll() is not None:
                    break
                time.sleep(.1)
            assert url, 'Go control did not start'
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
            csrf = ''

            def api(method, path, body=None):
                request = urllib.request.Request(url + path, method=method,
                    data=json.dumps(body).encode() if body is not None else None,
                    headers={'Content-Type': 'application/json', 'X-CSRF-Token': csrf})
                try:
                    response = opener.open(request, timeout=45)
                except urllib.error.HTTPError as error:
                    response = error
                with response:
                    return response.status, json.loads(response.read())

            code, login = api('POST', '/api/setup', dict(username='admin', password='Fixture-password-123'))
            assert code == 200, login
            csrf = login['csrf']
            code, cfg = api('GET', '/api/agent/settings')
            assert code == 200
            code, value = api('PUT', '/api/agent/settings', dict(revision=cfg['revision'], value=dict(
                protocol='completions', endpoint=f'http://127.0.0.1:{model.server_port}/v1', model='test',
                api_key='fixture-model-token', timeout_seconds=30)))
            assert code == 200, value
            code, health = api('GET', '/api/assistant/health')
            assert code == 200 and health['status'] == 'ready', health
            assert not requests, 'health check contacted the model'
            code, settings = api('GET', '/api/assistant/settings')
            assert code==200,settings
            settings['value'].update(instructions='先检查容器 MCP，再列出节点并记住用户偏好',builtin_tools=['list_nodes'],mcp_servers=[dict(id='metrics',url=f'http://127.0.0.1:{model.server_port}/mcp',enabled=True,tools=['status'])])
            code, settings = api('PUT','/api/assistant/settings',dict(revision=settings['revision'],value=settings['value'],tokens=dict(metrics='fixture-mcp-token'),clear_tokens=[]))
            assert code==200 and settings['has_tokens']['metrics'],settings
            assert 'fixture-mcp-token' not in json.dumps(settings)
            settings['value']['mcp_servers'][0]['tools']=['missing']
            code, settings = api('PUT','/api/assistant/settings',dict(revision=settings['revision'],value=settings['value'],tokens={},clear_tokens=[]))
            assert code==200,settings
            code, missing = api('POST','/api/assistant/sessions',dict(message='missing MCP tool'))
            assert code==502 and 'MCP' in missing['error'],missing
            assert not requests and not mcp_calls,'invalid tool configuration contacted model or invoked tools'
            assert api('GET','/api/assistant/sessions')[1]['sessions']==[]
            settings['value']['mcp_servers'][0]['tools']=['status']
            code, settings = api('PUT','/api/assistant/settings',dict(revision=settings['revision'],value=settings['value'],tokens={},clear_tokens=[]))
            assert code==200 and settings['has_tokens']['metrics'],settings
            code, turn = api('POST', '/api/assistant/sessions', dict(message='检查节点，记住巡检先检查 GPU'))
            assert code == 200, turn
            assert len(mcp_calls)==1,mcp_calls
            assert any(t['name']=='mcp:metrics:status' for t in turn['tools']),turn
            session_id = turn['id']
            code, memory = api('GET', '/api/assistant/memories')
            assert code == 200 and memory['memories'][0]['memory'] == '巡检先检查 GPU', memory
            docker('restart', 'agno')
            wait_ready()
            code, saved = api('GET', '/api/assistant/sessions/' + session_id)
            assert code == 200 and saved['messages'] == turn['messages']
            code, saved_memory = api('GET', '/api/assistant/memories')
            assert saved_memory == memory
            code, followup = api('POST', '/api/assistant/sessions/' + session_id + '/messages', dict(message='继续'))
            assert code == 200 and len(followup['messages']) == 4, followup
            assert '巡检先检查 GPU' in json.dumps(requests[-1], ensure_ascii=False)
            # Exercise cancellation across the container socket while the provider blocks.
            with socket.socket(socket.AF_UNIX) as client:
                client.settimeout(5)
                client.connect(str(sockets / 'agno.sock'))
                client.sendall(b'{"protocol":2,"operation":"run"}\n')
                with client.makefile('rb') as reader:
                    assert json.loads(reader.readline())['type'] == 'ready'
                    frame = dict(settings=dict(instructions='检查状态',builtin_tools=[],mcp_servers=[]),mcp_tokens={},user_id='cancel-user', session_id='cancel-session', message='block for cancellation',
                        session=None, memories=[], api_key='fixture-model-token', config=dict(protocol='completions',
                        endpoint=f'http://127.0.0.1:{model.server_port}/v1/chat/completions', model='test',
                        timeout_seconds=30, reasoning_summary=False))
                    client.sendall((json.dumps(frame)+'\n').encode())
                    assert blocking.wait(10), 'blocking model request did not start'
                    assert socket_health()['active_runs'] == 1
            for _ in range(100):
                if socket_health()['active_runs'] == 0:
                    break
                time.sleep(.05)
            assert socket_health()['active_runs'] == 0, 'cancelled container child still active'
            docker('stop', 'agno')
            code, health = api('GET', '/api/assistant/health')
            assert code == 200 and health['status'] == 'unavailable', health
            code, unavailable = api('POST', '/api/assistant/sessions/' + session_id + '/messages', dict(message='offline'))
            assert code == 503, unavailable
            code, saved = api('GET', '/api/assistant/sessions/' + session_id)
            assert saved['messages'] == followup['messages'], 'outage modified history'
            assert 'fixture-model-token' not in docker('logs', '--no-color', 'agno')
            assert 'fixture-mcp-token' not in docker('logs', '--no-color', 'agno')
            docker('start', 'agno')
            wait_ready()
            assert api('GET', '/api/assistant/health')[1]['status'] == 'ready'
            print('Agno container checks passed: non-root/read-only deployment, health, configured prompt/MCP allowlist and credentials, Go tool loop, persisted memory, restart, cancellation and outage recovery.')
        except Exception:
            print('Container test diagnostics: model request count =', len(requests))
            try:
                print('Runtime health:', socket_health())
            except Exception as error:
                print('Runtime health unavailable:', type(error).__name__)
            raise
        finally:
            release_model.set()
            if control is not None:
                control.terminate()
                try:
                    control.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    control.kill()
                    control.wait()
            log.close()
            docker('down', '--timeout', '10')
finally:
    release_model.set()
    model.shutdown()
    model.server_close()
