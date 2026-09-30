"""Real control + two API-only workers: navigation, isolation, registration and outages."""
import json
import os
import re
import sqlite3
import subprocess
import tempfile
import time
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='alpha-cluster-') as temporary:
    root = Path(temporary)
    binary = root / 'project-alpha'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=repo, check=True)
    processes, logs = [], []

    def start(name, mode, port=0):
        log = (root / (name + '.log')).open('w+')
        logs.append(log)
        env = dict(os.environ, PROJECT_ALPHA_WORKER_TOKEN='')
        process = subprocess.Popen([str(binary), mode, '--data-dir', str(root / name), '--port', str(port),
                                    '--tetragon-socket', str(root / 'missing.sock')],
                                   cwd=repo, env=env, stdout=log, stderr=log)
        processes.append(process)
        for _ in range(150):
            log.seek(0)
            output = log.read()
            match = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', output)
            token = re.search(r'worker token \(saved in data directory\): ([a-f0-9]{64})', output)
            if match and (mode == '--control' or token):
                return process, match[1], token[1] if token else ''
            if process.poll() is not None:
                break
            time.sleep(.1)
        log.seek(0)
        raise AssertionError(log.read())

    model_calls = []

    class ModelHandler(BaseHTTPRequestHandler):
        def log_message(self, *_):
            pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            model_calls.append(body)
            assert self.headers.get('Authorization') == 'Bearer control-model-key'
            if len(model_calls) == 1:
                message = dict(role='assistant', content='查询节点目录', tool_calls=[dict(id='host-query', type='function',
                    function=dict(name='get_host_directory', arguments=json.dumps(dict(path=str(root / 'files'), offset=0, limit=30))))])
                finish = 'tool_calls'
            else:
                assert 'host_allocated' in json.dumps(body), 'node tool evidence did not reach the central model'
                message = dict(role='assistant', content=json.dumps(dict(directories=[dict(path=str(root / 'files'), findings=[], note='测试目录，没有清理建议')]), ensure_ascii=False))
                finish = 'stop'
            for index, tool in enumerate(message.get('tool_calls', [])):
                tool['index'] = index
            data = ('data: ' + json.dumps(dict(choices=[dict(index=0, delta=message, finish_reason=finish)])) + '\n\ndata: [DONE]\n\n').encode()
            self.send_response(200)
            self.send_header('Content-Type', 'text/event-stream')
            self.send_header('Content-Length', str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    model_server = ThreadingHTTPServer(('127.0.0.1', 0), ModelHandler)
    threading.Thread(target=model_server.serve_forever, daemon=True).start()
    try:
        _, url, _ = start('control', '--control')
        node1, url1, token1 = start('node-one', '--worker')
        node2, url2, token2 = start('node-two', '--worker')
        for name in ['node-one', 'node-two']:
            with sqlite3.connect(root / name / 'platform.sqlite3') as db:
                # The same container ID on different nodes must count twice.
                db.execute("INSERT INTO managed_containers VALUES(?, 'unix:///missing.sock','test',?,'alice',?, 'fingerprint','create','',1,1)",
                           ('a' * 64, name + '-training', json.dumps(dict(image='test', network='bridge', port=2222, gpus='all'))))
                assert db.execute('SELECT count(*) FROM users').fetchone()[0] == 0
                assert not db.execute("SELECT name FROM sqlite_master WHERE name IN ('members','agent_settings','agent_sessions')").fetchall()
        with sync_playwright() as p:
            launch = dict(headless=True, args=['--no-sandbox'])
            if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
                launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
            browser = p.chromium.launch(**launch)
            context = browser.new_context(viewport=dict(width=1440, height=1000), reduced_motion='reduce')
            page = context.new_page()
            errors, requests = [], []
            page.on('pageerror', lambda error: errors.append(str(error)))
            page.on('request', lambda request: requests.append(request.url))
            public = p.request.new_context()
            assert public.get(url1).status == 401
            assert public.get(url1 + '/', headers={'Authorization': 'Bearer ' + token1}).status == 409
            page.goto(url)
            page.locator('#authUsername').fill('operator')
            page.locator('#authPassword').fill('A-test-password-123')
            page.locator('#authSubmit').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            expect(page.locator('#clusterNodes')).to_contain_text('第一个节点')
            expect(page.locator('body')).to_have_class('control-room')
            expect(page.locator('.platform-nav [data-page="overview"]')).not_to_be_visible()
            # Model credentials and configuration are saved once on the control.
            page.locator('.cluster-agent-entry [data-page="agent-settings"]').click()
            expect(page.locator('#agentSettingsStatus')).to_contain_text('已载入')
            page.locator('#agentProtocol').select_option('completions')
            page.locator('#agentEndpoint').fill('http://127.0.0.1:' + str(model_server.server_port))
            page.locator('#agentModel').fill('central-test-model')
            page.locator('#agentKey').fill('control-model-key')
            page.locator('#agentSaveSettings').click()
            expect(page.locator('#agentSettingsStatus')).to_contain_text('已保存')
            assert page.locator('#agentKey').input_value() == ''
            page.locator('#agentGuideNodes').click()
            for name, node_url, token in [('GPU 01', url1, token1), ('GPU 02', url2, token2)]:
                page.locator('#clusterAdd').click()
                page.locator('#nodeName').fill(name)
                page.locator('#nodeURL').fill(node_url)
                page.locator('#nodeToken').fill(token)
                page.locator('#nodeSave').click()
                expect(page.locator('#nodeDialog')).not_to_be_visible()
                expect(page.locator('#clusterNodes')).to_contain_text(name)
                assert page.locator('#nodeToken').input_value() == ''
            expect(page.locator('#clusterOnline')).to_have_text('2 / 2')
            expect(page.locator('#clusterContainers')).to_have_text('2')
            # Searching and filtering keep the cluster totals; empty results can be cleared.
            page.locator('#clusterSearch').fill('GPU 02')
            expect(page.locator('.node-card')).to_have_count(1)
            expect(page.locator('.node-card h3')).to_have_text('GPU 02')
            expect(page.locator('#clusterContainers')).to_have_text('2')
            page.locator('[data-node-filter="offline"]').click()
            expect(page.locator('#clusterNodes')).to_contain_text('没有匹配的节点')
            page.locator('[data-clear-nodes]').click()
            expect(page.locator('.node-card')).to_have_count(2)
            page.locator('.node-open').first.focus()
            page.evaluate('ClusterUI.refresh()')
            assert page.locator('.node-open').first.evaluate('(el) => el === document.activeElement'), 'refresh lost keyboard focus'
            assert not any('/api/' in u and '/snapshot' in u for u in requests), 'control room loaded a node snapshot'
            page.route(url + '/api/cluster/overview', lambda route: route.fulfill(status=503,
                       content_type='application/json', body=json.dumps(dict(error='临时连接失败'))), times=1)
            page.locator('#clusterRefresh').click()
            expect(page.locator('#clusterError')).to_contain_text('保留上次结果')
            expect(page.locator('#clusterContainers')).to_have_text('2')
            expect(page.locator('#clusterHealth')).to_have_text('集群状态更新失败')
            page.locator('#clusterRefresh').click()
            expect(page.locator('#clusterError')).to_be_empty()
            csrf = page.evaluate('platform.csrf')
            nodes = context.request.get(url + '/api/cluster/nodes').json()['nodes']
            assert all('token' not in n for n in nodes)
            first = next(n for n in nodes if n['name'] == 'GPU 01')
            second = next(n for n in nodes if n['name'] == 'GPU 02')
            # Users register only on the central control, via the existing invitation flow.
            page.locator('.platform-nav [data-page="members"]').click()
            expect(page.locator('#memberSchemaEditor')).to_be_enabled()
            page.locator('#memberCreateInvitation').click()
            expect(page.locator('#memberInvitationDialog')).to_be_visible()
            code = page.locator('#memberInvitationCode').input_value()
            page.locator('#memberInvitationClose').click()
            response = public.post(url + '/api/members/register', data=dict(username='alice', invitation_code=code,
                                  ssh_public_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f', schema_revision=1, profile={}))
            assert response.status == 201, response.text()
            registered_member_id = response.json()['id']
            page.locator('.platform-nav [data-page="allocations"]').click()
            expect(page.locator('#allocationRows')).to_contain_text('alice')
            expect(page.locator('#allocationRows summary')).to_contain_text('2 个容器 · 2 个节点')
            expect(page.locator('#allocationRows')).not_to_contain_text('未登记')
            page.route(url + '/api/cluster/overview', lambda route: route.fulfill(status=503,
                       content_type='application/json', body=json.dumps(dict(error='统计刷新失败'))), times=1)
            page.locator('#allocationsRefresh').click()
            expect(page.locator('#allocationStatus')).to_contain_text('上次结果')
            page.locator('#allocationSearch').fill('alice')
            expect(page.locator('#allocationStatus')).to_contain_text('上次结果')
            page.locator('#allocationSearch').fill('')
            page.locator('#allocationsRefresh').click()
            expect(page.locator('#allocationStatus')).to_contain_text('计算节点统计完整')
            expect(page.locator('#allocationRows')).to_contain_text('node-one-training')
            expect(page.locator('#allocationRows')).to_contain_text('node-two-training')
            page.screenshot(path='/tmp/project-alpha-cluster-users.png', full_page=True)
            page.locator('.platform-nav [data-page="cluster"]').click()
            page.screenshot(path='/tmp/project-alpha-cluster.png', full_page=True)
            page.set_viewport_size(dict(width=390, height=844))
            assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'mobile overflow'
            page.screenshot(path='/tmp/project-alpha-cluster-mobile.png', full_page=True)
            page.set_viewport_size(dict(width=1440, height=1000))
            # The name area is part of the card's real link, not a JS-only click target.
            card_title = page.locator('.node-card').filter(has=page.locator(f'a.node-open[href="/nodes/{first["id"]}/"]')).locator('h3')
            card_title.scroll_into_view_if_needed()
            box = card_title.bounding_box()
            page.mouse.click(box['x'] + 10, box['y'] + 10)
            expect(page.locator('#page-dashboard')).to_be_visible()
            expect(page.locator('#nodeContextName')).to_have_text('GPU 01')
            expect(page.locator('body')).not_to_have_class('control-room')
            expect(page.locator('.platform-nav [data-page="agent-settings"]')).not_to_be_visible()
            expect(page.locator('.platform-nav [data-page="members"]')).not_to_be_visible()
            expect(page.locator('#page-dashboard [data-page="agent-settings"]')).to_have_count(0)
            # Let session restoration finish before the final deferred module arrives.
            delayed = []
            page.route(url + '/cluster.js', lambda route: delayed.append(route), times=1)
            page.goto(url + '/nodes/' + first['id'] + '/', wait_until='commit')
            for _ in range(100):
                if page.evaluate('typeof enter === "function"'):
                    break
                page.wait_for_timeout(25)
            page.wait_for_timeout(500)
            assert delayed
            delayed[0].continue_()
            page.wait_for_load_state('domcontentloaded')
            expect(page.locator('#page-dashboard')).to_be_visible()
            expect(page.locator('.platform-nav [data-page="agent-settings"]')).not_to_be_visible()
            expect(page.locator('.platform-nav [data-page="members"]')).not_to_be_visible()
            page.screenshot(path='/tmp/project-alpha-node-dashboard.png', full_page=True)
            page.locator('.platform-nav [data-page="overview"]').click()
            page.locator('#storageNav [data-page="scan-settings"]').click()
            expect(page.locator('#settingsStatus')).to_contain_text('已保存配置')
            fixture = root / 'files'
            fixture.mkdir()
            (fixture / 'example.txt').write_text('cluster scan\n')
            page.locator('#cfgRoots').fill(str(fixture))
            page.locator('#cfgDocker').uncheck()
            page.locator('#cfgScanBackend').select_option('host')
            page.locator('#saveSettings').click()
            expect(page.locator('#settingsStatus')).to_contain_text('保存成功')
            page.locator('#storageNav [data-page="overview"]').click()
            page.locator('#startScan').click()
            expect(page.locator('#resultContent')).to_be_visible(timeout=30000)
            assert page.evaluate('platform.loaded'), 'proxied snapshot failed to load'
            assert any('/api/cluster/nodes/' + first['id'] + '/api/jobs/' in u and '/snapshot' in u for u in requests)
            # A report runs on the control and calls the selected worker's tools.
            csrf = page.evaluate('platform.csrf')
            report = context.request.post(url + '/api/cluster/nodes/' + first['id'] + '/api/agent/reports',
                headers={'X-CSRF-Token': csrf}, data=dict(scope='host', snapshot_id=page.evaluate('platform.loaded'),
                revision=page.evaluate('snapshot.revision'), concurrency=1))
            assert report.status == 202, report.text()
            session_id = report.json()['id']
            for _ in range(150):
                result = context.request.get(url + '/api/cluster/nodes/' + first['id'] + '/api/agent/sessions/' + session_id).json()
                if result['session']['status'] not in ['queued', 'running', 'scanning', 'cancelling']:
                    break
                time.sleep(.1)
            assert result['session']['status'] == 'completed', result['session']
            assert len(model_calls) >= 2
            other = context.request.get(url + '/api/cluster/nodes/' + second['id'] + '/api/agent/sessions').json()
            assert other['sessions'] == [], other
            assert context.request.get(url + '/api/cluster/nodes/' + second['id'] + '/api/agent/sessions/' + session_id).status == 404
            with sqlite3.connect(root / 'control' / 'platform.sqlite3') as db:
                assert db.execute('SELECT node_id FROM agent_sessions WHERE id=?', (session_id,)).fetchone()[0] == first['id']
                value, ciphertext = db.execute('SELECT value,api_key_ciphertext FROM agent_settings').fetchone()
                assert 'central-test-model' in value and 'control-model-key' not in value and ciphertext
            for name in ['node-one', 'node-two']:
                assert not (root / name / 'agent-api-key.key').exists()
            node2config = context.request.get(url + '/api/cluster/nodes/' + second['id'] + '/api/settings').json()
            assert node2config['value']['root'] != [str(fixture)], 'node databases were mixed'
            assert all(not u.startswith(url1 + '/') and not u.startswith(url2 + '/') for u in requests), 'browser connected directly to worker'
            page.locator('#nodeContext a').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            # A worker becoming unavailable after the overview loaded cannot log us out.
            with sqlite3.connect(root / 'control' / 'platform.sqlite3') as db:
                db.execute('UPDATE cluster_nodes SET token=? WHERE id=?', ('invalid-token-' * 5, second['id']))
            page.locator(f'a.node-open[href="/nodes/{second["id"]}/"]').click()
            expect(page.locator('#nodeUnavailableDialog')).to_be_visible()
            expect(page.locator('#page-cluster')).to_be_visible()
            expect(page.locator('#authPanel')).not_to_be_visible()
            page.locator('#nodeUnavailableClose').click()
            # Direct navigation to a failed node also preserves the authenticated workspace.
            page.goto(url + '/nodes/' + second['id'] + '/')
            expect(page.locator('#nodeUnavailableDialog')).to_be_visible()
            expect(page.locator('#authPanel')).not_to_be_visible()
            page.locator('#nodeUnavailableHome').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            with sqlite3.connect(root / 'control' / 'platform.sqlite3') as db:
                db.execute('UPDATE cluster_nodes SET token=? WHERE id=?', (token2, second['id']))
            node2.terminate()
            node2.wait(timeout=10)
            page.locator('#clusterRefresh').click()
            expect(page.locator('#clusterPartial')).to_be_visible()
            expect(page.locator('#clusterOnline')).to_have_text('1 / 2')
            expect(page.locator('#clusterHealth')).to_have_text('1 个节点暂时不可用')
            page.locator('[data-node-filter="offline"]').click()
            expect(page.locator('.node-card')).to_have_count(1)
            expect(page.locator('.node-metrics strong').first).to_have_text('—')
            page.locator('.node-open').click()
            expect(page.locator('#nodeUnavailableDialog')).to_be_visible()
            expect(page.locator('#page-cluster')).to_be_visible()
            expect(page.locator('#authPanel')).not_to_be_visible()
            page.locator('#nodeUnavailableClose').click()
            page.screenshot(path='/tmp/project-alpha-cluster-offline.png', full_page=True)
            # Kill and restart the default worker, keeping its data and listening address.
            # The stale offline card must probe again without waiting for an overview refresh.
            page.evaluate('clearTimeout(platform.poll)')
            node2, restarted_url, restarted_token = start('node-two', '--worker', urlsplit(url2).port)
            assert restarted_url == url2 and restarted_token == token2, 'restart changed node credentials'
            page.locator('.node-open').click()
            expect(page.locator('#page-dashboard')).to_be_visible()
            expect(page.locator('#nodeContextName')).to_have_text('GPU 02')
            expect(page.locator('.platform-nav [data-page="agent-settings"]')).not_to_be_visible()
            expect(page.locator('.platform-nav [data-page="members"]')).not_to_be_visible()
            # Recover within an already-open node page using the failure dialog.
            node2.kill()
            node2.wait(timeout=10)
            page.reload()
            expect(page.locator('#nodeUnavailableDialog')).to_be_visible()
            expect(page.locator('#authPanel')).not_to_be_visible()
            page.evaluate('clearTimeout(platform.poll)')
            node2, _, restarted_token = start('node-two', '--worker', urlsplit(url2).port)
            assert restarted_token == token2
            page.locator('#nodeUnavailableRetry').click()
            expect(page.locator('#nodeUnavailableDialog')).not_to_be_visible()
            expect(page.locator('#page-dashboard')).to_be_visible()
            expect(page.locator('#authPanel')).not_to_be_visible()
            page.locator('#nodeContext a').click()
            expect(page.locator('#clusterOnline')).to_have_text('2 / 2')
            page.locator('#clusterRefresh').click()
            expect(page.locator('#clusterOnline')).to_have_text('2 / 2')
            page.reload()
            expect(page.locator('#clusterOnline')).to_have_text('2 / 2')
            page.locator('[data-node-filter="all"]').click()
            csrf = page.evaluate('platform.csrf')
            blocked = context.request.delete(url + '/api/cluster/nodes/' + second['id'], headers={'X-CSRF-Token': csrf}, data={})
            assert blocked.status == 409, blocked.text()
            deleted = context.request.delete(url + '/api/members/' + registered_member_id, headers={'X-CSRF-Token': csrf}, data={})
            assert deleted.status == 202, deleted.text()
            for _ in range(100):
                remaining = context.request.get(url + '/api/members/' + registered_member_id + '/resources')
                if remaining.status == 404:
                    break
                page.wait_for_timeout(50)
            assert remaining.status == 404, remaining.text()
            page.locator(f'[data-remove-node="{second["id"]}"]').click()
            page.locator('#nodeRemoveConfirm').click()
            expect(page.locator('#nodeRemoveDialog')).not_to_be_visible()
            expect(page.locator('#clusterOnline')).to_have_text('1 / 1')
            # Viewer role is central and applies to every proxied node operation.
            csrf = page.evaluate('platform.csrf')
            response = context.request.post(url + '/api/users', headers={'X-CSRF-Token': csrf},
                                            data=dict(username='viewer', password='A-test-password-123', role='viewer'))
            assert response.status == 201
            page.locator('#logoutButton').click()
            page.locator('#authUsername').fill('viewer')
            page.locator('#authPassword').fill('A-test-password-123')
            page.locator('#authSubmit').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            expect(page.locator('#clusterAdd')).not_to_be_visible()
            expect(page.locator('[data-edit-node]')).to_have_count(0)
            expect(page.locator('[data-remove-node]')).to_have_count(0)
            viewer_csrf = page.evaluate('platform.csrf')
            denied = context.request.post(url + '/api/cluster/nodes/' + first['id'] + '/api/jobs',
                                          headers={'X-CSRF-Token': viewer_csrf}, data={})
            assert denied.status == 403
            assert not errors, errors
            with sqlite3.connect(root / 'control' / 'platform.sqlite3') as db:
                assert not db.execute("SELECT name FROM sqlite_master WHERE name='jobs'").fetchall()
            browser.close()
        print('Cluster browser checks passed: API-only workers, central members, two-node statistics, scoped scans/snapshots, central model and remote tools, deferred module loading, persisted tokens, offline/auth failure dialogs and crash recovery, removal, viewer guards and mobile layout.')
    finally:
        model_server.shutdown()
        model_server.server_close()
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        for log in logs:
            log.close()
