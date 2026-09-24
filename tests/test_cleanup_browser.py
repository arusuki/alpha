"""Report extraction, selection and explicit deletion through the storage subpage."""
import json
import mimetypes
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
user = dict(id='admin', username='admin', role='admin')
session = dict(id='a' * 32, status='running', snapshot_id='b' * 32, error='')
host_session = dict(id='h' * 32, status='completed', snapshot_id='b' * 32, error='', report_scope='host')
host_entries = [dict(id='8' * 32, path='/srv/host-cache', category=1, summary='Host 缓存',
                     detail=json.dumps(dict(bytes=8192, locations=['Host：/srv'], kind='缓存', summary='缓存目录', reason='可重建')),
                     status='pending', error='')]
entries = [dict(id=str(i) * 32, path=f'/data/{name}', category=i,
                summary='用途 <script>unsafe()</script>；处理前请核对依赖',
                detail=json.dumps(dict(bytes=4096 * i, locations=['容器：/workspace/' + name],
                                       kind='其他', summary='原始用途', reason='原始处理条件')),
                status='pending', error='')
           for i, name in enumerate(['cache', 'uncertain', 'keep', 'misplaced'], 1)]
extracted = False
host_extracted = False
reads = 0
deleting = False
delete_attempts = 0
history_deletes = 0
writes = []
trace_release = threading.Event()
long_output = 'x' * 40000
trace_tail = long_output[-32000:]
trace_messages = [
    dict(id=1, role='model_request', content=json.dumps(dict(request_id='extract-1', round=1, protocol='responses')), created_at=1789372800),
    dict(id=2, role='model_delta', content=json.dumps(dict(request_id='extract-1', deltas=[dict(kind='summary', text='先核对四类条目 <script>unsafe()</script>'), dict(kind='text', text='{"entries":[')])), created_at=1789372801),
    dict(id=3, role='model_delta', content=json.dumps(dict(request_id='extract-1', deltas=[dict(kind='summary_snapshot', text='先核对四类条目 <script>unsafe()</script>；再验证路径'), dict(kind='text_snapshot', text=trace_tail, dropped_bytes=8000)])), created_at=1789372802),
    dict(id=4, role='model_response', content=json.dumps(dict(request_id='extract-1', status='completed', text=trace_tail, text_dropped_bytes=8000, summary='先核对四类条目；再验证路径')), created_at=1789372803),
    dict(id=5, role='assistant', content='已从完整报告提取 4 个条目。', created_at=1789372804),
]


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
        global reads
        path = urlparse(self.path).path
        if path == '/api/session':
            return self.respond(dict(user=user, csrf='test', setup_required=False))
        if path == '/api/state':
            return self.respond(dict(jobs=[], directory_jobs=[], latest_id=None, active=None, interval_minutes=0))
        if path == '/api/agent/cleanup-reports':
            return self.respond(dict(reports=[dict(report_id=2, title='Host 空间报告', report_scope='host',
                snapshot_id=host_session['snapshot_id'], created_at=1789372801,
                cleanup_id=host_session['id'] if host_extracted else None, cleanup_status=host_session['status'], phase='extract'),
                dict(report_id=1, title='空间消耗总报告', report_scope='container',
                snapshot_id=session['snapshot_id'], created_at=1789372800,
                cleanup_id=session['id'] if extracted else None, cleanup_status=session['status'], phase='delete' if deleting else 'extract')]))
        if path == '/api/agent/cleanups/' + host_session['id']:
            return self.respond(dict(session=host_session, report_id=2, phase='extract', entries=host_entries))
        if path == '/api/agent/sessions/' + host_session['id']:
            return self.respond(dict(session=host_session, messages=[], next_after=0, has_more=False, active_job=None))
        if path == '/api/agent/cleanups/' + session['id']:
            reads += 1
            if (deleting and reads >= 2) or (not deleting and trace_release.is_set()):
                session['status'] = 'completed'
                if deleting:
                    entries[0]['status'] = 'deleted'
                    entries[0]['error'] = '已清理；保留 socket 1 个、字符设备 2 个及其所在目录'
            return self.respond(dict(session=session, report_id=1, phase='delete' if deleting else 'extract',
                                     entries=entries if session['status'] == 'completed' else []))
        if path == '/api/agent/sessions/' + session['id']:
            after = int(dict(x.split('=', 1) for x in urlparse(self.path).query.split('&') if '=' in x).get('after', '0'))
            available = trace_messages if trace_release.is_set() else trace_messages[:1]
            messages = [item for item in available if item['id'] > after]
            return self.respond(dict(session=session, messages=messages, next_after=messages[-1]['id'] if messages else after, has_more=False, active_job=None))
        if path == '/api/agent/sessions/' + session['id'] + '/events':
            after = int(dict(x.split('=', 1) for x in urlparse(self.path).query.split('&') if '=' in x).get('after', '0'))
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.end_headers()
            try:
                first = [item for item in trace_messages[:2] if item['id'] > after]
                update = dict(session=session.copy(), messages=first, next_after=2, has_more=False, active_job=None)
                self.wfile.write(('id: 2\nevent: session\ndata: ' + json.dumps(update) + '\n\n').encode())
                self.wfile.flush()
                if not trace_release.wait(10):
                    return
                session['status'] = 'completed'
                update = dict(session=session.copy(), messages=trace_messages[2:], next_after=5, has_more=False, active_job=None)
                self.wfile.write(('id: 5\nevent: session\ndata: ' + json.dumps(update) + '\n\n').encode())
                self.wfile.flush()
            except (BrokenPipeError, ConnectionResetError):
                pass
            return
        if path == '/api/agent/sessions':
            return self.respond(dict(sessions=[]))
        target = repo / 'dist' / ('index.html' if path == '/' else path.lstrip('/'))
        if target.is_file() and str(target.resolve()).startswith(str(repo / 'dist') + '/'):
            data = target.read_bytes()
            self.send_response(200)
            self.send_header('Content-Type', mimetypes.guess_type(str(target))[0] or 'application/octet-stream')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            return
        self.send_error(404)

    def do_POST(self):
        global extracted, host_extracted, user, deleting, reads, delete_attempts
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        password = body.pop('sudo_password', None)
        writes.append((self.path, body))
        assert self.headers.get('X-CSRF-Token') == 'test'
        if self.path == '/api/agent/cleanups':
            if body == dict(report_id=2):
                host_extracted = True
                return self.respond(host_session, 202)
            assert body == dict(report_id=1)
            extracted = True
            return self.respond(session, 202)
        if self.path.endswith('/delete'):
            assert password == 'test-only-browser-sudo'
            password = None
            delete_attempts += 1
            if delete_attempts == 1:
                return self.respond(dict(error='测试服务暂时不可用，请重新输入密码后再提交'), 503)
            assert body == dict(entry_ids=['1' * 32])
            entries[0]['status'] = 'deleting'
            session['status'] = 'running'
            deleting = True
            reads = 0
            return self.respond(dict(session=session, report_id=1, phase='delete', entries=entries), 202)
        if self.path == '/api/logout':
            user = None
            return self.respond(dict(ok=True))
        self.send_error(404)

    def do_DELETE(self):
        global extracted, history_deletes
        assert self.headers.get('X-CSRF-Token') == 'test'
        if self.path == '/api/agent/cleanups/' + session['id'] and session['status'] == 'completed':
            history_deletes += 1
            extracted = False
            return self.respond(dict(ok=True))
        self.send_error(404)


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        page = browser.new_page(viewport=dict(width=1440, height=1000))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        url = 'http://127.0.0.1:' + str(server.server_port)
        page.goto(url)
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('!document.getElementById("cleanupExtract").disabled')
        assert page.locator('#pageTitle').inner_text() == '诊断清理'
        page.locator('#cleanupExtract').click()
        page.wait_for_function('document.getElementById("cleanupAgentLog").textContent.includes("先核对四类条目")')
        assert page.locator('#cleanupAgentPanel').is_visible()
        assert page.locator('#cleanupAgentLog script').count() == 0
        assert '{"entries":[' in page.locator('#cleanupAgentLog').inner_text()
        page.locator('#cleanupAgentLog details summary').first.click()
        assert page.locator('#cleanupAgentLog details').first.evaluate('(element) => element.open')
        trace_release.set()
        page.wait_for_function('document.querySelectorAll("[data-cleanup-entry]").length === 4')
        assert page.locator('#cleanupEntries > .cleanup-group').count() == 4
        assert page.locator('#cleanupEntries > .cleanup-group table').count() == 4
        page.evaluate('''() => {
            window.cleanupOriginalEntries = cleanupView.entries;
            const first = cleanupView.entries[0], second = cleanupView.entries[1];
            cleanupView.entries = [
                ...cleanupView.entries,
                {...first, id: '5'.repeat(32), path: '/data/larger', detail: {...first.detail, bytes: 16384}},
                {...first, id: '6'.repeat(32), path: '/data/unknown', detail: {...first.detail, bytes: null}},
                {...second, id: '7'.repeat(32), path: '/data/smaller', detail: {...second.detail, bytes: 2048}},
            ];
            renderCleanup();
        }''')
        first_group = page.locator('.cleanup-group[data-cleanup-category="1"]')
        second_group = page.locator('.cleanup-group[data-cleanup-category="2"]')
        paths = lambda group: group.locator('tbody .cleanup-path').all_inner_texts()
        assert paths(first_group) == ['/data/larger', '/data/cache', '/data/unknown']
        assert paths(second_group) == ['/data/uncertain', '/data/smaller']
        first_group.locator('[data-cleanup-sort]').click()
        assert paths(first_group) == ['/data/cache', '/data/larger', '/data/unknown']
        assert first_group.locator('th[aria-sort="ascending"]').count() == 1
        assert paths(second_group) == ['/data/uncertain', '/data/smaller']
        first_group.locator('[data-cleanup-sort]').click()
        assert paths(first_group) == ['/data/larger', '/data/cache', '/data/unknown']
        first_group.locator('summary').first.click()
        page.wait_for_function('cleanupView.collapsed.has(1)')
        assert not first_group.evaluate('(element) => element.open')
        second_group.locator('[data-cleanup-sort]').click()
        assert not first_group.evaluate('(element) => element.open')
        assert paths(second_group) == ['/data/smaller', '/data/uncertain']
        page.locator('#cleanupSearch').fill('data')
        assert not first_group.evaluate('(element) => element.open')
        first_group.locator('summary').first.click()
        page.wait_for_function('!cleanupView.collapsed.has(1)')
        page.locator('#cleanupSearch').fill('')
        page.evaluate('cleanupView.entries = window.cleanupOriginalEntries; renderCleanup()')
        page.wait_for_function('document.getElementById("cleanupAgentLog").textContent.includes("再验证路径")')
        assert page.locator('#cleanupAgentLog details').first.evaluate('(element) => element.open')
        assert page.evaluate('cleanupView.requests.get("extract-1").text.length <= cleanupTraceWindowChars')
        assert '较早的模型输出已截断' in page.locator('.cleanup-agent-output .form-note').first.inner_text()
        assert page.locator('.cleanup-agent-output pre').first.evaluate('(element) => element.scrollTop > 0')
        assert page.locator('#cleanupAgentLog').inner_text().count('已从完整报告提取 4 个条目') == 1
        assert page.locator('[data-cleanup-entry]:checked').count() == 0
        assert page.locator('#cleanupDelete').is_disabled()
        assert page.locator('#cleanupEntries script').count() == 0
        assert '<script>' in page.locator('#cleanupEntries').inner_text()
        page.locator('#cleanupCategory').select_option('3')
        assert page.locator('[data-cleanup-entry]').count() == 1
        assert '/data/keep' in page.locator('#cleanupEntries').inner_text()
        page.locator('#cleanupSelectAll').check()
        page.locator('#cleanupCategory').select_option('')
        assert page.locator('[data-cleanup-entry]:checked').count() == 1
        page.locator('[data-cleanup-entry="' + '3' * 32 + '"]').uncheck()
        page.locator('#cleanupSearch').fill('cache')
        page.locator('#cleanupSelectAll').check()
        page.locator('#cleanupDelete').click()
        assert page.locator('#cleanupDeleteDialog').is_visible()
        assert '/data/cache' in page.locator('#cleanupDeletePaths').inner_text()
        assert not any(path.endswith('/delete') for path, _ in writes)
        page.locator('#cleanupSudoPassword').fill('test-only-browser-sudo')
        page.locator('#cleanupDeleteClose').click()
        assert page.locator('#cleanupSudoPassword').input_value() == ''
        assert not any(path.endswith('/delete') for path, _ in writes)
        page.locator('#cleanupDelete').click()
        assert page.locator('#cleanupSudoPassword').get_attribute('type') == 'password'
        assert page.locator('#cleanupSudoPassword').get_attribute('autocomplete') == 'off'
        page.locator('#cleanupDeleteConfirm').click()
        assert not any(path.endswith('/delete') for path, _ in writes)
        page.locator('#cleanupSudoPassword').fill('test-only-browser-sudo')
        page.locator('#cleanupDeleteConfirm').click()
        page.wait_for_function('document.getElementById("cleanupDeleteError").textContent.length > 0')
        assert page.locator('#cleanupSudoPassword').input_value() == ''
        assert page.locator('#cleanupDeleteDialog').is_visible()
        page.locator('#cleanupSudoPassword').fill('test-only-browser-sudo')
        page.locator('#cleanupSudoPassword').press('Enter')
        page.wait_for_function('!document.getElementById("cleanupDeleteDialog").open')
        page.wait_for_function("cleanupView.session.status === 'completed' && cleanupView.entries[0].status === 'deleted'")
        assert page.locator('[data-cleanup-entry]').is_disabled()
        assert '已清理' in page.locator('#cleanupEntries').inner_text()
        assert '已清理 1 项' in page.locator('#cleanupStatus').inner_text()
        assert '保留 socket 1 个、字符设备 2 个及其所在目录' in page.locator('#cleanupEntries').inner_text()
        assert page.locator('#cleanupEntries .error-text').count() == 0
        page.locator('#cleanupSearch').fill('')
        assert page.locator('[data-cleanup-entry]:disabled').count() == 1
        page.reload()
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('document.querySelectorAll("[data-cleanup-entry]").length === 4')
        page.wait_for_function('document.getElementById("cleanupAgentLog").textContent.includes("再验证路径")')
        assert page.evaluate('cleanupView.requests.get("extract-1").text.length <= cleanupTraceWindowChars')
        assert page.locator('[data-cleanup-entry]:disabled').count() == 1
        assert page.locator('#cleanupExtract').is_disabled()
        page.screenshot(path='/tmp/project-alpha-cleanup.png', full_page=True, animations='disabled')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
        page.screenshot(path='/tmp/project-alpha-cleanup-mobile.png', full_page=True, animations='disabled')
        page.locator('[data-cleanup-entry="' + '2' * 32 + '"]').check()
        page.locator('#cleanupDelete').click()
        page.locator('#cleanupSudoPassword').fill('test-only-browser-sudo')
        assert page.evaluate("!JSON.stringify(localStorage).includes('test-only-browser-sudo') && !JSON.stringify(sessionStorage).includes('test-only-browser-sudo')")
        page.evaluate('showAuth(false)')
        assert page.locator('#cleanupSudoPassword').input_value() == ''
        assert page.locator('#cleanupEntries').inner_text() == '选择完整报告后提取条目。'
        page.reload()
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('document.querySelectorAll("[data-cleanup-entry]").length === 4')
        page.locator('#cleanupRemoveHistory').click()
        assert page.locator('#cleanupHistoryDialog').is_visible()
        assert '已经删除的磁盘内容不会恢复' in page.locator('#cleanupHistoryDialog').inner_text()
        page.locator('#cleanupHistoryClose').click()
        assert history_deletes == 0
        page.locator('#cleanupRemoveHistory').click()
        page.locator('#cleanupHistoryConfirm').click()
        page.wait_for_function('cleanupView.session === null')
        assert history_deletes == 1
        assert page.locator('#cleanupAgentPanel').is_hidden()
        assert page.locator('#cleanupRemoveHistory').is_hidden()
        assert page.locator('#cleanupExtract').is_enabled()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.reload()
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('!document.getElementById("cleanupExtract").disabled')
        assert page.locator('#cleanupRemoveHistory').is_hidden()
        assert page.locator('#cleanupReport option').count() == 1
        assert page.locator('#cleanupHostReport option').count() == 1
        page.locator('#cleanupHostExtract').click()
        page.wait_for_function('cleanupView.reportScope === "host"')
        assert 'Host' in page.locator('#cleanupWorkspaceTitle').inner_text()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.locator('#cleanupHostExtract').click()
        page.wait_for_function('document.querySelector("[data-cleanup-entry]")?.getAttribute("data-cleanup-entry") === "' + '8' * 32 + '"')
        assert ('/api/agent/cleanups', dict(report_id=2)) in writes
        assert page.locator('#cleanupHostExtract').is_disabled()
        page.locator('#cleanupExtract').click()
        page.wait_for_function('cleanupView.reportScope === "container"')
        assert '容器 Agent' in page.locator('#cleanupWorkspaceTitle').inner_text()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.locator('#cleanupHostExtract').click()
        page.wait_for_function('document.querySelector("[data-cleanup-entry]")?.getAttribute("data-cleanup-entry") === "' + '8' * 32 + '"')
        # Both scopes now have one completed extraction: switch using real clicks.
        page.locator('#cleanupExtract').click()
        page.wait_for_function('cleanupView.reportScope === "container" && !cleanupView.loading')
        page.locator('#cleanupExtract').click()
        page.wait_for_function('cleanupView.session?.status === "completed" && !cleanupView.posting')
        extraction_count = sum(url == '/api/agent/cleanups' for url, _ in writes)
        page.locator('#cleanupHostExtract').click()
        page.wait_for_function('cleanupView.reportScope === "host" && !cleanupView.loading')
        page.locator('#cleanupExtract').click()
        page.wait_for_function('cleanupView.reportScope === "container" && !cleanupView.loading')
        assert sum(url == '/api/agent/cleanups' for url, _ in writes) == extraction_count
        assert not errors, errors
        browser.close()
        print('Cleanup browser checks passed: extraction, streaming, selection, filesystem deletion, history deletion, reload and mobile.')
finally:
    server.shutdown()
    server.server_close()
