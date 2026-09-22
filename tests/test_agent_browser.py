"""Browser regression for reports using a local, deterministic API fixture."""
import json
import mimetypes
import os
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import parse_qs, urlparse
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
sample = json.loads((repo / 'tests/fixtures/snapshot.json').read_text())
record_id, session_id = 'b' * 32, 'a' * 32
sample.update(job_id=record_id, revision=7)
job = dict(id=record_id, status='completed', trigger='manual', created_by='admin',
           created_at=1789372800, finished_at=1789372900, snapshot_revision=7,
           allocated=sample['tree']['allocated'])
report = '''# 空间消耗总报告

9 个容器 · 3 组 · 仅列主要占用；同一物理路径已合并。

## 1. 可立即删除（无争议）

暂无明确条目。

## 2. 存在争议

### 下载与包缓存

| 目录 / 文件（物理路径） | 容器 / 内部路径 | 实际占用 | 文件用途与分类原因 |
| --- | --- | --- | --- |
| /private/training-worker/root/.cache/pip | training-worker：/root/.cache/pip | 8 GiB | Python 安装包缓存。尚未确认能否重新获取私有依赖。 |

## 3. 必须保留（无争议）

暂无明确条目。

## 4. 放错位置

### 模型权重

| 目录 / 文件（物理路径） | 容器 / 内部路径 | 实际占用 | 文件用途与分类原因 |
| --- | --- | --- | --- |
| /private/research-worker/models | research-worker：/models | **120 GiB** | 模型权重存于容器可写层。应迁入已配置的 /workspace/models 共享挂载。 |

<img src=x onerror="window.reportInjected=true">
'''

session = dict(id=session_id, title='空间消耗总报告', status='running', model='test-model',
               provider='responses', snapshot_id=record_id, created_at=1789373000, updated_at=1789373000)
messages, writes, sessions = [], [], []
lock = threading.RLock()
continue_stream = threading.Event()
event_connections = []
stream_stage = 0

def trace(role, data, tool_name=None):
    message = dict(id=len(messages)+1, role=role, content=json.dumps(data, ensure_ascii=False), created_at=1789373001+len(messages))
    if tool_name:
        message['tool_name'] = tool_name
    messages.append(message)


