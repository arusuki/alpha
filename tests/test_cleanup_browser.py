"""Report selection, persisted cleanup results and explicit deletion in the node UI."""
import json
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright
from browser_support import NodeHandler, NODE_PATH, launch_options, start_server

user = dict(id='admin', username='admin', role='admin')
cleanup = dict(id='a' * 32, status='ready', snapshot_id='b' * 32, error='', report_scope='container')
host_cleanup = dict(id='c' * 32, status='ready', snapshot_id='b' * 32, error='', report_scope='host')
host_entries = [dict(id='8' * 32, path='/srv/host-cache', category=1, summary='Host 缓存',
                     detail=json.dumps(dict(bytes=8192, locations=['Host：/srv'], kind='下载与包缓存', summary='缓存目录', reason='可重建')),
                     status='pending', error='')]
entries = [dict(id=str(i) * 32, path=f'/data/{name}', category=i,
                summary='用途 <script>unsafe()</script>；处理前请核对依赖',
                detail=json.dumps(dict(bytes=4096 * i, locations=['容器：/workspace/' + name],
                                       kind='其他', summary='原始用途', reason='原始处理条件')),
                status='pending', error='')
           for i, name in enumerate(['cache', 'uncertain', 'keep', 'misplaced'], 1)]
prepared = host_prepared = deleting = False
reads = delete_attempts = history_deletes = 0
writes = []


