"""Central plan distribution against two real workers, with partial-result retries."""
import json
import os
import re
import subprocess
import tempfile
import time
from pathlib import Path

from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='alpha-scan-plan-') as temporary:
    root = Path(temporary)
    binary = root / 'project-alpha'
    subprocess.run(['go', 'generate', './internal/platform'], cwd=repo, check=True)
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=repo, check=True)
    processes, logs = [], []

    def start(name, mode):
        log = (root / (name + '.log')).open('w+')
        logs.append(log)
        process = subprocess.Popen([str(binary), mode, '--data-dir', str(root / name), '--port', '0',
                                    '--tetragon-socket', str(root / 'missing.sock')], cwd=repo,
                                   env=dict(os.environ, PROJECT_ALPHA_WORKER_TOKEN=''), stdout=log, stderr=log)
        processes.append(process)
        for _ in range(150):
            log.seek(0)
            output = log.read()
            address = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', output)
            token = re.search(r'worker token \(saved in data directory\): ([a-f0-9]{64})', output)
            if address and (mode == '--control' or token):
                return process, address[1], token[1] if token else ''
            if process.poll() is not None:
                break
            time.sleep(.1)
        log.seek(0)
        raise AssertionError(log.read())

    try:
        _, url, _ = start('control', '--control')
        _, url1, token1 = start('one', '--worker')
        worker2, url2, token2 = start('two', '--worker')
        with sync_playwright() as p:
            browser = p.chromium.launch(headless=True)
            context = browser.new_context(viewport=dict(width=1440, height=1000))
            page = context.new_page()
            errors = []
            page.on('pageerror', lambda error: errors.append(str(error)))
            page.goto(url)
            page.locator('#authUsername').fill('operator')
            page.locator('#authPassword').fill('A-test-password-123')
            page.locator('#authSubmit').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            csrf = page.evaluate('platform.csrf')
            for name, address, token in [('one', url1, token1), ('two', url2, token2)]:
                response = context.request.post(url + '/api/cluster/nodes', headers={'X-CSRF-Token': csrf},
                    data=dict(kind='worker', name=name, url=address, token=token, internal_ip='10.0.0.11'))
                assert response.status == 201, response.text()
            nodes = context.request.get(url + '/api/cluster/nodes').json()['nodes']
            first, second = [next(n for n in nodes if n['name'] == name) for name in ['one', 'two']]
            before = []
            for index, node in enumerate([first, second]):
                path = url + '/api/cluster/nodes/' + node['id'] + '/api/settings'
                settings = context.request.get(path).json()
                settings['value'].update(root=['/srv/' + node['name']], exclude=['/srv/' + node['name'] + '/skip'], max_depth=6+index)
                response = context.request.put(path, headers={'X-CSRF-Token': csrf}, data=settings)
                assert response.status == 200, response.text()
                before.append(response.json()['value'])
            saved, untouched = before
            # Distribute one plan to both workers without changing scan-specific settings.
            page.locator('#clusterScanSchedule').click()
            expect(page.locator('#scanPlanSubmit')).to_be_enabled()
            expect(page.locator('#scanPlanTargets input:checked')).to_have_count(2)
            page.locator('#planTimes').fill('03:15\n15:45')
            page.locator('#planTimezone').fill('Asia/Shanghai')
            page.locator('#planRetain').fill('5')
            page.locator('#scanPlanSubmit').click()
            expect(page.locator('#scanPlanStatus')).to_have_text('下发完成：成功 2 个，失败 0 个。')
            schedule_keys = {'schedule_mode', 'interval_minutes', 'schedule_times', 'schedule_weekdays', 'schedule_timezone', 'retain_records'}
            for node, before in [(first, saved), (second, untouched)]:
                current = context.request.get(url + '/api/cluster/nodes/' + node['id'] + '/api/settings').json()['value']
                assert current['schedule_times'] == ['03:15', '15:45'] and current['retain_records'] == 5
                assert {k: v for k, v in current.items() if k not in schedule_keys} == {k: v for k, v in before.items() if k not in schedule_keys}
            # Partial failures expose a retry that sends only failed IDs.
            batch_requests = []
            def partial_plan(route):
                body = route.request.post_data_json
                batch_requests.append(body)
                route.fulfill(status=200, content_type='application/json', body=json.dumps(dict(results=[
                    dict(id=first['id'], name=first['name'], ok=True),
                    dict(id=second['id'], name=second['name'], ok=False, error='模拟节点离线')])))
            page.route(url + '/api/cluster/scan-schedule', partial_plan, times=1)
            page.locator('#scanPlanSubmit').click()
            expect(page.locator('#scanPlanStatus')).to_have_text('下发完成：成功 1 个，失败 1 个。')
            expect(page.locator('#scanPlanResults')).to_contain_text('模拟节点离线')
            with page.expect_request('**/api/cluster/scan-schedule') as retry_request:
                page.locator('#scanPlanRetry').click()
            assert retry_request.value.post_data_json['node_ids'] == [second['id']]
            expect(page.locator('#scanPlanStatus')).to_have_text('下发完成：成功 2 个，失败 0 个。')
            expect(page.locator('#scanPlanRetry')).not_to_be_visible()
            # Subset selection and disabled automatic scans work independently.
            page.locator('#scanPlanSelectNone').click()
            expect(page.locator('#scanPlanSubmit')).to_be_disabled()
            page.locator('#scanPlanTargets input[value="' + first['id'] + '"]').check()
            page.locator('#planMode').select_option('off')
            page.locator('#scanPlanSubmit').click()
            expect(page.locator('#scanPlanStatus')).to_have_text('下发完成：成功 1 个，失败 0 个。')
            assert context.request.get(url + '/api/cluster/nodes/' + first['id'] + '/api/settings').json()['value']['schedule_mode'] == 'off'
            assert context.request.get(url + '/api/cluster/nodes/' + second['id'] + '/api/settings').json()['value']['schedule_mode'] == 'calendar'
            page.locator('#scanPlanClose').click()
            # An actual offline worker fails independently; the live worker is updated.
            worker2.terminate()
            worker2.wait(timeout=10)
            page.locator('#clusterScanSchedule').click()
            expect(page.locator('#scanPlanSubmit')).to_be_enabled()
            page.set_viewport_size(dict(width=390, height=844))
            assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'mobile overflow'
            assert page.locator('#scanPlanDialog').evaluate('(el) => el.scrollWidth <= el.clientWidth'), 'dialog overflow'
            page.screenshot(path='/tmp/project-alpha-scan-plan-mobile.png')
            page.set_viewport_size(dict(width=1440, height=1000))
            page.locator('#scanPlanSubmit').click()
            expect(page.locator('#scanPlanStatus')).to_have_text('下发完成：成功 1 个，失败 1 个。', timeout=20000)
            expect(page.locator('#scanPlanRetry')).to_be_visible()
            page.screenshot(path='/tmp/project-alpha-scan-plan.png')
            page.locator('#scanPlanClose').click()
            response = context.request.post(url + '/api/users', headers={'X-CSRF-Token': csrf},
                data=dict(username='viewer', password='A-test-password-123', role='viewer'))
            assert response.status == 201, response.text()
            page.locator('#logoutButton').click()
            page.locator('#authUsername').fill('viewer')
            page.locator('#authPassword').fill('A-test-password-123')
            page.locator('#authSubmit').click()
            expect(page.locator('#page-cluster')).to_be_visible()
            expect(page.locator('#clusterScanSchedule')).not_to_be_visible()
            assert not errors, errors
            browser.close()
        print('Scan plan browser checks passed: multiple workers, preserved settings, failed-only retries, subset selection, actual outage, viewer guard and mobile layout.')
    finally:
        for process in processes:
            if process.poll() is None:
                process.terminate()
        for process in processes:
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait(timeout=5)
        for log in logs:
            log.close()
