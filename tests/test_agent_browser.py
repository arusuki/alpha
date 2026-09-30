"""Browser regression for reports using a local, deterministic API fixture."""
import json
import threading
import time
from pathlib import Path
from urllib.parse import parse_qs, urlparse
from browser_support import NodeHandler, NODE_PATH, launch_options, start_server
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

session = dict(id=session_id, title='空间消耗总报告', report_scope='container', status='running', model='test-model',
               provider='responses', snapshot_id=record_id, created_at=1789373000, updated_at=1789373000)
messages, writes, sessions = [], [], []
lock = threading.RLock()
continue_stream = threading.Event()
event_connections = []
stream_stage = 0

def trace(role, data, tool_name=None, group_id=''):
    if role in ('model_request', 'model_delta', 'model_response'):
        data['group_id'] = {'request-one': 'group-1', 'request-middle': 'group-2', 'request-two': 'group-3'}[data['request_id']]
    elif role in ('tool_start', 'tool_end'):
        data['group_id'] = group_id
    message = dict(id=len(messages)+1, role=role, content=json.dumps(data, ensure_ascii=False), created_at=1789373001+len(messages))
    if tool_name:
        message['tool_name'] = tool_name
    messages.append(message)


def request_data(request_id, round):
    return dict(request_id=request_id, round=round, model='test-model', protocol='responses',
                context_items=2, context_bytes=12345, request_bytes=14000,
                context=[dict(role='system', content='只根据实际扫描证据给出结论 <script>bad()</script>'),
                         dict(role='user', content='生成空间消耗总报告')], tools=[dict(type='function', name='get_directory')])