class Handler(NodeHandler):
    def do_GET(self):
        global reads
        if self.control_request():
            return
        path = urlparse(self.path).path
        if path == '/api/session':
            return self.respond(dict(user=user, csrf='test', setup_required=False))
        if path == '/api/state':
            return self.respond(dict(jobs=[], directory_jobs=[], latest_id=None, active=None, interval_minutes=0))
        if path == '/api/agent/cleanup-reports':
            return self.respond(dict(reports=[dict(report_id=2, title='Host 空间报告', report_scope='host',
                snapshot_id=host_cleanup['snapshot_id'], created_at=1789372801,
                cleanup_id=host_cleanup['id'] if host_prepared else None, cleanup_status=host_cleanup['status']),
                dict(report_id=1, title='空间消耗总报告', report_scope='container',
                snapshot_id=cleanup['snapshot_id'], created_at=1789372800,
                cleanup_id=cleanup['id'] if prepared else None, cleanup_status=cleanup['status'])]))
        if path == '/api/agent/cleanups/' + host_cleanup['id']:
            return self.respond(dict(cleanup=host_cleanup, entries=host_entries))
        if path == '/api/agent/cleanups/' + cleanup['id']:
            reads += 1
            if deleting and reads >= 2:
                cleanup['status'] = 'completed'
                entries[0]['status'] = 'deleted'
                entries[0]['error'] = '已清理；保留 socket 1 个、字符设备 2 个及其所在目录'
            return self.respond(dict(cleanup=cleanup, entries=entries))
        if path == '/api/agent/sessions':
            return self.respond(dict(sessions=[]))
        self.serve_asset()

    def do_POST(self):
        global prepared, host_prepared, user, deleting, reads, delete_attempts
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        password = body.pop('sudo_password', None)
        writes.append((self.path, body))
        assert self.headers.get('X-CSRF-Token') == 'test'
        if self.path == '/api/agent/cleanups':
            if body == dict(report_id=2):
                host_prepared = True
                return self.respond(dict(cleanup=host_cleanup, entries=host_entries), 201)
            assert body == dict(report_id=1)
            if not prepared:
                deleting = False
                cleanup['status'] = 'ready'
                for entry in entries:
                    entry.update(status='pending', error='')
            prepared = True
            return self.respond(dict(cleanup=cleanup, entries=entries), 201)
        if self.path.endswith('/delete'):
            assert password == 'test-only-browser-sudo'
            password = None
            delete_attempts += 1
            if delete_attempts == 1:
                return self.respond(dict(error='测试服务暂时不可用，请重新输入密码后再提交'), 503)
            assert body == dict(entry_ids=['1' * 32])
            entries[0]['status'] = 'deleting'
            cleanup['status'] = 'running'
            deleting = True
            reads = 0
            return self.respond(dict(cleanup=cleanup, entries=entries), 202)
        if self.path == '/api/logout':
            user = None
            return self.respond(dict(ok=True))
        self.send_error(404)

    def do_DELETE(self):
        global prepared, history_deletes
        assert self.headers.get('X-CSRF-Token') == 'test'
        if self.path == '/api/agent/cleanups/' + cleanup['id'] and cleanup['status'] == 'completed':
            history_deletes += 1
            prepared = False
            return self.respond(dict(ok=True))
        self.send_error(404)


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1000))
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        url = 'http://127.0.0.1:' + str(server.server_port)
        page.goto(url + NODE_PATH)
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('!document.getElementById("cleanupPrepare").disabled')
        assert page.locator('#pageTitle').inner_text() == '诊断清理'
        page.locator('#cleanupPrepare').click()
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
        page.wait_for_function("cleanupView.cleanup.status === 'completed' && cleanupView.entries[0].status === 'deleted'")
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
        assert page.locator('[data-cleanup-entry]:disabled').count() == 1
        assert page.locator('#cleanupPrepare').is_disabled()
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
        assert page.locator('#cleanupEntries').inner_text() == '选择完整报告后读取条目。'
        page.reload()
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('document.querySelectorAll("[data-cleanup-entry]").length === 4')
        page.locator('#cleanupRemoveHistory').click()
        assert page.locator('#cleanupHistoryDialog').is_visible()
        assert '已清理的磁盘内容无法恢复' in page.locator('#cleanupHistoryDialog').inner_text()
        page.locator('#cleanupHistoryClose').click()
        assert history_deletes == 0
        page.locator('#cleanupRemoveHistory').click()
        page.locator('#cleanupHistoryConfirm').click()
        page.wait_for_function('cleanupView.cleanup === null')
        assert history_deletes == 1
        assert page.locator('#cleanupRemoveHistory').is_hidden()
        assert page.locator('#cleanupPrepare').is_enabled()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.reload()
        page.locator('.platform-nav [data-page="overview"]').click()
        page.locator('#storageNav [data-page="cleanup"]').click()
        page.wait_for_function('!document.getElementById("cleanupPrepare").disabled')
        assert page.locator('#cleanupRemoveHistory').is_hidden()
        assert page.locator('#cleanupReport option').count() == 1
        assert page.locator('#cleanupHostReport option').count() == 1
        page.locator('#cleanupHostPrepare').click()
        page.wait_for_function('cleanupView.reportScope === "host"')
        assert 'Host' in page.locator('#cleanupWorkspaceTitle').inner_text()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.locator('#cleanupHostPrepare').click()
        page.wait_for_function('document.querySelector("[data-cleanup-entry]")?.getAttribute("data-cleanup-entry") === "' + '8' * 32 + '"')
        assert ('/api/agent/cleanups', dict(report_id=2)) in writes
        assert page.locator('#cleanupHostPrepare').is_disabled()
        page.locator('#cleanupPrepare').click()
        page.wait_for_function('cleanupView.reportScope === "container"')
        assert '容器 Agent' in page.locator('#cleanupWorkspaceTitle').inner_text()
        assert page.locator('[data-cleanup-entry]').count() == 0
        page.locator('#cleanupHostPrepare').click()
        page.wait_for_function('document.querySelector("[data-cleanup-entry]")?.getAttribute("data-cleanup-entry") === "' + '8' * 32 + '"')
        # Both scopes now have one prepared cleanup record: switch using real clicks.
        page.locator('#cleanupPrepare').click()
        page.wait_for_function('cleanupView.reportScope === "container" && !cleanupView.loading')
        page.locator('#cleanupPrepare').click()
        page.wait_for_function('cleanupView.cleanup?.status === "ready" && !cleanupView.posting')
        prepare_count = sum(url == '/api/agent/cleanups' for url, _ in writes)
        page.locator('#cleanupHostPrepare').click()
        page.wait_for_function('cleanupView.reportScope === "host" && !cleanupView.loading')
        page.locator('#cleanupPrepare').click()
        page.wait_for_function('cleanupView.reportScope === "container" && !cleanupView.loading')
        assert sum(url == '/api/agent/cleanups' for url, _ in writes) == prepare_count
        assert not errors, errors
        browser.close()
        print('Cleanup browser checks passed: report entries, selection, filesystem deletion, history deletion, reload and mobile.')
finally:
    server.shutdown()
    server.server_close()
