"""Exercise the real control UI and database. Optional disposable worker URLs run the same flow on Docker."""
import contextlib
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import time
from playwright.sync_api import sync_playwright, expect
from browser_support import BrowserHandler, launch_options, start_server

TOKEN = 'interconnect-fixture-token-for-isolated-workers'
ROOT = Path(__file__).resolve().parents[1]
PATH = '/api/cluster/interconnect'


def worker_fixture(index):
    identity = str(index) * 32

    class Handler(BrowserHandler):
        applied = 0

        def do_GET(self):
            if self.path == '/api/worker/info':
                return self.respond(dict(id=identity, mode='worker', protocol=13))
            if self.path == '/api/worker/inventory':
                return self.respond(dict(containers=[], services=[], filesystems=[], gpu=dict(devices=[])))
            if self.path == '/api/containers/interconnect':
                candidates = [dict(id=str(index)*64, name='one', owner='user-'+str(index))]
                for i, name in enumerate(('inference-api', 'embedding-worker', 'training-runner')):
                    candidates.append(dict(id=f'{index * 16 + i:064x}', name=name, owner='ml-platform'))
                return self.respond(dict(candidates=candidates, applied=type(self).applied))
            self.respond({})

        def do_POST(self):
            request = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            assert self.headers['Authorization'] == 'Bearer ' + TOKEN
            if request['operation'] == 'apply':
                type(self).applied = request['config']['revision']
            self.respond(dict(protocol=1, interface='eth0', firewall='iptables', applied=type(self).applied))

    return start_server(Handler)