class Handler(NodeHandler):
    def do_GET(self):
        if self.control_request():
            return
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
        self.serve_asset()

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
                trace('model_delta', dict(request_id='request-middle', deltas=[dict(kind='text', text='第二组正在核对缓存。')]))
                stream_stage = 1
        elif stream_stage == 1:
            if not continue_stream.wait(15):
                return
            with lock:
                trace('model_response', dict(request_id='request-one', status='completed', text='正在比较目录占用。', summary='先核对最大的目录。', duration_ms=1250,
                      usage=dict(input_tokens=1200, output_tokens=80, total_tokens=1280, cached_tokens=400, reasoning_tokens=30)))
                trace('tool_start', dict(call_id='tool-one', arguments='{"path":"/data","limit":20}'), 'get_directory', group_id='group-1')
                trace('tool_end', dict(call_id='tool-one', status='completed', result='{"allocated":123456}', duration_ms=40), 'get_directory', group_id='group-1')
                trace('group_report', dict(group_id='group-1', text='### 第 1/3 组 · 4 个容器\n\n第一组目录已核对。'))
                trace('group_state', dict(id='group-1', status='completed'))
                trace('group_state', dict(id='group-3', status='running'))
                trace('model_request', request_data('request-two', 2))
                trace('model_delta', dict(request_id='request-two', deltas=[dict(kind='text', text='{"containers":[')]))
                trace('model_response', dict(request_id='request-two', status='completed', text='{"containers":[]}' , summary='', duration_ms=350,
                      usage=dict(input_tokens=1600, output_tokens=400, total_tokens=2000, cached_tokens=800, reasoning_tokens=50)))
                trace('group_report', dict(group_id='group-3', text='### 第 3/3 组 · 1 个容器\n\n本组主要占用已分类。'))
                trace('group_state', dict(id='group-3', status='completed'))
                trace('model_response', dict(request_id='request-middle', status='completed', text='第二组正在核对缓存。'))
                trace('tool_start', dict(call_id='tool-one', arguments='{"container":"worker-4"}'), 'get_container', group_id='group-2')
                trace('tool_end', dict(call_id='tool-one', status='completed', result='{"secret_evidence":42}', duration_ms=25), 'get_container', group_id='group-2')
                trace('group_report', dict(group_id='group-2', text='### 第 2/3 组 · 4 个容器\n\n第二组缓存已核对。'))
                trace('group_state', dict(id='group-2', status='completed'))
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
                assert body == dict(snapshot_id=record_id, revision=7, concurrency=2, scope='container')
                sessions.append(session)
                messages.append(dict(id=1, role='user', content='生成空间消耗总报告'))
                trace('report_plan', dict(concurrency=2, groups=[dict(id=f'group-{g+1}', number=g+1, containers=[dict(id=f'c{i}', name=f'worker-{i}') for i in range(g*4, min(g*4+4, 9))]) for g in range(3)]))
                trace('group_state', dict(id='group-1', status='running'))
                trace('model_request', request_data('request-one', 1))
                trace('group_state', dict(id='group-2', status='running'))
                trace('model_request', request_data('request-middle', 1))
                return self.respond(session, 202)
            if self.path.endswith('/messages'):
                messages.extend([dict(id=len(messages)+1, role='user', content=body['message']),
                                 dict(id=len(messages)+2, role='assistant', content='## 继续排查结果\n已核对缓存候选。')])
                return self.respond(session, 202)
            if self.path.endswith('/retry'):
                assert body == dict(requests=[dict(group_id='group-2', request_id='failed-second'), dict(group_id='group-3', request_id='failed-latest')])
                next_id = max(m['id'] for m in messages) + 1
                events = []
                for target in body['requests']:
                    group = target['group_id']
                    request = 'retry-success-' + group
                    events.extend([
                        ('group_state', dict(id=group, status='running')),
                        ('model_request', dict(request_id=request, group_id=group, protocol='responses', round=3)),
                        ('model_response', dict(request_id=request, group_id=group, status='completed', text='已从失败请求恢复。')),
                        ('group_report', dict(group_id=group, text='### 重试结果\n\n本组恢复完成。')),
                        ('group_state', dict(id=group, status='completed')),
                    ])
                messages.extend(dict(id=next_id+i, role=role, content=json.dumps(data)) for i, (role, data) in enumerate(events))
                session['status'] = 'completed'
                return self.respond(session, 202)
            if self.path.endswith('/cancel'):
                session['status'] = 'cancelled'
                return self.respond(dict(ok=True))
        self.send_error(404)


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1080))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto('http://127.0.0.1:' + str(server.server_port) + NODE_PATH)
        page.locator('.platform-nav [data-page=overview]').click()
        page.wait_for_function('platform.loaded !== null')
        page.locator('#reportConcurrency').fill('2')
        page.locator('#generateReport').click()
        page.wait_for_function('document.querySelector("#agentActivityLog").textContent.includes("正在比较目录占用")')
        assert page.evaluate('agentView.session.status') == 'running'
        assert page.locator('#agentReports').is_hidden()
        assert page.locator('#agentActivityLog').inner_text().count('正在比较目录占用') == 1
        summary = page.locator('.agent-summary').first
        assert summary.get_attribute('open') is None
        summary.locator('summary').click()
        assert '先核对最大的目录' in summary.inner_text()
        assert '准备读取目录明细' in page.locator('#agentActivityLog').inner_text()
        for raw in ['只根据实际扫描证据', '{"path":', 'context_items', 'Token 用量']:
            assert raw not in page.locator('#agentActivityLog').inner_html()
        assert page.locator('.agent-user-message').count() == 1
        assert page.locator('.agent-assistant-message').count() == 2
        assert page.locator('#agentActivityLog script').count() == 0
        page.locator('#agentDialog').screenshot(path='/tmp/project-alpha-agent-streaming-desktop.png')
        assert page.locator('[data-agent-group]').count() == 3
        assert 'worker-0' in page.locator('[data-agent-group="group-1"]').inner_text()
        assert '分析中' in page.locator('[data-agent-group="group-2"]').inner_text()
        assert '分析中 2' in page.locator('#agentConcurrencyStatus').inner_text()
        assert page.locator('#agentConcurrency').input_value() == '2'
        assert page.locator('#agentConcurrency').is_disabled()
        assert '1 个容器' in page.locator('[data-agent-group="group-3"]').inner_text()
        page.locator('[data-agent-group="group-3"]').click()
        assert '等待空闲名额' in page.locator('#agentScopeEmpty').inner_text()
        page.locator('[data-agent-group="group-2"]').click()
        assert '正在比较目录占用' not in page.locator('#agentActivityLog').inner_text()
        continue_stream.set()
        page.wait_for_function('agentView.session?.status === "completed"')
        assert len(event_connections) >= 2, event_connections
        assert event_connections[1] > event_connections[0]
        assert page.evaluate('agentView.scope') == 'group-2', 'stream updates must not change manual selection'
        assert '第二组正在核对缓存' in page.locator('#agentActivityLog').inner_text()
        assert '正在比较目录占用' not in page.locator('#agentActivityLog').inner_text()
        assert page.locator('.agent-tool:visible').count() == 1
        assert 'worker-4' in page.locator('.agent-tool:visible').inner_text()
        page.locator('[data-agent-group="group-1"]').click()
        assert summary.get_attribute('open') is not None
        assert page.locator('.agent-tool:visible').count() == 1
        assert '40 ms' in page.locator('.agent-tool:visible').inner_text()
        assert '/data' in page.locator('.agent-tool:visible').inner_text()
        assert 'allocated' not in page.locator('#agentActivityLog').inner_html()
        assert 'secret_evidence' not in page.locator('#agentActivityLog').inner_html()
        assert '第二组正在核对缓存' not in page.locator('#agentActivityLog').inner_text()
        assert page.locator('#agentFollowup').is_hidden()
        assert page.locator('[data-agent-group]').filter(has_text='已完成').count() == 3
        page.locator('[data-agent-group="group-3"]').click()
        assert page.locator('.agent-reply-state:visible').count() == 0
        group = page.locator('.agent-group-result:visible')
        assert group.count() == 1
        assert group.get_attribute('open') is None
        group.locator('summary').click()
        assert '本组主要占用已分类' in group.inner_text()
        page.locator('[data-agent-group="group-1"]').click()
        page.locator('#agentActivityLog').evaluate('(el)=>el.scrollTop=0')
        page.locator('#agentDialog').screenshot(path='/tmp/project-alpha-agent-chat-desktop.png')
        page.locator('#agentReportTab').click()
        assert page.locator('#agentReports table').count() == 2
        assert page.locator('#agentReports img').count() == 0
        assert not page.evaluate('window.reportInjected || false')
        for category in ['可立即删除', '存在争议', '必须保留', '放错位置']:
            assert page.locator('#agentReports h3').filter(has_text=category).count() == 1
        assert '本组主要占用已分类' not in page.locator('#agentReports').inner_text()
        assert page.locator('#stopAgent').is_hidden()
        assert not page.locator('#agentConcurrency').is_disabled()
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
        assert page.locator('[data-agent-group]').count() == 3
        page.locator('[data-agent-group="group-1"]').click()
        assert '正在比较目录占用' in page.locator('#agentActivityLog').inner_text()
        page.locator('#agentActivityLog').evaluate('(el)=>el.scrollTop=0')
        page.locator('#agentDialog').screenshot(path='/tmp/project-alpha-agent-conversation-mobile.png')
        page.locator('#agentConversationTab').click()
        assert page.locator('#agentFollowup').is_visible()
        assert page.locator('#agentQuestion').bounding_box()['y'] < 844
        assert page.locator('#agentActivityLog').bounding_box()['height'] > 120
        page.locator('#agentReportTab').click()
        page.screenshot(path='/tmp/project-alpha-agent-report-mobile.png', full_page=True)
        assert page.evaluate('document.querySelector("#agentDialog").scrollWidth <= document.querySelector("#agentDialog").clientWidth + 1')
        # Exercise actual DOM scrolling and partial/failed states without a paid model.
        page.locator('#agentConversationTab').click()
        page.evaluate("""() => {
          agentView.session.status='running';
          const event=(id,role,data,tool_name)=>({id,role,content:JSON.stringify(data),tool_name});
          window.agentTestEvent=event;
          appendAgentMessages([
            {id:1000,role:'user',content:'继续检查目录'},
            event(1001,'model_request',{request_id:'partial',round:1}),
            event(1002,'model_delta',{request_id:'partial',deltas:[{kind:'text',text:'{"containers":['}]})
          ]);
        }""")
        assert page.locator('#agentRequestOutput-1001').inner_text() == ''
        assert '整理分析结果' in page.locator('#agentRequestState-1001').inner_text()
        assert 'containers' not in page.locator('#agentActivityLog').inner_html()
        page.evaluate("""() => {
          const event=window.agentTestEvent;
          appendAgentMessages([
            event(1003,'model_response',{request_id:'partial',status:'completed',text:'{"containers":[]}'}),
            event(1004,'tool_start',{call_id:'slow',arguments:'{"path":"/data/long/path"}'},'scan_directory')
          ]);
        }""")
        assert page.locator('#agentRequest-1001').is_hidden()
        assert '执行中' in page.locator('#agentTool-1004').inner_text()
        page.evaluate("""() => {
          const event=window.agentTestEvent;
          appendAgentMessages([
            event(1005,'tool_end',{call_id:'slow',status:'failed',result:'{"error":"目录不可读","debug":"hidden_raw_result"}'}),
            event(1006,'model_request',{request_id:'cancelled',round:2}),
            event(1007,'model_delta',{request_id:'cancelled',deltas:[{kind:'text',text:'已检查部分目录。\\n'.repeat(50)}]})
          ]);
          document.querySelector('#agentActivityLog').scrollTop=0;
        }""")
        assert '目录不可读' in page.locator('#agentTool-1004').inner_text()
        assert 'hidden_raw_result' not in page.locator('#agentActivityLog').inner_html()
        page.evaluate("""() => appendAgentMessages([agentTestEvent(1008,'model_delta',{request_id:'cancelled',deltas:[{kind:'text',text:'新的进度'}]})])""")
        assert page.locator('#agentActivityLog').evaluate('(el)=>el.scrollTop') == 0
        page.locator('#agentJumpLatest').click()
        page.evaluate("""() => appendAgentMessages([agentTestEvent(1009,'model_delta',{request_id:'cancelled',deltas:[{kind:'text',text:'\\n继续检查。'.repeat(10)}]})])""")
        assert page.locator('#agentActivityLog').evaluate('(el)=>el.scrollHeight-el.scrollTop-el.clientHeight') < 2
        page.evaluate("""() => {
          appendAgentMessages([agentTestEvent(1010,'model_response',{request_id:'cancelled',status:'cancelled',text:'',error:'分析已停止'})]);
          agentView.session.status='cancelled';renderAgentSession();
        }""")
        assert '已检查部分目录' in page.locator('#agentRequestOutput-1006').inner_text()
        assert '未完成' in page.locator('#agentRequestState-1006').inner_text()
        assert page.locator('#stopAgent').is_hidden()
        # Recover a persisted failed group through the real retry button/API.
        with lock:
            events = [
                ('model_request', dict(request_id='failed-second', group_id='group-2', protocol='responses', round=2)),
                ('model_response', dict(request_id='failed-second', group_id='group-2', status='failed', error='模型连接失败')),
                ('group_state', dict(id='group-2', status='failed')),
                ('model_request', dict(request_id='failed-latest', group_id='group-3', protocol='responses', round=3)),
                ('model_response', dict(request_id='failed-latest', group_id='group-3', status='failed', error='模型连接失败')),
                ('group_state', dict(id='group-3', status='failed', error='模型连接失败')),
            ]
            messages.extend(dict(id=2000+i, role=role, content=json.dumps(data)) for i, (role, data) in enumerate(events))
            session['status'] = 'failed'
        page.evaluate('readAgentSession()')
        page.locator('[data-agent-group="group-3"]').click()
        assert page.locator('#agentRetry').is_visible()
        assert page.locator('#agentRetry').is_enabled()
        assert '恢复 2 个失败 Agent' in page.locator('#agentRetry').get_attribute('title')
        assert '2' in page.locator('#agentRetry').inner_text()
        assert page.evaluate('document.querySelector("#agentDialog").scrollWidth <= document.querySelector("#agentDialog").clientWidth + 1')
        page.locator('#agentDialog').screenshot(path='/tmp/project-alpha-agent-retry-mobile.png')
        page.locator('#agentRetry').click()
        page.wait_for_function('agentView.requests.has("retry-success-group-2") && agentView.requests.has("retry-success-group-3")')
        assert page.evaluate('agentView.scope') == 'group-3'
        assert page.locator('#agentRetry').is_hidden()
        assert page.evaluate('agentView.groups.get("group-1").status') == 'completed'
        assert page.evaluate('agentView.groups.get("group-2").status') == 'completed'
        assert len([w for w in writes if w[0].endswith('/retry')]) == 1
        assert len([w for w in writes if w[0] == '/api/agent/reports']) == 1
        assert not errors, errors
        browser.close()
    print('Chromium Agent report passed: selected record, incremental SSE, reconnect cursors, chat bubbles, compact tools, hidden JSON, summary disclosure, scroll following, cancellation, safe Markdown tables, export, followup, reload recovery, mobile layout and no page errors.')
finally:
    server.shutdown()
