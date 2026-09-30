"""Member registration UI and public API against an isolated, real Go service."""
import os
import json
import re
import subprocess
import tempfile
import time
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='alpha-members-') as temporary:
    root = Path(temporary)
    binary = root / 'project-alpha'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=repo, check=True)
    with (root / 'service.log').open('w+') as log:
        service = subprocess.Popen([str(binary), 'serve', '--data-dir', str(root / 'data'),
                                    '--port', '0', '--tetragon-socket', str(root / 'missing.sock')],
                                   cwd=repo, stdout=log, stderr=log)
        try:
            url = None
            for _ in range(150):
                log.seek(0)
                match = re.search(r'project alpha: (http://127\.0\.0\.1:\d+)', log.read())
                if match:
                    url = match[1]
                    break
                if service.poll() is not None:
                    break
                time.sleep(.1)
            if not url:
                log.seek(0)
                raise AssertionError(log.read())
            with sync_playwright() as p:
                launch = dict(headless=True, args=['--no-sandbox'])
                if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
                    launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
                browser = p.chromium.launch(**launch)
                context = browser.new_context(viewport=dict(width=1440, height=1080), reduced_motion='reduce')
                page = context.new_page()
                errors = []
                page.on('pageerror', lambda error: errors.append(str(error)))
                public = p.request.new_context(base_url=url)
                page.goto(url)
                page.locator('#authUsername').fill('operator')
                page.locator('#authPassword').fill('A-test-password-123')
                page.locator('#authSubmit').click()
                page.locator('.platform-nav [data-page="members"]').click()
                expect(page.locator('#memberSchemaEditor')).to_be_enabled()
                assert '暂无使用者' in page.locator('#membersBody').inner_text()
                page.locator('#memberExampleFields').click()
                assert page.locator('[data-member-field]').count() == 3
                page.locator('#memberAddField').click()
                page.locator('[data-member-field="3"] [data-field-key]').fill('note')
                page.locator('[data-member-field="3"] [data-field-label]').fill('备注')
                page.locator('[data-member-field="3"] [data-field-type]').select_option('select')
                page.locator('[data-member-field="3"] [data-field-options]').fill('是\n否')
                assert '备注' in page.locator('#memberSchemaPreview').inner_text()
                page.locator('[data-remove-field="3"]').click()
                page.locator('#memberSaveSchema').click()
                expect(page.locator('#memberSchemaStatus')).to_contain_text('版本 2')
                schema = public.get('/api/members/registration-schema').json()
                assert schema['fields'][1]['options'] == ['博士', '硕士']
                page.locator('#memberInvitationLabel').fill('A组入组')
                page.locator('#memberInvitationQuota').fill('2')
                page.locator('#memberCreateInvitation').click()
                page.locator('#memberInvitationDialog').wait_for(state='visible')
                code = page.locator('#memberInvitationCode').input_value()
                assert len(code) == 48
                page.locator('#memberInvitationClose').click()
                assert page.locator('#memberInvitationCode').input_value() == ''
                payload = dict(username='alice', invitation_code=code, ssh_public_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f', schema_revision=schema['revision'],
                               profile=dict(full_name='<img src=x onerror=alert(1)>', degree='博士', group='A组'))
                registered = public.post('/api/members/register', data=payload)
                assert registered.status == 201, registered.text()
                member_token = registered.json()['resource_token']
                member_id = registered.json()['id']
                mine = public.get('/api/members/me/resources', headers={'Authorization': 'Bearer ' + member_token})
                assert mine.status == 200 and mine.json()['member_id'] == member_id
                assert public.get('/api/members/me/resources').status == 401
                assert 'set-cookie' not in registered.headers
                assert public.get('/api/members').status == 401
                assert public.post('/api/login', data=dict(username='alice', password='A-test-password-123')).status == 401
                assert public.post('/api/members/register', data=payload).status == 409
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersBody')).to_contain_text('alice')
                assert page.locator('#membersBody img').count() == 0
                assert '<img' in page.locator('#membersBody').inner_text()
                assert '1 / 2' in page.locator('#memberInvitationsBody').inner_text()
                payload['username'] = 'bob'
                assert public.post('/api/members/register', data=payload).status == 201
                payload['username'] = 'charlie'
                assert public.post('/api/members/register', data=payload).status == 400
                page.locator('#membersRefresh').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('名额已用尽')
                assert page.locator('[data-revoke-invitation]').count() == 0
                # Refresh lists preserves unsaved schema edits; concurrent saves are rejected.
                page.locator('[data-field-label]').first.fill('姓名草稿')
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                assert page.locator('[data-field-label]').first.input_value() == '姓名草稿'
                csrf = page.evaluate('platform.csrf')
                schema['fields'][0]['label'] = '姓名新版'
                response = context.request.put(url + '/api/members/registration-schema', data=schema,
                                               headers={'X-CSRF-Token': csrf})
                assert response.status == 200
                page.locator('#memberSaveSchema').click()
                expect(page.locator('#membersError')).to_contain_text('重新载入')
                assert page.locator('[data-field-label]').first.input_value() == '姓名草稿'
                page.locator('#memberReloadSchema').click()
                expect(page.locator('#memberSchemaStatus')).to_contain_text('版本 3')
                assert page.locator('[data-field-label]').first.input_value() == '姓名新版'
                assert '姓名：' in page.locator('#membersBody').inner_text(), 'existing profiles retain old labels'
                page.locator('#memberInvitationLabel').fill('撤销测试')
                page.locator('#memberInvitationQuota').fill('1')
                page.locator('#memberCreateInvitation').click()
                page.locator('#memberInvitationDialog').wait_for(state='visible')
                revoked_code = page.locator('#memberInvitationCode').input_value()
                page.locator('#memberInvitationClose').click()
                page.locator('[data-revoke-invitation] button').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('已作废')
                payload.update(invitation_code=revoked_code, schema_revision=3)
                assert public.post('/api/members/register', data=payload).status == 400
                page.screenshot(path='/tmp/project-alpha-members-desktop.png', full_page=True)
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'mobile horizontal overflow'
                page.screenshot(path='/tmp/project-alpha-members-mobile.png', full_page=True)
                page.set_viewport_size(dict(width=1440, height=1080))
                # Control-only access management, using the real local API without
                # provisioning any real host account or contacting Tailscale.
                page.locator('.platform-nav [data-page="bastion"]').click()
                expect(page.locator('#page-bastion')).to_be_visible()
                expect(page.locator('#bastionSettingsFields')).to_be_enabled()
                expect(page.locator('#bastionAssignments')).to_contain_text('alice')
                page.locator('#bastionToken').fill('tskey-api-browser-test')
                page.locator('#bastionSettingsForm .primary').click()
                expect(page.locator('#bastionStatus')).to_contain_text('凭据已保存')
                assert page.locator('#bastionToken').input_value() == ''
                assert 'api_token' not in context.request.get(url + '/api/tailscale/settings').json()
                page.route('**/api/tailscale/devices', lambda route: route.fulfill(status=200,
                    content_type='application/json', body=json.dumps(dict(devices=[dict(nodeId='node-test',
                    hostname='<img src=x onerror=alert(1)>', addresses=['100.64.0.1'], authorized=True,
                    isExternal=False)]))))
                page.locator('#bastionDevicesRefresh').click()
                expect(page.locator('#bastionDevices')).to_contain_text('<img')
                assert page.locator('#bastionDevices img').count() == 0
                install_data = context.request.get(url + '/api/bastion/resources').json()
                assert not install_data['jump_installation']['ready']
                expect(page.locator('#bastionJumpInstallation')).to_have_text(install_data['jump_installation']['error'])
                # Exercise the password dialog without invoking host sudo.
                install_data['jump_installation']['web'] = dict(available=True, service_user='service-test', error='')
                install_data['jump_installation']['account'] = dict(exists=True, managed=False, released=False, removed=False)
                page.route('**/api/bastion/resources', lambda route: route.fulfill(status=200,
                    content_type='application/json', body=json.dumps(install_data)))
                installs = []
                page.route('**/api/bastion/install', lambda route: installs.append(route))
                page.locator('#bastionRefresh').click()
                expect(page.locator('#bastionAdopt')).to_be_visible()
                expect(page.locator('#bastionInstall')).not_to_be_visible()
                page.locator('#bastionAdopt').click()
                expect(page.locator('#bastionInstallTitle')).to_have_text('接管已有 alpha-jump')
                page.locator('#bastionInstallClose').click()
                install_data['jump_installation']['account']['exists'] = False
                with page.expect_response('**/api/bastion/resources') as refreshed:
                    page.locator('#bastionRefresh').click()
                assert not refreshed.value.json()['jump_installation']['account']['exists']
                expect(page.locator('#bastionRefresh')).to_be_enabled()
                expect(page.locator('#bastionError')).to_have_text('')
                assert not errors, errors
                expect(page.locator('#bastionInstall')).to_be_visible()
                expect(page.locator('#bastionInstall')).to_be_enabled()
                page.locator('#bastionInstall').click()
                expect(page.locator('#bastionInstallUser')).to_contain_text('service-test')
                page.locator('#bastionSudoPassword').fill('discard-on-close')
                page.locator('#bastionInstallClose').click()
                assert page.locator('#bastionSudoPassword').input_value() == ''
                page.locator('#bastionInstall').click()
                page.locator('#bastionSudoPassword').fill('browser-sudo-password')
                page.locator('#bastionInstallSubmit').click()
                expect(page.locator('#bastionSudoPassword')).to_have_value('')
                expect(page.locator('#bastionInstallSubmit')).to_be_disabled()
                page.wait_for_timeout(100)
                assert len(installs) == 1
                assert installs[0].request.post_data_json == dict(action='init', sudo_password='browser-sudo-password')
                installs.pop().fulfill(status=403, content_type='application/json', body=json.dumps(dict(error='sudo 认证失败')))
                expect(page.locator('#bastionInstallError')).to_contain_text('sudo 认证失败')
                expect(page.locator('#bastionInstallSubmit')).to_be_enabled()
                assert page.locator('#bastionSudoPassword').input_value() == ''
                page.locator('#bastionSudoPassword').fill('browser-sudo-retry')
                page.locator('#bastionInstallSubmit').click()
                page.wait_for_timeout(100)
                assert len(installs) == 1
                install_data['jump_installation'].update(ready=True, error='', account=dict(exists=True, managed=True, released=False, removed=False))
                installs.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(page.locator('#bastionInstallDialog')).not_to_be_visible()
                expect(page.locator('#bastionStatus')).to_contain_text('跳板安装完成')
                assert page.locator('#bastionSudoPassword').input_value() == ''
                assert 'browser-sudo' not in page.evaluate('JSON.stringify([localStorage, sessionStorage])')
                expect(page.locator('#bastionJumpInstallation')).to_have_text('已安装，可管理 alpha-jump 公钥。')
                page.locator('#bastionInstall').click()
                page.locator('#bastionSudoPassword').fill('discard-on-navigation')
                page.evaluate('BastionUI.leave()')
                expect(page.locator('#bastionInstallDialog')).not_to_be_visible()
                assert page.locator('#bastionSudoPassword').input_value() == ''
                # The pool is a superset: show free keys and clean only the
                # selected unreferenced entry; referenced rows have no cleanup.
                used_id, free_id = '1' * 32, '2' * 32
                install_data['key_pool'] = dict(error='', keys=[
                    dict(id=used_id, public_key='ssh-ed25519 used-key', fingerprint='SHA256:used',
                         state='used', members=[dict(id=member_id, username='alice', status='active')]),
                    dict(id=free_id, public_key='ssh-ed25519 ' + 'A' * 360, fingerprint='SHA256:free',
                         state='free', members=[])])
                with page.expect_response('**/api/bastion/resources'):
                    page.locator('#bastionRefresh').click()
                expect(page.locator('#bastionKeyPool')).to_contain_text('free · 未关联用户')
                expect(page.locator('#bastionKeyPool')).to_contain_text('alice')
                assert page.locator('[data-clean-key]').count() == 1
                assert page.locator('[data-clean-key="' + used_id + '"]').count() == 0
                page.locator('#bastionKeyPool details').last.locator('summary').click()
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'pool key mobile overflow'
                page.set_viewport_size(dict(width=1440, height=1080))
                cleanups = []
                page.route('**/api/bastion/keys/' + free_id, lambda route: cleanups.append(route))
                page.locator('[data-clean-key="' + free_id + '"]').click()
                page.wait_for_timeout(100)
                assert cleanups[0].request.method == 'DELETE'
                cleanups.pop().fulfill(status=409, content_type='application/json',
                    body=json.dumps(dict(error='该公钥已有使用者关联，不能按 free 清理')))
                expect(page.locator('#bastionError')).to_contain_text('已有使用者关联')
                expect(page.locator('[data-clean-key="' + free_id + '"]')).to_be_enabled()
                page.locator('[data-clean-key="' + free_id + '"]').click()
                page.wait_for_timeout(100)
                install_data['key_pool']['keys'].pop()
                cleanups.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(page.locator('#bastionStatus')).to_have_text('已清理 free 公钥。')
                expect(page.locator('[data-clean-key]')).to_have_count(0)
                syncs = []
                page.route('**/api/bastion/keys/sync', lambda route: syncs.append(route))
                page.locator('#bastionKeySync').click()
                page.wait_for_timeout(100)
                assert syncs[0].request.method == 'POST' and syncs[0].request.post_data_json == {}
                syncs.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(page.locator('#bastionStatus')).to_contain_text('已补齐用户公钥')
                expect(page.locator('#bastionRefresh')).to_be_enabled()
                page.unroute('**/api/bastion/keys/' + free_id)
                page.unroute('**/api/bastion/keys/sync')
                # Account lifecycle choices share the one-shot sudo dialog.
                for action, button in [('release', '#bastionRelease'), ('adopt', '#bastionAdopt'),
                                       ('delete', '#bastionRemoveAccount')]:
                    page.locator(button).click()
                    expect(page.locator('#bastionInstallDescription')).to_contain_text('data')
                    page.locator('#bastionSudoPassword').fill('account-action-password')
                    if action == 'delete':
                        page.locator('#bastionInstallSubmit').click()
                        assert not installs, 'delete did not require account confirmation'
                        page.locator('#bastionAccountConfirm').fill('alpha-jump')
                    page.locator('#bastionInstallSubmit').click()
                    expect(page.locator('#bastionSudoPassword')).to_have_value('')
                    page.wait_for_timeout(100)
                    assert len(installs) == 1
                    payload = dict(action=action, sudo_password='account-action-password')
                    if action == 'delete':
                        payload['confirm'] = 'alpha-jump'
                    assert installs[0].request.post_data_json == payload
                    install_data['jump_installation'].update(ready=action == 'adopt',
                        error='' if action == 'adopt' else '账号已停止管理',
                        account=dict(exists=action != 'delete', managed=action == 'adopt',
                                     released=action == 'release', removed=action == 'delete'))
                    installs.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                    expect(page.locator('#bastionInstallDialog')).not_to_be_visible()
                    expect(page.locator('#bastionRefresh')).to_be_enabled()
                    if action == 'release':
                        expect(page.locator('#bastionAdopt')).to_be_visible()
                        expect(page.locator('#bastionInstall')).not_to_be_visible()
                expect(page.locator('#bastionInstall')).to_have_text('添加账号')
                expect(page.locator('#bastionInstall')).to_be_visible()
                assert 'account-action-password' not in page.evaluate('JSON.stringify([localStorage, sessionStorage])')
                page.unroute('**/api/bastion/resources')
                page.unroute('**/api/bastion/install')
                page.locator(f'[data-member="{member_id}"]').click()
                expect(page.locator('#bastionMemberDialog')).to_be_visible()
                expect(page.locator('#bastionMemberContent')).to_contain_text('alpha-jump')
                page.locator('#bastionMemberClose').click()
                page.screenshot(path='/tmp/project-alpha-bastion-desktop.png', full_page=True)
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'bastion mobile overflow'
                page.screenshot(path='/tmp/project-alpha-bastion-mobile.png', full_page=True)
                page.set_viewport_size(dict(width=1440, height=1080))
                page.locator(f'[data-member="{member_id}"]').click()
                page.locator('#bastionDeleteConfirm').fill('alice')
                page.locator('#bastionDeleteForm button').click()
                expect(page.locator('#bastionMemberDialog')).not_to_be_visible()
                for _ in range(100):
                    response = public.get('/api/members/me/resources', headers={'Authorization': 'Bearer ' + member_token})
                    if response.status == 401:
                        break
                    page.wait_for_timeout(50)
                assert response.status == 401
                page.locator('.platform-nav [data-page="members"]').click()
                # An invitation response arriving after logout must not expose its code.
                pending = []
                page.route('**/admin/member-invitations/create', lambda route: pending.append(route)
                           if route.request.method == 'POST' else route.continue_())
                page.locator('#memberCreateInvitation').click()
                page.wait_for_timeout(100)
                assert len(pending) == 1
                page.locator('#logoutButton').click()
                expect(page.locator('#authPanel')).to_be_visible()
                pending[0].fulfill(status=200, content_type='text/html',
                                   body='<input data-issued-invitation value="stale-secret">')
                page.wait_for_timeout(100)
                assert page.locator('#memberInvitationDialog').is_hidden()
                assert page.locator('#memberInvitationCode').input_value() == ''
                assert page.locator('#membersBody').inner_text() == ''
                assert not errors, errors
                public.dispose()
                browser.close()
            print('Member browser checks passed: real API, schema editor, quota, independent identity, escaping, schema conflicts, revocation, mobile layout and stale logout responses.')
        finally:
            service.terminate()
            try:
                service.wait(timeout=10)
            except subprocess.TimeoutExpired:
                service.kill()
                service.wait()