def main():
    with tempfile.TemporaryDirectory(prefix='alpha-interconnect-browser-') as tmp, contextlib.ExitStack() as stack:
        binary = Path(tmp) / 'project-alpha'
        subprocess.run(['go', 'build', '-o', binary, './cmd/project-alpha'], cwd=ROOT, check=True)
        log = stack.enter_context((Path(tmp) / 'control.log').open('w+'))
        process = subprocess.Popen([binary, 'serve', '--data-dir', tmp, '--port', '0'], stdout=log, stderr=log)
        stack.callback(lambda: (process.terminate(), process.wait(timeout=15)))
        for _ in range(150):
            log.seek(0)
            url = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', log.read())
            if url:
                break
            time.sleep(.1)
        assert url, 'control did not start'
        url = url[1]
        workers = []
        if os.environ.get('ALPHA_INTERCONNECT_BROWSER_WORKERS'):
            workers = json.loads(os.environ['ALPHA_INTERCONNECT_BROWSER_WORKERS'])
        else:
            for index in (1, 2):
                server = worker_fixture(index)
                stack.callback(server.shutdown)
                workers.append(dict(id=str(index)*32, url='http://127.0.0.1:'+str(server.server_port),
                                    internal_ip='192.0.2.'+str(index)))
        with sqlite3.connect(Path(tmp)/'platform.sqlite3') as db:
            for index, worker in enumerate(workers):
                db.execute('INSERT INTO cluster_nodes VALUES(?,?,?,?,?,?,?)',
                           (worker['id'], '计算 '+str(index+1), worker['url'], TOKEN, index, 'worker', worker['internal_ip']))
        with sync_playwright() as pw:
            browser = pw.chromium.launch(**launch_options())
            page = browser.new_page(viewport=dict(width=1280, height=900))
            errors = []
            page.on('pageerror', lambda error: errors.append(getattr(error,'stack',str(error))))
            page.goto(url)
            page.locator('#authUsername').fill('operator')
            page.locator('#authPassword').fill('Operator-password-123')
            page.locator('#authSubmit').click()
            page.locator('[data-page="interconnect"]').click()
            def tab(target, name):
                target.locator(f'[data-link-tab="{name}"]').click()

            def candidate(root, node_id):
                return root.locator(f'button[data-key^="{node_id}/"]').filter(has_text=re.compile(r'^one'))

            tab(page, 'network')
            expect(page.locator('#linkNodes')).to_contain_text('helper v1', timeout=30000)
            page.locator('#linkPool').fill('10.240.0.0/16')
            page.locator('#linkPoolForm button').click()
            expect(page.locator('#linkVersion')).to_have_text('配置版本 1')

            # Standalone attachment and lookup by owner, short ID and IP.
            page.locator('#linkAttach').click()
            attach = page.locator('#linkAttachPicker')
            attach.get_by_label('筛选节点').select_option(workers[0]['id'])
            candidate(attach, workers[0]['id']).click()
            expect(page.locator('#linkEndpoints article')).to_have_count(1, timeout=30000)
            expect(page.locator('#linkPoolForm button')).to_be_disabled()

            tab(page, 'build')
            directory = page.locator('#linkPicker')
            a = candidate(directory, workers[0]['id'])
            b = candidate(directory, workers[1]['id'])
            search = directory.get_by_label('搜索容器')
            search.fill('not-a-matching-container')
            expect(directory).to_contain_text('没有匹配的容器')
            search.fill('10.240.0.2')
            expect(directory.locator('.ic-choice')).to_have_count(1)
            short_id = a.get_attribute('data-key').split('/')[1][:12]
            search.fill(short_id)
            expect(a).to_be_visible()
            search.fill('user-1')
            a.click()
            expect(page.locator('#linkA')).to_contain_text('计算 1')
            expect(page.locator('#linkSave')).to_be_disabled()
            a.click()
            expect(page.locator('#linkA')).to_contain_text('选择容器')
            a.click()
            page.locator('#linkB').get_by_role('button', name='选择端点 B').click()
            expect(search).to_be_focused()
            search.fill('one')
            directory.get_by_label('筛选节点').select_option(workers[1]['id'])
            b.click()
            expect(page.locator('#linkB')).to_contain_text('计算 2')
            page.locator('#linkB').get_by_role('button', name='移除端点 B').click()
            expect(page.locator('#linkSave')).to_be_disabled()
            b.click()
            expect(page.locator('#linkSave')).to_have_text('接入并保存连接')
            page.locator('[name=first]').fill('18000')
            page.locator('[name=last]').fill('18001')
            page.locator('#linkAddRule').click()
            page.locator('[name=protocol]').last.select_option('icmp')
            expect(page.locator('[name=first]').last).to_be_disabled()

            # Lose the final save after an attachment has been acknowledged.
            # A retry must keep the draft and must not allocate the endpoint again.
            def fail_save(route):
                if route.request.method == 'POST' and route.request.post_data_json.get('action') == 'save':
                    route.fulfill(status=409, content_type='application/json',
                                  body=json.dumps(dict(error='测试：请重试保存')))
                else:
                    route.continue_()

            page.route('**' + PATH, fail_save)
            page.locator('#linkSave').click()
            expect(page.locator('#linkMessage')).to_contain_text('请重试保存', timeout=30000)
            expect(page.locator('#linkEndpointCount')).to_have_text('2')
            expect(page.locator('#linkSave')).to_have_text('保存并应用')
            expect(page.locator('[name=first]').first).to_have_value('18000')
            page.unroute('**' + PATH, fail_save)
            page.locator('#linkSave').click()
            expect(page.locator('#linkConnections')).to_contain_text('已应用', timeout=30000)
            page.locator('#linkConnections').get_by_role('button', name='编辑').click()

            # A colleague changing these rules must not overwrite the open draft.
            session = page.request.get(url+'/api/session').json()
            assert page.request.post(url+'/api/users', headers={'X-CSRF-Token': session['csrf']},
                    data=dict(username='editor', password='Editor-password-123', role='admin')).ok
            colleague = browser.new_context()
            assert colleague.request.post(url+'/api/login',
                    data=dict(username='editor', password='Editor-password-123')).ok
            other_admin = colleague.new_page()
            other_admin.goto(url+'/#interconnect')
            tab(other_admin, 'connections')
            expect(other_admin.locator('#linkConnections')).to_contain_text('已应用', timeout=30000)
            page.locator('[name=first]').first.fill('18003')
            page.locator('[name=last]').first.fill('18003')
            other_admin.locator('#linkConnections').get_by_role('button', name='编辑').click()
            other_admin.locator('[name=first]').first.fill('18004')
            other_admin.locator('[name=last]').first.fill('18004')
            other_admin.locator('#linkSave').click()
            expect(other_admin.locator('#linkConnections')).to_contain_text('18004', timeout=30000)
            updated = page.request.get(url+PATH).json()
            page.locator('#linkRefresh').click()
            expect(page.locator('#linkVersion')).to_have_text('配置版本 '+str(updated['revision']))
            expect(page.locator('[name=first]').first).to_have_value('18003')
            expect(page.locator('#linkEditorVersion')).to_contain_text('重载')
            with page.expect_response(lambda response: response.url == url+PATH and response.request.method == 'POST') as stale:
                page.locator('#linkSave').click()
            assert stale.value.status == 409
            expect(page.locator('#linkMessage')).to_contain_text('配置版本已改变')
            preserved = page.request.get(url+PATH).json()
            assert preserved['revision'] == updated['revision']
            assert preserved['connections'][0]['rules'][0]['first'] == 18004
            page.locator('#linkReload').click()
            expect(page.locator('[name=first]').first).to_have_value('18004')
            expect(page.locator('#linkConflict')).to_be_hidden()
            page.locator('[name=first]').first.fill('18000')
            page.locator('[name=last]').first.fill('18001')
            page.locator('#linkSave').click()
            expect(page.locator('#linkConnections')).to_contain_text('18000–18001', timeout=30000)
            colleague.close()

            page.locator('#linkConnections').get_by_role('button', name='诊断').click()
            expect(page.locator('#linkProbeResult')).to_have_text('尚未测试')
            page.locator('#linkProbePort').fill('18000')
            page.locator('#linkProbeForm button').click()
            expect(page.locator('#linkProbeResult')).to_contain_text('TCP 连接成功', timeout=20000)
            page.locator('#linkProbeDialog').get_by_role('button', name='关闭').click()
            page.set_viewport_size(dict(width=390, height=844))
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
            page.locator('#linkConnections').get_by_role('button', name='禁用').click()
            expect(page.locator('#linkConnections')).to_contain_text('已禁用', timeout=30000)
            page.on('dialog', lambda dialog: dialog.accept())
            page.locator('#linkConnections').get_by_role('button', name='删除').click()
            expect(page.locator('#linkConnections article')).to_have_count(0, timeout=30000)
            tab(page, 'network')
            for remaining in (1, 0):
                page.locator('#linkEndpoints button').first.click()
                expect(page.locator('#linkEndpoints article')).to_have_count(remaining, timeout=30000)
            tab(page, 'build')
            search.fill('one')
            directory.get_by_label('筛选节点').select_option(value='all')
            expect(directory.get_by_label('筛选节点').locator('option:checked')).to_have_text('全部节点')
            a.click()
            b.click()
            expect(page.locator('#linkSave')).to_be_in_viewport()
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
            session = page.request.get(url+'/api/session').json()
            assert page.request.post(url+PATH, data=dict(action='retry')).status == 403
            assert page.request.post(url+'/api/users', headers={'X-CSRF-Token': session['csrf']},
                    data=dict(username='reader', password='Reader-password-123', role='viewer')).ok
            viewer = browser.new_context()
            assert viewer.request.post(url+'/api/login', data=dict(username='reader', password='Reader-password-123')).ok
            other = viewer.new_page()
            other.goto(url+'/#interconnect')
            expect(other.locator('[data-page=interconnect]')).to_be_hidden()
            assert viewer.request.get(url+PATH).status == 403
            assert not errors, errors
            browser.close()


if __name__ == '__main__':
    main()
    print('Interconnect browser flow passed.')
