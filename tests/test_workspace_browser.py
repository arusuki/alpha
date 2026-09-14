"""Workspace navigation and process regressions against deterministic local APIs."""
import json
import mimetypes
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
sample = json.loads((repo / 'tests/fixtures/snapshot.json').read_text())
record = 'b' * 32
sample.update(job_id=record, revision=7)
job = dict(id=record, status='completed', trigger='manual', created_by='admin',
           created_at=1789372800, finished_at=1789372900, snapshot_revision=7,
           allocated=sample['tree']['allocated'])
user = None
calls, writes = [], []
unavailable = False
has_records = True
model = dict(revision=1, value=dict(protocol='responses', endpoint='http://model.test/v1',
             model='example-model', max_rounds=12, timeout_seconds=180, has_api_key=True))


def process(pid, binary, children=None, command=None):
    return dict(exec_id='exec-' + str(pid), pid=pid, uid=1000, binary=binary,
                command=command or binary, cwd='/workspace', started_at='2026-09-14T08:00:00Z',
                children=children or [])


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def respond(self, value, status=200):
        raw = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_GET(self):
        path = urlparse(self.path).path
        calls.append(self.path)
        if path == '/api/session':
            return self.respond(dict(user=user, csrf='test', setup_required=False))
        if path == '/api/state':
            return self.respond(dict(jobs=[job] if has_records else [], directory_jobs=[], latest_id=record if has_records else None, active=None, interval_minutes=30))
        if path.endswith('/snapshot'):
            return self.respond(sample)
        if path.endswith('/changes'):
            return self.respond(dict(job_id=record, base_revision=7, revision=7,
                metadata={k: v for k, v in sample.items() if k != 'tree'}, replacements=[], ancestors=[]))
        if path.endswith('/events'):
            self.send_response(204)
            self.end_headers()
            return
        if path == '/api/process/forest':
            if unavailable:
                return self.respond(dict(error='未启用容器进程监控'), 503)
            forest = dict(captured_at='2026-09-14T08:30:00Z', containers=[
                dict(id='training-container', process_count=3, roots=[process(101, '/bin/bash', [
                    process(102, '/usr/bin/python', command='python train.py --data /workspace'),
                    process(103, '/usr/bin/worker', command='<img src=x onerror=alert(1)>')])]),
                dict(id='web-container', process_count=1, roots=[process(201, '/usr/bin/nginx')])])
            if 'host=1' in self.path:
                forest['host'] = dict(id='', process_count=1, roots=[process(1, '/sbin/init')])
            return self.respond(dict(status=dict(connected=True, bootstrapped=True), forest=forest))
        if path == '/api/agent/settings':
            return self.respond(model)
        if path == '/api/settings':
            return self.respond(dict(revision=1, value=dict(root=['/srv'], exclude=[], scan_backend='auto',
                scan_mode='normal', no_docker=False, include_docker_root=False, max_depth=5, max_nodes=50000,
                docker_timeout=120, owner_label='project-alpha.owner', interval_minutes=30)))
        if path == '/api/agent/sessions':
            return self.respond(dict(sessions=[]))
        if path == '/api/users':
            return self.respond(dict(users=[dict(user, enabled=True)]))
        if path == '/api/audit':
            return self.respond(dict(events=[]))
        file = repo / 'dist' / ('index.html' if path == '/' else path.lstrip('/'))
        if not file.is_file():
            self.send_error(404)
            return
        raw = file.read_bytes()
        self.send_response(200)
        self.send_header('Content-Type', mimetypes.guess_type(str(file))[0] or 'text/plain')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def do_POST(self):
        global user
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path == '/api/login':
            user = dict(id='user', username=body['username'], role='viewer' if body['username'] == 'reader' else 'admin')
            return self.respond(dict(user=user, csrf='test'))
        if self.path == '/api/logout':
            user = None
            return self.respond(dict(ok=True))
        self.send_error(404)

    def do_PUT(self):
        global model
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        writes.append((self.path, body))
        value = body['value']
        value['has_api_key'] = bool(value.pop('api_key')) or model['value']['has_api_key']
        if value.pop('clear_api_key'):
            value['has_api_key'] = False
        model = dict(revision=model['revision'] + 1, value=value)
        return self.respond(model)


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        page = browser.new_page(viewport=dict(width=1440, height=1080))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto('http://127.0.0.1:' + str(server.server_port))
        page.locator('#authUsername').fill('admin')
        page.locator('#authPassword').fill('test-password')
        page.screenshot(path='/tmp/project-alpha-login.png', full_page=True, animations='disabled')
        page.locator('#authSubmit').click()
        page.wait_for_function('platform.latest !== null')
        assert page.locator('#page-dashboard').is_visible()
        assert not any('/snapshot' in path for path in calls if path.startswith('/api/'))
        assert page.locator('#resultLoadingDialog').is_hidden()
        page.wait_for_function('getComputedStyle(document.querySelector(".workspace-art")).opacity === "1"')
        page.screenshot(path='/tmp/project-alpha-dashboard.png', full_page=True, animations='disabled')
        page.locator('.recent-row').focus()
        page.evaluate('syncState()')
        assert page.locator('.recent-row').evaluate('(e) => e === document.activeElement')
        page.locator('.module-agent').click()
        page.wait_for_function('settingsState.model !== null')
        assert page.locator('#storageNav').is_hidden()
        assert page.locator('#storageMonitor').is_hidden()
        assert page.locator('#agentSettingsForm').is_visible()
        page.locator('#agentModel').fill('updated-model')
        page.locator('#agentSaveSettings').click()
        page.wait_for_function('settingsState.model?.revision === 2')
        assert page.locator('#agentKey').input_value() == ''
        page.screenshot(path='/tmp/project-alpha-agent-settings.png', full_page=True, animations='disabled')
        page.locator('.platform-nav [data-page=processes]').click()
        page.wait_for_function('processView.data !== null')
        assert page.locator('#processTotalCount').inner_text() == '4'
        assert page.locator('#processTree tr').count() == 3
        assert page.locator('#processTree img').count() == 0
        page.locator('[data-process-toggle=exec-101]').focus()
        page.evaluate('loadProcesses()')
        assert page.locator('[data-process-toggle=exec-101]').evaluate('(e) => e === document.activeElement')
        page.locator('[data-process-toggle=exec-101]').click()
        assert page.locator('#processTree tr').count() == 1
        page.locator('#processSearch').fill('train.py')
        assert page.locator('#processTree tr').count() == 2
        page.locator('#processSearch').fill('no-matching-process')
        assert page.locator('#processEmptyTitle').inner_text() == '没有匹配的进程'
        page.locator('#processSearch').fill('')
        page.locator('#processExpand').click()
        page.locator('#processHost').check()
        page.wait_for_function('processView.data?.forest.host !== undefined')
        assert page.locator('#processTotalCount').inner_text() == '5'
        page.locator('[data-process-group="@host"]').click()
        assert page.locator('#processTreeTitle').inner_text() == '宿主机'
        page.locator('[data-process-group="training-container"]').click()
        page.screenshot(path='/tmp/project-alpha-processes.png', full_page=True, animations='disabled')
        unavailable = True
        page.locator('#processRefresh').click()
        page.wait_for_function('processView.error !== ""')
        assert '上次采集' in page.locator('#processTreeHint').inner_text()
        page.locator('.platform-nav [data-page=dashboard]').click()
        assert page.evaluate('processView.controller === null')
        count = len([c for c in calls if '/api/process/' in c])
        page.wait_for_timeout(3200)
        assert len([c for c in calls if '/api/process/' in c]) == count
        page.go_back()
        assert page.locator('#page-processes').is_visible()
        unavailable = False
        page.locator('#processRefresh').click()
        page.wait_for_function('processView.error === ""')
        page.locator('.platform-nav [data-page=overview]').click()
        page.wait_for_function('platform.loaded !== null')
        assert page.locator('#storageNav').is_visible()
        assert page.locator('#storageNav [data-page=overview]').get_attribute('aria-current') == 'page'
        page.locator('#storageNav [data-page=history]').click()
        assert page.locator('#page-history').is_visible()
        assert page.locator('.platform-nav [data-page=overview]').get_attribute('aria-current') == 'page'
        page.locator('#storageNav [data-page=overview]').click()
        page.screenshot(path='/tmp/project-alpha-storage.png', full_page=True, animations='disabled')
        page.locator('#viewReports').click()
        page.locator('#agentModelSettings').click()
        assert page.locator('#page-agent-settings').is_visible()
        assert page.locator('#agentDialog').is_hidden()
        page.reload()
        page.wait_for_function('platform.user !== null')
        assert page.locator('#page-dashboard').is_visible()
        page.emulate_media(reduced_motion='reduce')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.locator('#page-dashboard').evaluate('(e) => getComputedStyle(e).animationName') == 'none'
        for module in ['dashboard', 'overview', 'processes', 'agent-settings', 'settings']:
            page.locator('.platform-nav [data-page=' + module + ']').click()
            if module == 'overview':
                page.wait_for_function('platform.loaded !== null')
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), module
            page.screenshot(path='/tmp/project-alpha-' + module + '-mobile.png', full_page=True, animations='disabled')
        page.locator('.platform-nav [data-page=overview]').click()
        page.locator('#storageNav [data-page=scan-settings]').click()
        page.wait_for_function('platform.config !== null')
        assert page.locator('#cfgRoots').input_value() == '/srv'
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('#logoutButton').click()
        page.locator('#authUsername').fill('reader')
        page.locator('#authPassword').fill('test-password')
        page.locator('#authSubmit').click()
        page.wait_for_function('platform.user?.role === "viewer"')
        assert page.locator('#page-dashboard').is_visible()
        assert page.locator('.module-agent').is_hidden()
        page.evaluate('showPage("agent-settings")')
        assert page.locator('#page-agent-settings').is_hidden()
        unavailable = True
        page.locator('.module-process').click()
        page.wait_for_function('processView.error !== ""')
        assert page.locator('#processEmptyTitle').inner_text() == '暂时无法读取进程'
        assert page.locator('#processTotalCount').inner_text() == '—'
        unavailable = False
        page.locator('#processRetry').click()
        page.wait_for_function('processView.data !== null')
        assert page.locator('#processWorkspace').is_visible()
        has_records = False
        page.locator('#logoutButton').click()
        page.locator('#authUsername').fill('admin')
        page.locator('#authPassword').fill('test-password')
        page.locator('#authSubmit').click()
        page.wait_for_function('platform.user !== null')
        assert page.locator('#dashboardRecent').inner_text().startswith('还没有扫描记录')
        assert page.locator('#dashboardAllocated').inner_text() == '—'
        page.locator('.module-storage').click()
        assert page.locator('#firstScan').is_visible()
        assert not errors, errors
        browser.close()
    print('Workspace browser checks passed: login dashboard, lazy snapshots, module navigation/history, Agent settings, process search/tree/host/errors/polling, permissions and responsive layouts.')
finally:
    server.shutdown()