def request_data(request_id, round):
    return dict(request_id=request_id, round=round, model='test-model', protocol='responses',
                context_items=2, context_bytes=12345, request_bytes=14000,
                context=[dict(role='system', content='只根据实际扫描证据给出结论 <script>bad()</script>'),
                         dict(role='user', content='生成空间消耗总报告')], tools=[dict(type='function', name='get_directory')])


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
        parsed = urlparse(self.path)
        path = parsed.path
        if path == '/api/agent/sessions/' + session_id + '/events':
            return self.stream_events(parsed)
        with lock:
            if path == '/api/session':
                return self.respond(dict(user=dict(id='admin', username='admin', role='admin'), csrf='test'))
            if path == '/api/state':
                return self.respond(dict(jobs=[job], directory_jobs=[], latest_id=record_id, active=None, interval_minutes=0))
            if path.endswith('/snapshot'):
                return self.respond(sample)
            if path.endswith('/events'):
                self.send_response(204)
                self.end_headers()
                return
            if path.endswith('/changes'):
                return self.respond(dict(job_id=record_id, base_revision=7, revision=7,
                                         metadata={k: v for k, v in sample.items() if k != 'tree'}, replacements=[], ancestors=[]))
            if path == '/api/agent/settings':
                return self.respond(dict(revision=1, value=dict(model='test-model', endpoint='http://model.test', protocol='responses')))
            if path == '/api/agent/sessions':
                return self.respond(dict(sessions=sessions))
            if path == '/api/agent/sessions/' + session_id:
                after = int(parse_qs(parsed.query).get('after', ['0'])[0])
                batch = [m for m in messages if m['id'] > after]
                return self.respond(dict(session=session, messages=batch, next_after=messages[-1]['id'] if messages else after, has_more=False, active_job=None))
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

    def stream_events(self, parsed):
        global stream_stage
        after = int(parse_qs(parsed.query).get('after', ['0'])[0])
        event_connections.append(after)
        self.send_response(200)
        self.send_header('Content-Type', 'text/event-stream')
        self.end_headers()
        if stream_stage == 0:
            with lock:
                trace('model_delta', dict(request_id='request-one', deltas=[dict(kind='summary', text='先核对最大的目录。'), dict(kind='text', text='正在比较目录占用。'), dict(kind='tool', index=0, name='get_directory', arguments='{"path":')]))
                stream_stage = 1
        elif stream_stage == 1:
            if not continue_stream.wait(15):
                return
            with lock:
                trace('model_response', dict(request_id='request-one', status='completed', text='正在比较目录占用。', summary='先核对最大的目录。', duration_ms=1250,
                      usage=dict(input_tokens=1200, output_tokens=80, total_tokens=1280, cached_tokens=400, reasoning_tokens=30)))
                trace('tool_start', dict(call_id='tool-one', arguments='{"path":"/data","limit":20}'), 'get_directory')
                trace('tool_end', dict(call_id='tool-one', status='completed', result='{"allocated":123456}', duration_ms=40), 'get_directory')
                trace('model_request', request_data('request-two', 2))
                trace('model_delta', dict(request_id='request-two', deltas=[dict(kind='text', text=report[:50])]))
                trace('model_response', dict(request_id='request-two', status='completed', text=report, summary='', duration_ms=350,
                      usage=dict(input_tokens=1600, output_tokens=400, total_tokens=2000, cached_tokens=800, reasoning_tokens=50)))
                messages.append(dict(id=len(messages)+1, role='group_report', content='### 第 3/3 组 · 1 个容器\n\n本组主要占用已分类。', created_at=1789373029))
                messages.append(dict(id=len(messages)+1, role='assistant', content=report, created_at=1789373030))
                session['status'] = 'completed'
                stream_stage = 2
        with lock:
            batch = [m for m in messages if m['id'] > after]
            update = dict(session=session, messages=batch, next_after=messages[-1]['id'], has_more=False, active_job=None)
            raw = json.dumps(update)
        try:
            self.wfile.write(('event: session\nid: ' + str(update['next_after']) + '\ndata: ' + raw + '\n\n').encode())
            self.wfile.flush()
        except (BrokenPipeError, ConnectionResetError):
            pass

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with lock:
            writes.append((self.path, body))
            assert self.headers['X-CSRF-Token'] == 'test'
            if self.path == '/api/agent/reports':
                assert body == dict(snapshot_id=record_id, revision=7)
                sessions.append(session)
                messages.append(dict(id=1, role='user', content='生成空间消耗总报告'))
                messages.append(dict(id=2, role='status', content='第 1/3 组 · 4 个容器：正在核对主要占用'))
                trace('model_request', request_data('request-one', 1))
                return self.respond(session, 202)
            if self.path.endswith('/messages'):
                messages.extend([dict(id=len(messages)+1, role='user', content=body['message']),
                                 dict(id=len(messages)+2, role='assistant', content='## 继续排查结果\n已核对缓存候选。')])
                return self.respond(session, 202)
            if self.path.endswith('/cancel'):
                session['status'] = 'cancelled'
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
        page = browser.new_page(viewport=dict(width=1440, height=1080))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto('http://127.0.0.1:' + str(server.server_port))
        page.locator('.platform-nav [data-page=overview]').click()
        page.wait_for_function('platform.loaded !== null')
        page.locator('#generateReport').click()
        page.wait_for_function('document.querySelector("#agentActivityLog").textContent.includes("正在比较目录占用")')
        assert page.evaluate('agentView.session.status') == 'running'
        assert page.locator('#agentReports').is_hidden()
        assert page.locator('#agentActivityLog').inner_text().count('正在比较目录占用') == 1
        assert '先核对最大的目录' in page.locator('#agentActivityLog').inner_text()
        assert '等待接口统计' in page.locator('#agentMetrics').inner_text()
        page.locator('.agent-context > summary').first.click()
        page.locator('.agent-context details > summary').first.click()
        assert '只根据实际扫描证据' in page.locator('.agent-context').first.inner_text()
        assert page.locator('#agentActivityLog script').count() == 0
        page.screenshot(path='/tmp/project-alpha-agent-streaming-desktop.png', full_page=True)
        continue_stream.set()
        page.wait_for_function('agentView.session?.status === "completed"')
        assert len(event_connections) >= 2, event_connections
        assert event_connections[1] > event_connections[0]
        assert page.locator('.agent-context').first.get_attribute('open') is not None
        assert '3,280 token' in page.locator('#agentMetrics').inner_text()
        assert '已统计 2/2' in page.locator('#agentMetrics').inner_text()
        assert page.locator('.agent-model-step').count() == 2
        assert page.locator('.agent-tool-step').count() == 1
        assert '40 ms' in page.locator('.agent-tool-step').inner_text()
        group = page.locator('#agentActivityLog > details').filter(has_text='分组结果')
        assert group.count() == 1
        assert group.get_attribute('open') is None
        group.locator('summary').click()
        assert '本组主要占用已分类' in group.inner_text()
        page.locator('#agentReportTab').click()
        assert page.locator('#agentReports table').count() == 2
        assert page.locator('#agentReports img').count() == 0
        assert not page.evaluate('window.reportInjected || false')
        for category in ['可立即删除', '存在争议', '必须保留', '放错位置']:
            assert page.locator('#agentReports h3').filter(has_text=category).count() == 1
        assert '本组主要占用已分类' not in page.locator('#agentReports').inner_text()
        assert page.locator('#stopAgent').is_hidden()
        assert not page.locator('#sendAgentQuestion').is_disabled()
        page.screenshot(path='/tmp/project-alpha-agent-report-desktop.png', full_page=True)
        with page.expect_download() as info:
            page.locator('#downloadReport').click()
        download = info.value
        assert download.suggested_filename.endswith('.md')
        assert report in Path(download.path()).read_text()
        page.locator('[data-agent-question]').first.click()
        assert page.locator('#agentQuestion').input_value()
        page.locator('#sendAgentQuestion').click()
        page.wait_for_function('document.querySelector("#agentReports").textContent.includes("已核对缓存候选")')
        page.locator('#closeAgent').click()
        page.locator('#viewReports').click()
        assert page.locator('#agentReports .agent-report').count() == 2
        page.reload()
        page.locator('.platform-nav [data-page=overview]').click()
        page.wait_for_function('platform.loaded !== null')
        page.locator('#viewReports').click()
        page.wait_for_function('document.querySelectorAll(".agent-report").length === 2')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.locator('.agent-model-step').count() == 2
        assert '3,280 token' in page.locator('#agentMetrics').inner_text()
        page.screenshot(path='/tmp/project-alpha-agent-conversation-mobile.png', full_page=True)
        page.locator('#agentReportTab').click()
        page.screenshot(path='/tmp/project-alpha-agent-report-mobile.png', full_page=True)
        assert page.evaluate('document.querySelector("#agentDialog").scrollWidth <= document.querySelector("#agentDialog").clientWidth + 1')
        assert len([w for w in writes if w[0] == '/api/agent/reports']) == 1
        assert not errors, errors
        browser.close()
    print('Chromium Agent report passed: selected record, incremental SSE, reconnect cursors, context inspection, token usage, safe Markdown tables, export, followup, reload recovery, mobile layout and no page errors.')
finally:
    server.shutdown()
