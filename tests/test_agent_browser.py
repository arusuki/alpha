"""Browser regression for reports using a local, deterministic API fixture."""
import json
import mimetypes
import os
import threading
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

## 总览与主要结论
当前记录中，主要空间集中在模型权重、数据集与训练产物。以下为测试报告。

## 重点容器与用户排行
| 容器 | 负责人 | 存储来源 | 容器内路径 | 实际占用 | 证据与建议 |
| --- | --- | --- | --- | --- | --- |
| research-worker | alice | bind mount | `/workspace/models` | **120 GiB** | 模型权重候选，需确认任务依赖 |
| training-worker | bob | 可写层 | `/root/.cache/pip` | 8 GiB | 可重建缓存候选，需确认重建成本 |

## 大数据资产与时间分布
- 90 天以内：模型权重候选。
- 时间未知：未补查的数据集，不推定为旧文件。

## 清理候选与长期治理
候选占用不等于可释放空间。迁出可写层前需要核实使用方和挂载配置。

## 覆盖范围、证据与待确认事项
只读取文件元数据，未验证进程占用或创建时间。

<img src=x onerror="window.reportInjected=true">
'''
session = dict(id=session_id, title='空间消耗总报告', status='running', model='test-model',
               provider='responses', snapshot_id=record_id, created_at=1789373000, updated_at=1789373000)
messages, writes, sessions = [], [], []
lock = threading.RLock()


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
                if after and session['status'] == 'running':
                    session['status'] = 'completed'
                    messages.append(dict(id=len(messages) + 1, role='assistant', content=report, created_at=1789373030))
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

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        with lock:
            writes.append((self.path, body))
            assert self.headers['X-CSRF-Token'] == 'test'
            if self.path == '/api/agent/reports':
                assert body == dict(snapshot_id=record_id, revision=7)
                sessions.append(session)
                messages.extend([
                    dict(id=1, role='user', content='生成空间消耗总报告'),
                    dict(id=2, role='status', content='已读取当前扫描记录与可写层排行'),
                    dict(id=3, role='tool_call', tool_name='get_directory', content='{"path":"/data"}'),
                    dict(id=4, role='tool_result', tool_name='get_directory', content='{"allocated":123456}')])
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
        page.wait_for_function('agentView.session?.status === "completed"')
        assert page.locator('#agentReports table').count() == 1
        assert page.locator('#agentReports img').count() == 0
        assert not page.evaluate('window.reportInjected || false')
        assert page.locator('#agentReports h3').filter(has_text='大数据资产').count() == 1
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
        page.screenshot(path='/tmp/project-alpha-agent-report-mobile.png', full_page=True)
        assert page.evaluate('document.querySelector("#agentDialog").scrollWidth <= document.querySelector("#agentDialog").clientWidth + 1')
        assert len([w for w in writes if w[0] == '/api/agent/reports']) == 1
        assert not errors, errors
        browser.close()
    print('Chromium Agent report passed: selected record, progress, safe Markdown tables, export, followup, reload recovery, mobile layout and no page errors.')
finally:
    server.shutdown()
