"""Real registry/control processes; synthetic provisioning results, no external services."""
import json
import os
import re
import secrets
import sqlite3
import subprocess
import tempfile
import time
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
ssh_key = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f\necdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBIvR3cOir2XFsX4NiA4QO1JKQ7c87emaiV0rBXS3fiseEt0seHFTvuv2Tl0Zz5jQJS1Ko0oVLFAQZ4BtLtn6hKg='

with tempfile.TemporaryDirectory(prefix='alpha-registry-') as temporary:
    root = Path(temporary)
    binary = root / 'project-alpha'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=repo, check=True)
    environment = {**os.environ, 'REG_PASS': 'Abcd1234', 'PROJECT_ALPHA_REGISTRY_TOKEN': ''}
    services = []
    logs = []

    def start(name, arguments):
        log = (root / f'{name}-{len(logs)}.log').open('w+')
        logs.append(log)
        process = subprocess.Popen([str(binary), *arguments], cwd=repo, env=environment, stdout=log, stderr=log)
        services.append(process)
        for _ in range(150):
            log.seek(0)
            content = log.read()
            match = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', content)
            if match:
                return process, match[1]
            if process.poll() is not None:
                break
            time.sleep(.1)
        raise AssertionError(content)

    def sql(directory, statement, values=()):
        with sqlite3.connect(root / directory / 'platform.sqlite3', timeout=10) as db:
            return db.execute(statement, values).fetchall()

    try:
        registry, registry_url = start('registry', ['--registry', '--data-dir', str(root / 'registry'), '--port', '0'])
        registry_token = (root / 'registry' / 'registry-token').read_text().strip()
        assert re.fullmatch('[a-f0-9]{64}', registry_token)
        control, control_url = start('control', ['--control', '--data-dir', str(root / 'control'), '--port', '0'])
        fields = [dict(key='group', label='课题组', type='select', required=True, options=['A组', 'B组']),
                  dict(key='note', label='<img src=x onerror=alert(1)>', type='text', required=False)]
        sql('control', 'UPDATE member_registration_schema SET fields=?', (json.dumps(fields),))

        with sync_playwright() as p:
            launch = dict(headless=True, args=['--no-sandbox'])
            if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
                launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
            browser = p.chromium.launch(**launch)
            context = browser.new_context(viewport=dict(width=1280, height=1000), reduced_motion='reduce')
            page = context.new_page()
            errors = []
            page.on('pageerror', lambda error: errors.append(str(error)))
            admin = context.new_page()
            admin.on('pageerror', lambda error: errors.append(str(error)))
            admin.goto(control_url)
            admin.locator('#authUsername').fill('operator')
            admin.locator('#authPassword').fill('A-test-password-123')
            admin.locator('#authSubmit').click()
            expect(admin.locator('#page-cluster')).to_be_visible()
            admin.locator('.platform-nav [data-page="settings"]').click()
            expect(admin.locator('#settingsUsername')).to_have_text('operator')
            admin.locator('.platform-nav [data-page="cluster"]').click()
            admin.locator('#clusterAdd').click()
            admin.locator('#nodeKind').select_option('registry')
            expect(admin.locator('#nodeKindHint')).to_contain_text('公网注册入口')
            admin.locator('#nodeName').fill('Public registry')
            admin.locator('#nodeURL').fill(registry_url)
            admin.locator('#nodeToken').fill('wrong-token-' * 4)
            admin.locator('#nodeSave').click()
            expect(admin.locator('#nodeFormError')).not_to_be_empty()
            assert sql('control', 'SELECT count(*) FROM cluster_nodes')[0][0] == 0
            admin.locator('#nodeToken').fill(registry_token)
            admin.locator('#nodeSave').click()
            expect(admin.locator('#nodeDialog')).not_to_be_visible()
            card = admin.locator('.node-card').filter(has_text='REGISTRY')
            expect(card.locator('.node-status')).to_have_text('已连接', timeout=15000)
            expect(card).to_contain_text('••••••')
            assert card.locator('[data-open-node]').count() == 0
            expect(admin.locator('#clusterOnline')).to_have_text('1 / 1')
            expect(admin.locator('#clusterContainers')).to_have_text('0')
            assert sql('control', 'SELECT kind,token FROM cluster_nodes')[0] == ('registry', registry_token)
            card.locator('[data-share-registry]').click()
            expect(admin.locator('#registryShareHint')).to_contain_text('暂无可用邀请码')
            expect(admin.locator('#registryShareInvitation')).to_be_disabled()
            admin.locator('#registryShareCreate').click()
            expect(admin.locator('#registryShareDialog')).not_to_be_visible()
            expect(admin.locator('#page-members')).to_be_visible()
            expect(admin.locator('#memberInvitationLabel')).to_be_focused()
            invitation_label = 'browser </option><img src=x onerror=alert(1)> " & <'
            admin.locator('#memberInvitationLabel').fill(invitation_label)
            admin.locator('#memberCreateInvitation').click()
            expect(admin.locator('#memberInvitationDialog')).to_be_visible()
            code = admin.locator('#memberInvitationCode').input_value()
            admin.locator('#memberInvitationClose').click()
            admin.locator('.platform-nav [data-page="cluster"]').click()
            card.locator('[data-share-registry]').click()
            expect(admin.locator('#registryShareInvitation')).to_be_enabled()
            invitation_id = sql('control', 'SELECT id FROM member_invitations')[0][0]
            expect(admin.locator(f'#registryShareInvitation option[value="{invitation_id}"]')).to_have_text(
                f'{invitation_label} · 剩余 1 / 1 · {invitation_id}')
            admin.locator('#registryShareInvitation').select_option(invitation_id)
            entry = registry_url + '/registry/Abcd1234/' + code
            expect(admin.locator('#registryShareURL')).to_have_value(entry)
            expect(admin.locator('#registryShareCopy')).to_be_enabled()
            context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=control_url)
            admin.evaluate("() => { navigator.clipboard.writeText = async () => { throw new DOMException('Denied', 'NotAllowedError'); }; }")
            admin.locator('#registryShareCopy').click()
            expect(admin.locator('#registryShareStatus')).to_have_text('链接已复制。')
            assert admin.evaluate('navigator.clipboard.readText()') == entry
            admin.set_viewport_size(dict(width=390, height=844))
            assert admin.evaluate('document.documentElement.scrollWidth <= innerWidth')
            admin.screenshot(path='/tmp/project-alpha-registry-share-mobile.png', full_page=True)
            admin.set_viewport_size(dict(width=1280, height=1000))
            admin.locator('#registryShareClose').click()
            expect(admin.locator('#registryShareURL')).to_have_value('')
            card.locator('[data-edit-node]').click()
            expect(admin.locator('#nodeKind')).to_be_disabled()
            expect(admin.locator('#nodeToken')).to_have_attribute('placeholder', '••••••')
            assert admin.locator('#nodeToken').input_value() == ''
            admin.locator('#nodeName').fill('Registration gateway')
            admin.locator('#nodeSave').click()
            expect(admin.locator('#nodeDialog')).not_to_be_visible()
            expect(card.locator('h3')).to_have_text('Registration gateway')
            assert sql('control', 'SELECT token FROM cluster_nodes')[0][0] == registry_token

            # Remote token rotation is applied through the control's edit dialog.
            registry.terminate()
            registry.wait(timeout=15)
            expect(card.locator('.node-status')).to_have_text('重连中', timeout=15000)
            expect(admin.locator('#clusterPartial')).to_be_hidden()
            card.locator('[data-share-registry]').click()
            expect(admin.locator('#registryShareInvitation')).to_be_enabled()
            admin.locator('#registryShareInvitation').select_option(invitation_id)
            expect(admin.locator('#registryShareError')).not_to_be_empty()
            expect(admin.locator('#registryShareCopy')).to_be_disabled()
            admin.locator('#registryShareClose').click()
            environment['PROJECT_ALPHA_REGISTRY_TOKEN'] = secrets.token_hex(32)
            registry, restarted_url = start('registry', ['--registry', '--data-dir', str(root / 'registry'), '--port', registry_url.rsplit(':', 1)[1]])
            assert restarted_url == registry_url
            card.locator('[data-edit-node]').click()
            admin.locator('#nodeToken').fill(environment['PROJECT_ALPHA_REGISTRY_TOKEN'])
            admin.locator('#nodeSave').click()
            expect(admin.locator('#nodeDialog')).not_to_be_visible()
            expect(card.locator('.node-status')).to_have_text('已连接', timeout=15000)
            assert sql('control', 'SELECT token FROM cluster_nodes')[0][0] == environment['PROJECT_ALPHA_REGISTRY_TOKEN']

            # Persisted registry links reconnect without control startup flags.
            control.terminate()
            control.wait(timeout=15)
            control, restarted_control_url = start('control', ['--control', '--data-dir', str(root / 'control'), '--port', control_url.rsplit(':', 1)[1]])
            assert restarted_control_url == control_url
            admin.reload()
            expect(card.locator('.node-status')).to_have_text('已连接', timeout=15000)
            card.locator('[data-share-registry]').click()
            expect(admin.locator('#registryShareInvitation')).to_be_enabled()
            admin.locator('#registryShareInvitation').select_option(invitation_id)
            expect(admin.locator('#registryShareURL')).to_have_value(entry)
            admin.screenshot(path='/tmp/project-alpha-registry-share-desktop.png', full_page=True)
            admin.locator('#registryShareClose').click()
            admin.set_viewport_size(dict(width=390, height=844))
            assert admin.evaluate('document.documentElement.scrollWidth <= innerWidth')
            expect(card.locator('[data-edit-node]')).to_be_visible()
            page.goto(entry)
            expect(page.locator('#registration')).to_be_visible()
            expect(page.locator('#fields label').last).to_contain_text('<img')
            assert page.locator('#fields img').count() == 0
            page.locator('[name=username]').fill('alice')
            page.locator('[name=password]').fill('Member-password-123')
            page.locator('[name=password_confirm]').fill('Different-password-123')
            page.locator('[name=ssh_public_key]').fill(ssh_key)
            page.locator('[data-key=group]').select_option('A组')
            page.locator('#submit').click()
            expect(page.locator('#errorDialog')).to_be_visible()
            expect(page.locator('#errorMessage')).to_contain_text('两次输入的密码不一致')
            page.locator('#errorDialog button').click()
            expect(page.locator('[name=password_confirm]')).to_be_focused()
            page.locator('[name=password_confirm]').fill('Member-password-123')
            page.locator('#submit').click()
            expect(page.locator('#progress')).to_be_visible()
            expect(page.locator('#retry')).to_be_visible()
            expect(page.locator('#share')).to_be_hidden()
            expect(page.locator('#steps')).to_contain_text('暂无可分配')
            expect(page.locator('#errorDialog')).to_be_visible()
            expect(page.locator('#errorMessage')).to_contain_text('暂无可分配')
            page.locator('#errorDialog button').click()
            member_id = sql('control', 'SELECT id FROM members WHERE username=?', ('alice',))[0][0]
            for _ in range(100):
                if sql('control', 'SELECT pending FROM member_work WHERE member_id=?', (member_id,))[0][0] == 0:
                    break
                time.sleep(.05)
            # Supply the result an external resource provisioner would persist.
            share_url = 'https://login.tailscale.com/admin/invite/browser-test'
            sql('control', "INSERT INTO bastion_tailscale VALUES('browser-share','Share',1,'100.64.0.2',22,9765,'http://10.0.0.1:8765')")
            sql('control', "UPDATE member_access SET tailscale_id='browser-share',invite_id='browser',invite_url=?,invite_state='invited',key_state='ready',error='' WHERE member_id=?", (share_url, member_id))
            expect(page.locator('#share')).to_be_visible()
            expect(page.locator('#shareLink')).to_have_attribute('href', share_url)
            expect(page.locator('#percent')).to_have_text('100%')
            expect(page.locator('#controlGuide')).to_be_visible()
            expect(page.locator('#controlStatusLink')).to_have_attribute('href', 'http://100.64.0.2:9765/status/alice')
            expect(page.locator('#controlGuide')).to_contain_text('注册时设置的密码')
            # The embedded tutorial image must be allowed by the real registry CSP
            # and served through the authenticated registration asset route.
            page.get_by_text('接受成功后，页面是什么样的？', exact=True).click()
            tutorial = page.locator('#share .guide-figure img')
            tutorial.scroll_into_view_if_needed()
            page.wait_for_function('(img) => img.complete && img.naturalWidth > 0', arg=tutorial.element_handle())
            image_response = context.request.get(page.url.rstrip('/') + '/guide/tailscale-shared-machine.jpg')
            assert image_response.status == 200
            assert image_response.headers['content-type'] == 'image/jpeg'
            expect(page.locator('#proxyFix')).to_be_visible()
            expect(page.locator('#proxyBypassHost')).to_have_text('100.64.0.2')
            for name in ['clash-settings.png', 'clash-bypass.png']:
                screenshot = page.locator(f'#proxyFix img[src$="/{name}"]')
                screenshot.scroll_into_view_if_needed()
                page.wait_for_function('(img) => img.complete && img.naturalWidth > 0', arg=screenshot.element_handle())
                response = context.request.get(page.url.rstrip('/') + '/guide/' + name)
                assert response.status == 200
                assert response.headers['content-type'] == 'image/png'
            context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=registry_url)
            page.evaluate("() => { navigator.clipboard.writeText = async () => { throw new DOMException('Denied', 'NotAllowedError'); }; }")
            page.locator('#copy').click()
            expect(page.locator('#message')).to_contain_text('分享链接已复制')
            assert page.evaluate('navigator.clipboard.readText()') == page.locator('#shareLink').get_attribute('href')
            page.locator('#copyControl').click()
            expect(page.locator('#message')).to_contain_text('总控地址已复制')
            assert page.evaluate('navigator.clipboard.readText()') == 'http://100.64.0.2:9765/status/alice'
            page.locator('#copyProxyBypass').click()
            expect(page.locator('#proxyCopyStatus')).to_contain_text('绕过地址已复制')
            assert page.evaluate('navigator.clipboard.readText()') == '100.64.0.2'
            page.locator('#proxyFix').screenshot(path='/tmp/project-alpha-proxy-fix-desktop.png')
            page.set_viewport_size(dict(width=390, height=844))
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
            page.locator('#proxyFix').screenshot(path='/tmp/project-alpha-proxy-fix-mobile.png')
            page.set_viewport_size(dict(width=1280, height=1000))
            assert page.locator('#memberResourceToken').count() == 0
            assert sql('registry', 'SELECT registration FROM registry_sessions')[0][0] == ''
            expect(page.locator('#retry')).to_be_hidden()
            page.reload()
            expect(page.locator('#share')).to_be_visible()
            expect(page.locator('#registration')).to_be_hidden()
            expect(page.locator('#controlGuide')).to_be_visible()
            assert sql('control', 'SELECT count(*) FROM members')[0][0] == 1
            assert sql('control', 'SELECT used FROM member_invitations')[0][0] == 1
            status_page = context.new_page()
            status_page.goto(control_url + '/status/alice')
            status_page.locator('#loginPassword').fill('wrong-password')
            status_page.locator('#loginSubmit').click()
            expect(status_page.locator('#statusError')).to_contain_text('用户名或密码错误')
            status_page.locator('#loginPassword').fill('Member-password-123')
            status_page.locator('#loginSubmit').click()
            expect(status_page.locator('#statusContent')).to_be_visible()
            status_page.locator('summary', has_text='修改密码').click()
            status_page.locator('#currentPassword').fill('Member-password-123')
            status_page.locator('#newMemberPassword').fill('Changed-password-123')
            status_page.locator('#confirmMemberPassword').fill('Different-password-123')
            status_page.locator('#passwordSubmit').click()
            expect(status_page.locator('#passwordError')).to_contain_text('两次输入的密码不一致')
            status_page.locator('#confirmMemberPassword').fill('Changed-password-123')
            status_page.locator('#passwordSubmit').click()
            expect(status_page.locator('#loginPanel')).to_be_visible()
            expect(status_page.locator('#statusError')).to_contain_text('密码已修改')
            status_page.locator('#loginPassword').fill('Member-password-123')
            status_page.locator('#loginSubmit').click()
            expect(status_page.locator('#statusError')).to_contain_text('用户名或密码错误')
            status_page.locator('#loginPassword').fill('Changed-password-123')
            status_page.locator('#loginSubmit').click()
            expect(status_page.locator('#statusContent')).to_be_visible()
            status_page.reload()
            expect(status_page.locator('#statusContent')).to_be_visible()
            status_page.locator('#logout').click()
            expect(status_page.locator('#loginPanel')).to_be_visible()
            assert status_page.evaluate('sessionStorage.length') == 0
            status_page.close()
            card.locator('[data-share-registry]').click()
            expect(admin.locator('#registryShareHint')).to_contain_text('暂无可用邀请码')
            assert admin.locator('#registryShareInvitation option').count() == 1
            admin.locator('#registryShareClose').click()

            # Responses arriving after a dialog closes must not reveal a link.
            extra = context.request.post(control_url + '/admin/member-invitations/create',
                form=dict(label='stale response', quota='1'), headers={'X-CSRF-Token': admin.evaluate('platform.csrf')})
            assert extra.status == 200
            extra_id = re.search(r'name="id" value="([a-f0-9]{32})"', extra.text()).group(1)
            # Await the same API promise as the UI, after its handler has resumed.
            admin.evaluate('''() => {
                window.shareTestAPI = api;
                api = (path, options) => {
                    const result = window.shareTestAPI(path, options);
                    if (path.endsWith('/registration-link')) window.shareTestRequest = result;
                    return result;
                };
            }''')
            for action in ['close', 'logout']:
                pending = []
                admin.route('**/registration-link', lambda route: pending.append(route))
                card.locator('[data-share-registry]').click()
                expect(admin.locator('#registryShareInvitation')).to_be_enabled()
                with admin.expect_request('**/registration-link'):
                    admin.locator('#registryShareInvitation').select_option(extra_id)
                if action == 'close':
                    admin.locator('#registryShareClose').click()
                else:
                    admin.evaluate("document.getElementById('logoutButton').click()")
                    expect(admin.locator('#authPanel')).to_be_visible()
                assert len(pending) == 1
                with admin.expect_response('**/registration-link'):
                    pending.pop().fulfill(status=200, content_type='application/json',
                        body=json.dumps(dict(url=f'https://stale.example/{action}-secret')))
                admin.evaluate('() => window.shareTestRequest')
                expect(admin.locator('#registryShareURL')).to_have_value('')
                expect(admin.locator('#registryShareDialog')).not_to_be_visible()
                admin.unroute('**/registration-link')
            admin.evaluate('''() => {
                api = window.shareTestAPI;
                delete window.shareTestAPI;
                delete window.shareTestRequest;
            }''')
            admin.locator('#authUsername').fill('operator')
            admin.locator('#authPassword').fill('A-test-password-123')
            admin.locator('#authSubmit').click()
            expect(card.locator('.node-status')).to_have_text('已连接', timeout=15000)

            # A gateway restart keeps both its binding and the existing browser session.
            registry.terminate()
            registry.wait(timeout=15)
            expect(card.locator('.node-status')).to_have_text('重连中', timeout=15000)
            expect(card.locator('[data-reconnect-node]')).to_be_enabled()
            admin.set_viewport_size(dict(width=390, height=844))
            assert admin.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'registry reconnect card overflow'
            admin.set_viewport_size(dict(width=1280, height=1000))
            registry, restarted_url = start('registry', ['--registry', '--data-dir', str(root / 'registry'), '--port', registry_url.rsplit(':', 1)[1]])
            assert restarted_url == registry_url
            with admin.expect_response(lambda response: response.url.endswith('/reconnect') and response.request.method == 'POST') as reconnected:
                card.locator('[data-reconnect-node]').click()
            assert reconnected.value.status == 202
            expect(card.locator('.node-status')).to_have_text('已连接', timeout=15000)
            expect(card.locator('[data-reconnect-node]')).to_have_count(0)
            expect(admin.locator('#page-cluster')).to_be_visible()
            page.reload()
            expect(page.locator('#share')).to_be_visible(timeout=15000)
            page.set_viewport_size(dict(width=390, height=844))
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
            expect(page.locator('#shareLink')).to_be_visible()
            card.locator('[data-remove-node]').click()
            expect(admin.locator('#nodeRemoveDescription')).to_contain_text('断开注册连接')
            admin.locator('#nodeRemoveConfirm').click()
            expect(admin.locator('#nodeRemoveDialog')).not_to_be_visible()
            expect(admin.locator('.node-card')).to_have_count(0)
            assert sql('control', 'SELECT count(*) FROM cluster_nodes')[0][0] == 0
            assert sql('registry', 'SELECT count(*) FROM registry_control')[0][0] == 1
            assert not errors, errors
            browser.close()
        print('Registry browser checks passed: share links, invitation shortcut, clipboard, unavailable and exhausted invitations, stale close/logout responses, typed nodes, credentials, restarts, registration progress, mobile layout and removal.')
    finally:
        for process in reversed(services):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        for log in logs:
            log.close()
