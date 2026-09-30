"""Member registration UI and public API against an isolated, real Go service."""
import os
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
                payload = dict(username='alice', invitation_code=code, schema_revision=schema['revision'],
                               profile=dict(full_name='<img src=x onerror=alert(1)>', degree='博士', group='A组'))
                registered = public.post('/api/members/register', data=payload)
                assert registered.status == 201, registered.text()
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
