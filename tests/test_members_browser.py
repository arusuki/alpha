"""Member registration UI and public API against an isolated, real Go service."""
import os
import json
import re
import sqlite3
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
                expect(page.locator('#membersBody')).to_contain_text('暂无使用者')
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
                context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=url)
                page.evaluate("() => { navigator.clipboard.writeText = async () => { throw new DOMException('Denied', 'NotAllowedError'); }; }")
                page.locator('#memberInvitationCopy').click()
                expect(page.locator('#memberInvitationCopyStatus')).to_have_text('已复制。')
                assert page.evaluate('navigator.clipboard.readText()') == code
                page.locator('#memberInvitationClose').click()
                assert page.locator('#memberInvitationCode').input_value() == ''
                invitation_label = page.locator('[data-update-invitation] input[name="label"]')
                invitation_display = page.locator('[data-edit-invitation]')
                expect(invitation_label).to_have_value('A组入组')
                expect(invitation_label).to_be_hidden()
                expect(invitation_display).to_have_text('A组入组')
                assert page.locator('[data-update-invitation] button').count() == 0
                updates = []
                page.on('request', lambda request: updates.append(request.post_data)
                        if request.url.endswith('/admin/member-invitations/update') else None)
                invitation_display.click()
                expect(invitation_label).to_be_focused()
                page.locator('#memberInvitationsPanel th').first.click()
                expect(invitation_label).to_be_hidden()
                invitation_display.focus()
                invitation_display.press('Enter')
                expect(invitation_label).to_be_focused()
                invitation_label.fill('未保存的修改')
                invitation_label.press('Escape')
                expect(invitation_label).to_be_hidden()
                expect(invitation_display).to_be_focused()
                expect(invitation_display).to_have_text('A组入组')
                assert not updates, 'unchanged or cancelled edits must not submit'
                invitation_display.click()
                invitation_label.fill('  新备注 <img src=x onerror=alert(1)> "  ')
                # A failed save retains the draft for retry.
                page.route('**/admin/member-invitations/update', lambda route: route.fulfill(
                    status=500, content_type='application/json', body='{"error":"备注保存失败"}'))
                invitation_label.press('Enter')
                expect(page.locator('#membersError')).to_have_text('备注保存失败')
                expect(invitation_label).to_be_visible()
                expect(invitation_label).to_have_value('  新备注 <img src=x onerror=alert(1)> "  ')
                page.unroute('**/admin/member-invitations/update')
                page.locator('#memberInvitationsPanel th').first.click()
                expect(page.locator('#membersStatus')).to_have_text('邀请码备注已保存。')
                expect(invitation_label).to_be_hidden()
                expect(invitation_display).to_have_text('新备注 <img src=x onerror=alert(1)> "')
                expect(invitation_label).to_have_value('新备注 <img src=x onerror=alert(1)> "')
                assert len(updates) == 2, 'one failed save and one retry'
                assert page.locator('#memberInvitationsPanel img').count() == 0
                assert page.locator('#memberInvitationDialog').is_hidden()
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                expect(invitation_label).to_have_value('新备注 <img src=x onerror=alert(1)> "')
                expect(invitation_label).to_be_hidden()
                expect(invitation_display).to_have_text('新备注 <img src=x onerror=alert(1)> "')
                payload = dict(username='alice', password='Member-password-123', invitation_code=code, ssh_public_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f', schema_revision=schema['revision'],
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
                expect(page.locator('#membersBody a', has_text='使用者状态页')).to_have_attribute('href', '/status/alice')
                assert page.locator('#membersBody img').count() == 0
                assert '<img' in page.locator('#membersBody').inner_text()
                assert '1 / 2' in page.locator('#memberInvitationsBody').inner_text()
                # Reset from account management and verify actual session revocation.
                old_session = public.post('/api/status/alice/login', data={'password': 'Member-password-123'}).json()['session_token']
                page.locator(f'#membersBody [data-reset-member="{member_id}"]').click()
                expect(page.locator('#memberPasswordDialog')).to_be_visible()
                page.locator('#memberPassword').fill('Reset-password-456')
                page.locator('#memberPasswordConfirm').fill('Different-password-456')
                page.locator('#memberPasswordSubmit').click()
                expect(page.locator('#memberPasswordError')).to_contain_text('两次输入')
                page.locator('#memberPasswordConfirm').fill('Reset-password-456')
                page.locator('#memberPasswordSubmit').click()
                expect(page.locator('#memberPasswordStatus')).to_contain_text('密码重置完成')
                expect(page.locator('#memberPassword')).to_have_value('')
                expect(page.locator('#memberPasswordConfirm')).to_have_value('')
                assert public.get('/api/status/alice', headers={'Authorization': 'Bearer ' + old_session}).status == 401
                assert public.post('/api/status/alice/login', data={'password': 'Member-password-123'}).status == 401
                assert public.post('/api/status/alice/login', data={'password': 'Reset-password-456'}).status == 200
                page.locator('#memberPasswordCancel').click()
                expect(page.locator('#memberPasswordDialog')).not_to_be_visible()
                # The container list shares the reset dialog. Exercise partial failure
                # and retry without real Docker containers or external nodes.
                page.locator('.platform-nav [data-page="allocations"]').click()
                page.set_viewport_size(dict(width=390, height=844))
                reset_button = page.locator(f'#allocationRows [data-reset-member="{member_id}"]')
                expect(reset_button).to_be_visible()
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth')
                reset_button.click()
                expect(page.locator('#memberPasswordDialog')).to_be_visible()
                page.locator('#memberPassword').fill('Reset-password-789')
                page.locator('#memberPasswordConfirm').fill('Reset-password-789')
                page.route('**/api/members/' + member_id + '/password', lambda route: route.fulfill(
                    content_type='application/json', body=json.dumps(dict(ok=False, account_reset=True, updated=1,
                    nodes=[dict(node_name='GPU 02', ok=False, errors=['容器已停止，请启动后重试'])]))), times=1)
                page.locator('#memberPasswordSubmit').click()
                expect(page.locator('#memberPasswordStatus')).to_contain_text('账号密码已重置')
                expect(page.locator('#memberPasswordError')).to_contain_text('GPU 02：容器已停止')
                expect(page.locator('#memberPassword')).to_have_value('Reset-password-789')
                expect(page.locator('#memberPasswordSubmit')).to_be_enabled()
                page.locator('#memberPasswordSubmit').click()
                expect(page.locator('#memberPasswordStatus')).to_contain_text('密码重置完成')
                expect(page.locator('#memberPasswordError')).to_be_empty()
                page.screenshot(path='/tmp/project-alpha-member-password-mobile.png', full_page=True)
                page.locator('#memberPasswordCancel').click()
                reset_button.click()
                expect(page.locator('#memberPassword')).to_have_value('')
                expect(page.locator('#memberPasswordStatus')).to_be_empty()
                page.locator('#memberPassword').fill('Cancelled-password-123')
                page.locator('#memberPasswordDialog').press('Escape')
                expect(page.locator('#memberPassword')).to_have_value('')
                page.set_viewport_size(dict(width=1440, height=1080))
                page.locator('.platform-nav [data-page="members"]').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                payload['username'] = 'bob'
                assert public.post('/api/members/register', data=payload).status == 201
                payload['username'] = 'charlie'
                assert public.post('/api/members/register', data=payload).status == 400
                page.locator('#membersRefresh').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('名额已用尽')
                assert page.locator('[data-revoke-invitation]').count() == 0
                invitation_display.click()
                invitation_label.fill('')
                invitation_label.press('Enter')
                expect(invitation_display).to_have_text('未填写备注')
                expect(invitation_label).to_be_hidden()
                expect(invitation_display).to_be_enabled()
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                expect(invitation_label).to_have_value('')
                expect(page.locator('#memberInvitationsBody')).to_contain_text('名额已用尽')
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
                revoked_row = page.locator('#memberInvitationsBody tr').filter(has_text='已作废')
                revoked_row.locator('[data-edit-invitation]').click()
                revoked_row.locator('input[name="label"]').fill('已作废的备注')
                page.locator('#memberInvitationsPanel th').first.click()
                expect(revoked_row.locator('[data-edit-invitation]')).to_have_text('已作废的备注')
                expect(revoked_row.locator('input[name="label"]')).to_be_hidden()
                expect(revoked_row.locator('input[name="label"]')).to_have_value('已作废的备注')
                expect(page.locator('#membersStatus')).to_have_text('邀请码备注已保存。')
                payload.update(invitation_code=revoked_code, schema_revision=3)
                assert public.post('/api/members/register', data=payload).status == 400
                page.screenshot(path='/tmp/project-alpha-members-desktop.png', full_page=True)
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'mobile horizontal overflow'
                page.screenshot(path='/tmp/project-alpha-members-mobile.png', full_page=True)
                page.set_viewport_size(dict(width=1440, height=1080))
                # Delete invitations in every state through the real admin form.
                exhausted_row = page.locator('#memberInvitationsBody tr').filter(has_text='名额已用尽')
                exhausted_row.locator('[data-delete-invitation] button').click()
                expect(exhausted_row).to_have_count(0)
                expect(page.locator('#membersStatus')).to_have_text('邀请码已删除，已登记的使用者不受影响。')
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                expect(page.locator('#memberInvitationsBody tr')).to_have_count(1)
                expect(page.locator('#membersBody')).to_contain_text('alice')
                expect(page.locator('#membersBody')).to_contain_text('bob')
                mine = public.get('/api/members/me/resources', headers={'Authorization': 'Bearer ' + member_token})
                assert mine.status == 200 and mine.json()['member_id'] == member_id
                revoked_row.locator('[data-delete-invitation] button').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('尚未生成邀请码')
                expect(page.locator('[data-delete-invitation]')).to_have_count(0)
                page.locator('#memberInvitationLabel').fill('删除可用的邀请码')
                page.locator('#memberInvitationQuota').fill('2')
                page.locator('#memberCreateInvitation').click()
                page.locator('#memberInvitationDialog').wait_for(state='visible')
                deleted_code = page.locator('#memberInvitationCode').input_value()
                page.locator('#memberInvitationClose').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('可使用')
                page.locator('[data-delete-invitation] button').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('尚未生成邀请码')
                payload.update(invitation_code=deleted_code)
                assert public.post('/api/members/register', data=payload).status == 400
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersRefresh')).to_be_enabled()
                expect(page.locator('[data-delete-invitation]')).to_have_count(0)
                # Control-only access management, using the real local API without
                # provisioning any real host account or contacting Tailscale.
                page.locator('.platform-nav [data-page="bastion"]').click()
                expect(page.locator('#page-bastion')).to_be_visible()
                expect(page.locator('#bastionSettingsFields')).to_be_enabled()
                expect(page.locator('#bastionAssignments')).to_contain_text('alice')
                expect(page.locator('#bastionAssignments')).to_contain_text('尚未分配 share node')
                expect(page.locator('#bastionSSHFields')).to_be_enabled()
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('尚未启用')
                assert context.request.get(url + '/api/bastion/ssh').json()['identity_file'] == ''
                expect(page.locator('#bastionSSHForm')).not_to_be_visible()
                page.locator('#bastionGlobalSettings > summary').click()
                # Mock cryptographic commands: browser coverage never creates test keys.
                ssh_cfg = context.request.get(url + '/api/bastion/ssh').json()
                identity = dict(identity_file=ssh_cfg['key_directory'] + '/' + 'long-name-' * 12 + '/id_ed25519',
                                public_key='ssh-ed25519 ' + 'A' * 360, fingerprint='SHA256:browser-example')
                ssh_saves, generations = [], []
                page.route('**/api/bastion/ssh', lambda route: ssh_saves.append(route)
                           if route.request.method == 'PUT' else route.fulfill(status=200,
                           content_type='application/json', body=json.dumps(ssh_cfg)))
                page.route('**/api/bastion/ssh/generate', lambda route: generations.append(route))
                page.locator('#bastionSSHName').fill('browser-control')
                page.locator('#bastionSSHGenerate').click()
                expect(page.locator('#bastionSSHGenerate')).to_be_disabled()
                page.wait_for_timeout(100)
                assert generations[0].request.post_data_json == dict(name='browser-control')
                ssh_cfg['identities'].append(identity['identity_file'])
                generations.pop().fulfill(status=201, content_type='application/json', body=json.dumps(identity))
                expect(page.locator('[data-select-identity][aria-pressed="true"]')).to_have_count(1)
                expect(page.locator('#bastionSSHStatus')).to_contain_text('已持久保存')
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('尚未启用')
                with page.expect_download() as exported:
                    page.locator('#bastionSSHDownload').click()
                assert exported.value.suggested_filename == 'control-service.pub'
                assert Path(exported.value.path()).read_text() == identity['public_key'] + '\n'
                # A full page reload must still expose the saved, inactive key.
                page.route('**/api/bastion/ssh/public-key?*', lambda route: route.fulfill(status=200,
                    content_type='application/json', body=json.dumps(identity)))
                page.reload()
                page.locator('.platform-nav [data-page="bastion"]').click()
                expect(page.locator('#bastionSSHFields')).to_be_enabled()
                page.locator('#bastionGlobalSettings > summary').click()
                expect(page.locator('#bastionSSHIdentity')).to_have_count(0)
                saved_key = page.locator('#bastionSSHInventory .bastion-key-card').filter(has_text='long-name-')
                expect(saved_key).to_contain_text('未启用')
                expect(saved_key).to_contain_text('Ed25519')
                saved_key.locator('[data-select-identity]').click()
                expect(page.locator('[data-select-identity][aria-pressed="true"]')).to_have_count(1)
                expect(page.locator('#bastionSSHPublicKey')).to_have_text(identity['public_key'])
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('尚未启用')
                assert not ssh_saves, 'selecting an existing key must not change the active identity'
                with page.expect_download() as restored_export:
                    page.locator('#bastionSSHDownload').click()
                assert Path(restored_export.value.path()).read_text() == identity['public_key'] + '\n'
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'control SSH mobile overflow'
                page.screenshot(path='/tmp/project-alpha-control-ssh-mobile.png', full_page=True)
                page.set_viewport_size(dict(width=1440, height=1080))
                page.locator('#bastionSSHSave').click()
                expect(page.locator('#bastionSSHSave')).to_be_disabled()
                page.wait_for_timeout(100)
                assert ssh_saves[0].request.post_data_json == dict(revision=1, identity_file=identity['identity_file'])
                ssh_saves.pop().fulfill(status=409, content_type='application/json',
                                       body=json.dumps(dict(error='分享节点校验失败，SSH 配置未保存')))
                expect(page.locator('#bastionError')).to_contain_text('校验失败')
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('尚未启用')
                page.locator('#bastionSSHSave').click()
                page.wait_for_timeout(100)
                ssh_cfg.update(identity, revision=2)
                ssh_saves.pop().fulfill(status=200, content_type='application/json', body=json.dumps(ssh_cfg))
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('版本 2')
                expect(saved_key.locator('[data-delete-identity]')).to_be_disabled()
                expect(page.locator('#bastionSSHSave')).to_be_disabled()
                # Create and activate a replacement before deleting the first key.
                replacement = dict(identity_file=ssh_cfg['key_directory'] + '/replacement-123/id_ed25519',
                                   public_key=identity['public_key'], fingerprint=identity['fingerprint'])
                page.locator('#bastionSSHCreate > summary').click()
                page.locator('#bastionSSHName').fill('replacement')
                page.locator('#bastionSSHGenerate').click()
                page.wait_for_timeout(100)
                ssh_cfg['identities'].append(replacement['identity_file'])
                generations.pop().fulfill(status=201, content_type='application/json', body=json.dumps(replacement))
                expect(page.locator('#bastionSSHSave')).to_be_enabled()
                page.locator('#bastionSSHSave').click()
                page.wait_for_timeout(100)
                assert ssh_saves[0].request.post_data_json == dict(revision=2, identity_file=replacement['identity_file'])
                ssh_cfg.update(replacement, revision=3)
                ssh_saves.pop().fulfill(status=200, content_type='application/json', body=json.dumps(ssh_cfg))
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('replacement-123')
                deletions = []
                page.route('**/api/bastion/ssh/identity', lambda route: deletions.append(route))
                expect(saved_key.locator('[data-delete-identity]')).to_be_enabled()
                # Cancellation sends no request. Failed deletion retains the key.
                page.once('dialog', lambda dialog: dialog.dismiss())
                saved_key.locator('[data-delete-identity]').click()
                assert not deletions
                saved_key.locator('[data-select-identity]').click()
                expect(page.locator('#bastionSSHPublic')).to_be_visible()
                page.once('dialog', lambda dialog: dialog.accept())
                saved_key.locator('[data-delete-identity]').click()
                expect(saved_key.locator('[data-delete-identity]')).to_be_disabled()
                page.wait_for_timeout(100)
                assert deletions[0].request.method == 'DELETE'
                assert deletions[0].request.post_data_json == dict(identity_file=identity['identity_file'])
                deletions.pop().fulfill(status=409, content_type='application/json', body=json.dumps(dict(error='不能删除当前使用的 SSH 身份')))
                expect(page.locator('#bastionError')).to_contain_text('不能删除当前使用')
                expect(saved_key.locator('[data-delete-identity]')).to_be_enabled()
                expect(page.locator('#bastionSSHPublic')).to_be_visible()
                page.once('dialog', lambda dialog: dialog.accept())
                saved_key.locator('[data-delete-identity]').click()
                page.wait_for_timeout(100)
                ssh_cfg['identities'].remove(identity['identity_file'])
                deletions.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(saved_key).to_have_count(0)
                expect(page.locator('#bastionSSHPublic')).to_be_visible()
                expect(page.locator('#bastionSSHCurrent')).to_contain_text('replacement-123')
                expect(page.locator('#bastionSSHStatus')).to_have_text('密钥文件已删除。')
                page.locator('#bastionRefresh').click()
                expect(page.locator('#bastionRefresh')).to_be_enabled()
                expect(saved_key).to_have_count(0)
                page.unroute('**/api/bastion/ssh/identity')
                page.unroute('**/api/bastion/ssh')
                page.unroute('**/api/bastion/ssh/generate')
                page.unroute('**/api/bastion/ssh/public-key?*')
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
                share_data = context.request.get(url + '/api/bastion/resources').json()
                assert 'jump_installation' not in share_data
                assert page.locator('#bastionSudoPassword').count() == 0
                expect(page.locator('#bastionShareSummary')).to_contain_text('先在 share node')
                expect(page.locator('#bastionInvitesRefresh')).to_be_disabled()
                page.route('**/api/bastion/resources', lambda route: route.fulfill(status=200,
                    content_type='application/json', body=json.dumps(share_data)))
                saves = []
                page.route('**/api/bastion/tailscale/node-test', lambda route: saves.append(route))
                page.locator('[data-add-node="node-test"]').click()
                expect(page.locator('#bastionNodeDialog')).to_be_visible()
                expect(page.locator('#bastionNodeHost')).to_have_value('100.64.0.1')
                page.locator('#bastionNodeSSHPort').fill('2222')
                expect(page.locator('#bastionNodeStatusPort')).to_have_count(0)
                page.locator('#bastionNodeSubmit').click()
                expect(page.locator('#bastionNodeSubmit')).to_be_disabled()
                page.wait_for_timeout(100)
                assert saves[0].request.post_data_json == dict(enabled=True, ssh_host='100.64.0.1', ssh_port=2222)
                saves.pop().fulfill(status=502, content_type='application/json', body=json.dumps(dict(error='alpha-worker SSH 认证失败')))
                expect(page.locator('#bastionNodeError')).to_contain_text('SSH 认证失败')
                expect(page.locator('#bastionNodeSubmit')).to_be_enabled()
                page.locator('#bastionNodeSubmit').click()
                page.wait_for_timeout(100)
                share_data['tailscale'] = [dict(id='node-test', name='Share node', enabled=1,
                    ssh_host='100.64.0.1', ssh_port=2222, status_port=9765, member_count=1, control_url='http://10.0.0.1:8765')]
                saves.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(page.locator('#bastionNodeDialog')).not_to_be_visible()
                expect(page.locator('#bastionStatus')).to_contain_text('已校验 alpha-worker')
                expect(page.locator('#bastionTailscalePool')).to_contain_text('总控入口 9765')
                page.locator('[data-edit-node="node-test"]').click()
                expect(page.locator('#bastionNodeSSHPort')).to_have_value('2222')
                page.locator('#bastionNodeClose').click()
                share_data['tailscale'][0]['enabled'] = 0
                with page.expect_response('**/api/bastion/resources'):
                    page.locator('#bastionRefresh').click()
                expect(page.locator('#bastionTailscalePool')).to_contain_text('已停用')
                page.locator('[data-edit-node="node-test"]').click()
                page.locator('#bastionNodeSubmit').click()
                expect(page.locator('#bastionNodeSubmit')).to_be_disabled()
                page.wait_for_timeout(100)
                assert len(saves) == 1 and saves[0].request.post_data_json['enabled'] is False
                saves.pop().fulfill(status=200, content_type='application/json', body='{"ok":true}')
                expect(page.locator('#bastionNodeDialog')).not_to_be_visible()
                expect(page.locator('#bastionTailscalePool')).to_contain_text('已停用')
                share_data['tailscale'].append(dict(id='node-second', name='Second share node', enabled=1,
                    ssh_host='100.64.0.2', ssh_port=22, status_port=8765, member_count=1, control_url='http://10.0.0.1:8765'))
                for assignment in share_data['assignments']:
                    assignment['tailscale_id'] = 'node-test' if assignment['username'] == 'alice' else 'node-second'
                    assignment['invite_id'] = 'invite-' + assignment['username']
                    assignment['invite_state'] = 'invited'
                first_node = page.locator('[data-share-node="node-test"]')
                second_node = page.locator('[data-share-node="node-second"]')
                page.locator('#bastionGlobalSettings > summary').click()
                # The pool is a superset: show free keys and clean only the
                # selected unreferenced entry; referenced rows have no cleanup.
                used_id, free_id = '1' * 32, '2' * 32
                share_data['key_pool'] = dict(error='', keys=[
                    dict(id=used_id, node_id='node-test', node_name='Share node', public_key='ssh-ed25519 used-key', fingerprint='SHA256:used',
                         state='used', members=[dict(id=member_id, username='alice', status='active')]),
                    dict(id=free_id, node_id='node-test', node_name='Share node', public_key='ssh-ed25519 ' + 'A' * 360, fingerprint='SHA256:free',
                         state='free', members=[]),
                    dict(id=used_id, node_id='node-second', node_name='Second share node', public_key='ssh-ed25519 second-key', fingerprint='SHA256:second',
                         state='used', members=[dict(id='second-member', username='bob', status='active')])])
                with page.expect_response('**/api/bastion/resources'):
                    page.locator('#bastionRefresh').click()
                expect(first_node.locator('.bastion-node-keys')).to_contain_text('free · 未关联用户')
                expect(first_node.locator('.bastion-node-keys')).to_contain_text('alice')
                expect(first_node).not_to_contain_text('bob')
                expect(second_node).to_contain_text('bob')
                expect(second_node).not_to_contain_text('alice')
                expect(second_node.locator('.bastion-node-keys')).to_contain_text('SHA256:second')
                expect(first_node.locator('.bastion-node-keys')).not_to_contain_text('SHA256:second')
                expect(first_node.locator('.bastion-assignment')).to_have_count(1)
                expect(second_node.locator('.bastion-assignment')).to_have_count(1)
                user_rows = page.locator('#bastionAssignments .bastion-assignment')
                expect(user_rows).to_have_count(2)
                expect(user_rows.filter(has_text='alice')).to_contain_text('Share node · Share node (node-test)')
                expect(user_rows.filter(has_text='bob')).to_contain_text('Share node · Second share node (node-second)')
                # Query Tailscale through the refresh endpoint, retain successful
                # updates when another member fails, and allow a later retry.
                invite_refreshes = []
                page.route('**/api/bastion/members/*/refresh', lambda route: invite_refreshes.append(route))
                page.locator('#bastionInvitesRefresh').click()
                expect(page.locator('#bastionInvitesRefresh')).to_be_disabled()
                expect(page.locator('#bastionInvitesStatus')).to_contain_text('正在查询 Tailscale')
                page.locator('#bastionInvitesRefresh').evaluate('(button) => button.click()')
                for index in range(2):
                    for _ in range(100):
                        if len(invite_refreshes) > index:
                            break
                        page.wait_for_timeout(20)
                    route = invite_refreshes[index]
                    assignment = next(a for a in share_data['assignments'] if a['member_id'] in route.request.url)
                    assert route.request.method == 'POST' and route.request.post_data_json == {}
                    if assignment['username'] == 'alice':
                        assignment['invite_state'] = 'accepted'
                        route.fulfill(status=200, content_type='application/json', body='{"ok":true}')
                    else:
                        route.fulfill(status=502, content_type='application/json', body='{"error":"Tailscale 查询失败"}')
                expect(page.locator('#bastionInvitesRefresh')).to_be_enabled()
                assert len(invite_refreshes) == 2
                expect(user_rows.filter(has_text='alice')).to_contain_text('已接受')
                expect(first_node.locator('.bastion-assignment')).to_contain_text('已接受')
                expect(user_rows.filter(has_text='bob')).to_contain_text('等待接受')
                expect(page.locator('#bastionInvitesStatus')).to_have_text('已查询 2 名使用者的邀请状态 · 成功 1 · 失败 1')
                expect(page.locator('#bastionInvitesError')).to_have_text('bob：Tailscale 查询失败')
                page.unroute('**/api/bastion/members/*/refresh')
                def refresh_invite(route):
                    assignment = next(a for a in share_data['assignments'] if a['member_id'] in route.request.url)
                    assignment['invite_state'] = 'accepted'
                    route.fulfill(status=200, content_type='application/json', body='{"ok":true}')
                page.route('**/api/bastion/members/*/refresh', refresh_invite)
                page.locator('#bastionInvitesRefresh').click()
                expect(page.locator('#bastionInvitesStatus')).to_have_text('已查询 2 名使用者的邀请状态 · 成功 2 · 失败 0')
                expect(user_rows.filter(has_text='bob')).to_contain_text('已接受')
                expect(page.locator('#bastionInvitesError')).to_be_empty()
                page.unroute('**/api/bastion/members/*/refresh')
                first_node.locator(f'[data-member="{member_id}"]').click()
                expect(page.locator('#bastionMemberDialog')).to_be_visible()
                page.locator('#bastionMemberClose').click()
                for node in (first_node, second_node):
                    expect(node.locator('.bastion-node-accounts')).to_contain_text('alpha-worker')
                    expect(node.locator('.bastion-node-accounts')).to_contain_text('alpha-jump')
                assert first_node.bounding_box()['width'] > 900
                assert abs(first_node.bounding_box()['x'] - second_node.bounding_box()['x']) < 1
                assert page.locator('[data-clean-key]').count() == 1
                assert page.locator('[data-clean-key="' + used_id + '"]').count() == 0
                # Pools collapse independently and retain per-node searches on refresh.
                first_pool = first_node.locator('.bastion-key-pool')
                second_pool = second_node.locator('.bastion-key-pool')
                expect(first_node.locator('[data-key-search]')).to_be_hidden()
                first_pool.locator(':scope > summary').click()
                search = first_node.locator('[data-key-search]')
                search.fill('  ALI  ')
                expect(first_node.locator('.bastion-node-keys .bastion-row')).to_have_count(1)
                expect(first_node.locator('.bastion-node-keys')).to_contain_text('alice')
                expect(first_node.locator('.bastion-key-results [role="status"]')).to_have_text('显示 1 / 2 条公钥')
                expect(search).to_be_focused()
                expect(second_node.locator('[data-key-search]')).to_be_hidden()
                first_pool.locator(':scope > summary').click()
                with page.expect_response('**/api/bastion/resources'):
                    page.locator('#bastionRefresh').click()
                expect(search).to_be_hidden()
                first_pool.locator(':scope > summary').focus()
                first_pool.locator(':scope > summary').press('Enter')
                expect(search).to_have_value('  ALI  ')
                expect(first_node.locator('.bastion-node-keys .bastion-row')).to_have_count(1)
                second_pool.locator(':scope > summary').click()
                expect(second_node.locator('[data-key-search]')).to_have_value('')
                expect(second_node.locator('.bastion-node-keys .bastion-row')).to_have_count(1)
                search.fill('bob')
                expect(first_node.locator('.bastion-node-keys')).to_have_text('没有匹配该用户的公钥。')
                search.fill('')
                expect(first_node.locator('.bastion-node-keys .bastion-row')).to_have_count(2)
                expect(first_node.locator('[data-clean-key]')).to_be_visible()
                with page.expect_response('**/api/bastion/resources'):
                    page.locator('#bastionRefresh').click()
                expect(search).to_be_visible()
                first_node.locator('.bastion-node-keys details').last.locator('summary').click()
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'pool key mobile overflow'
                page.set_viewport_size(dict(width=1440, height=1080))
                cleanups = []
                page.route('**/api/bastion/keys/node-test/' + free_id, lambda route: cleanups.append(route))
                page.locator('[data-clean-key="' + free_id + '"]').click()
                page.wait_for_timeout(100)
                assert cleanups[0].request.method == 'DELETE'
                cleanups.pop().fulfill(status=409, content_type='application/json',
                    body=json.dumps(dict(error='该公钥已有使用者关联，不能按 free 清理')))
                expect(page.locator('#bastionError')).to_contain_text('已有使用者关联')
                expect(page.locator('[data-clean-key="' + free_id + '"]')).to_be_enabled()
                page.locator('[data-clean-key="' + free_id + '"]').click()
                page.wait_for_timeout(100)
                share_data['key_pool']['keys'].pop(1)
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
                page.unroute('**/api/bastion/keys/node-test/' + free_id)
                page.unroute('**/api/bastion/keys/sync')
                page.unroute('**/api/bastion/resources')
                page.unroute('**/api/bastion/tailscale/node-test')
                page.locator(f'#bastionAssignments [data-member="{member_id}"]').click()
                expect(page.locator('#bastionMemberDialog')).to_be_visible()
                expect(page.locator('#bastionMemberContent')).to_contain_text('alpha-jump')
                page.locator('#bastionMemberClose').click()
                page.screenshot(path='/tmp/project-alpha-bastion-desktop.png', full_page=True)
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'bastion mobile overflow'
                page.screenshot(path='/tmp/project-alpha-bastion-mobile.png', full_page=True)
                page.set_viewport_size(dict(width=1440, height=1080))
                page.locator(f'#bastionAssignments [data-member="{member_id}"]').click()
                # This fixture never writes real alpha-jump keys or Tailscale shares.
                with sqlite3.connect(root / 'data' / 'platform.sqlite3') as db:
                    db.execute("UPDATE member_access SET invite_state='deleted',key_state='deleted'")
                page.locator('#bastionDeleteConfirm').fill('alice')
                page.locator('#bastionDeleteForm button').click()
                expect(page.locator('#bastionMemberDialog')).not_to_be_visible()
                for _ in range(100):
                    response = public.get('/api/members/me/resources', headers={'Authorization': 'Bearer ' + member_token})
                    if response.status == 401:
                        break
                    page.wait_for_timeout(50)
                assert response.status == 401
                expect(page.locator('#bastionAssignments')).not_to_contain_text('alice')
                assert context.request.get(url + '/api/members/' + member_id + '/resources').status == 404
                page.locator('.platform-nav [data-page="members"]').click()
                expect(page.locator('#membersBody')).not_to_contain_text('alice')
                page.locator('#membersBody [data-delete-member]').click()
                expect(page.locator('#memberDeleteDialog')).to_be_visible()
                page.locator('#memberDeleteConfirm').fill('wrong-user')
                page.locator('#memberDeleteSubmit').click()
                expect(page.locator('#memberDeleteError')).to_contain_text('完整使用者标识')
                page.locator('#memberDeleteConfirm').fill('bob')
                page.locator('#memberDeleteSubmit').click()
                expect(page.locator('#memberDeleteDialog')).not_to_be_visible()
                expect(page.locator('#membersBody')).to_contain_text('暂无使用者')
                expect(page.locator('#membersStatus')).to_contain_text('未归属')
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
            print('Member browser checks passed: real API, schema editor, quota, control SSH selection/generation/save/public download, independent identity, escaping, schema conflicts, revocation, invitation deletion, retained resources, mobile layout and stale logout responses.')
        finally:
            service.terminate()
            try:
                service.wait(timeout=10)
            except subprocess.TimeoutExpired:
                service.kill()
                service.wait()
