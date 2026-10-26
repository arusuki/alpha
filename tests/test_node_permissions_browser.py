"""Invitation scope and per-member node permissions against a real control."""
import json
import os
import re
import sqlite3
import subprocess
import tempfile
import time
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
node_a, node_b = 'a' * 32, 'b' * 32
with tempfile.TemporaryDirectory(prefix='alpha-node-permissions-') as temporary:
    root = Path(temporary)
    binary = root / 'project-alpha'
    subprocess.run(['go', 'build', '-o', str(binary), './cmd/project-alpha'], cwd=repo, check=True)
    with (root / 'service.log').open('w+') as log:
        service = subprocess.Popen([str(binary), 'serve', '--data-dir', str(root / 'data'), '--port', '0'],
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
            assert url, 'control failed to start'
            with sqlite3.connect(root / 'data/platform.sqlite3') as db:
                for index, (node, name) in enumerate([(node_a, '计算 A'), (node_b, '计算 B <script>')]):
                    db.execute('INSERT INTO cluster_nodes VALUES(?,?,?,?,?,?,?)',
                               (node, name, f'http://127.0.0.1:{index+1}', 'test-token', index, 'worker', f'10.0.0.{index+1}'))
            with sync_playwright() as p:
                launch = dict(headless=True, args=['--no-sandbox'])
                if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
                    launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
                browser = p.chromium.launch(**launch)
                page = browser.new_page(viewport=dict(width=1280, height=1000))
                errors = []
                page.on('pageerror', lambda error: errors.append(str(error)))
                page.goto(url)
                page.locator('#authUsername').fill('operator')
                page.locator('#authPassword').fill('A-test-password-123')
                page.locator('#authSubmit').click()
                page.locator('.platform-nav [data-page="members"]').click()
                expect(page.locator('#invitationNodeChoices input')).to_have_count(2)
                expect(page.locator('#invitationNodeChoices input').first).to_be_disabled()
                page.locator('#invitationAllNodes').uncheck()
                page.locator(f'#invitationNodeChoices input[value="{node_a}"]').check()
                page.locator('#memberInvitationLabel').fill('只授权计算 A')
                page.locator('#memberCreateInvitation').click()
                expect(page.locator('#memberInvitationDialog')).to_be_visible()
                code = page.locator('#memberInvitationCode').input_value()
                page.locator('#memberInvitationClose').click()
                expect(page.locator('#memberInvitationsBody')).to_contain_text('计算 A')
                expect(page.locator('#memberInvitationsBody')).not_to_contain_text('计算 B')
                public = p.request.new_context(base_url=url)
                options = public.post('/api/members/registration-options', data=dict(invitation_code=code, username='alice'))
                assert options.status == 200, options.text()
                assert [n['node_id'] for n in options.json()['nodes']] == [node_a]
                registered = public.post('/api/members/register', data=dict(
                    username='alice', password='Member-password-123', invitation_code=code,
                    ssh_public_key='ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f',
                    schema_revision=1, profile={}, containers=[dict(node_id=node_a, mode='skip', container_id='')]))
                assert registered.status == 201, registered.text()
                member_id = registered.json()['id']
                page.locator('#membersRefresh').click()
                expect(page.locator('#membersBody')).to_contain_text('计算 A')
                page.locator(f'[data-member-permissions="{member_id}"]').click()
                expect(page.locator('#memberPermissionsDialog')).to_be_visible()
                expect(page.locator('#memberAllNodes')).not_to_be_checked()
                expect(page.locator(f'#memberNodeChoices input[value="{node_a}"]')).to_be_checked()
                page.locator(f'#memberNodeChoices input[value="{node_a}"]').uncheck()
                page.locator(f'#memberNodeChoices input[value="{node_b}"]').check()
                page.set_viewport_size(dict(width=390, height=844))
                assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
                page.screenshot(path='/tmp/alpha-node-permissions-mobile.png')
                page.locator('#memberPermissionsSave').click()
                expect(page.locator('#memberPermissionsDialog')).to_be_hidden()
                expect(page.locator('#membersBody')).to_contain_text('计算 B <script>')
                assert page.locator('#membersBody script').count() == 0
                page.locator(f'[data-member-permissions="{member_id}"]').click()
                expect(page.locator(f'#memberNodeChoices input[value="{node_b}"]')).to_be_checked()
                expect(page.locator(f'#memberNodeChoices input[value="{node_a}"]')).not_to_be_checked()
                with sqlite3.connect(root / 'data/platform.sqlite3') as db:
                    policy = json.loads(db.execute('SELECT permissions FROM member_node_permissions WHERE member_id=?', (member_id,)).fetchone()[0])
                    assert policy == dict(all_nodes=False, node_ids=[node_b])
                    assert db.execute('SELECT count(*) FROM member_node_resources').fetchone()[0] == 0
                assert not errors, errors
                browser.close()
        finally:
            service.terminate()
            service.wait(timeout=15)
print('Node permission browser checks passed: invitation scope, deferred allocation, member editing, escaping and mobile layout.')
