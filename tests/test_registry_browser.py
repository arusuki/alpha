"""Real registry/control processes; synthetic provisioning results, no external services."""
import hashlib
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
ssh_key = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f'

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
        code = secrets.token_hex(24)
        sql('control', 'INSERT INTO member_invitations(id,code_hash,label,quota,created_by,created_at) VALUES(?,?,?,?,?,?)',
            (secrets.token_hex(16), hashlib.sha256(code.encode()).hexdigest(), 'browser', 1, 'test', time.time()))
        fields = [dict(key='group', label='课题组', type='select', required=True, options=['A组', 'B组']),
                  dict(key='note', label='<img src=x onerror=alert(1)>', type='text', required=False)]
        sql('control', 'UPDATE member_registration_schema SET fields=?', (json.dumps(fields),))
        entry = registry_url + '/registry/Abcd1234/' + code

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
            admin.set_viewport_size(dict(width=390, height=844))
            assert admin.evaluate('document.documentElement.scrollWidth <= innerWidth')
            expect(card.locator('[data-edit-node]')).to_be_visible()
            page.goto(entry)
            expect(page.locator('#registration')).to_be_visible()
            expect(page.locator('#fields label').last).to_contain_text('<img')
            assert page.locator('#fields img').count() == 0
            page.locator('[name=username]').fill('alice')
            page.locator('[name=ssh_public_key]').fill(ssh_key)
            page.locator('[data-key=group]').select_option('A组')
            page.locator('#submit').click()
            expect(page.locator('#progress')).to_be_visible()
            expect(page.locator('#retry')).to_be_visible()
            expect(page.locator('#share')).to_be_hidden()
            expect(page.locator('#steps')).to_contain_text('暂无可分配')
            member_id = sql('control', 'SELECT id FROM members WHERE username=?', ('alice',))[0][0]
            for _ in range(100):
                if sql('control', 'SELECT pending FROM member_work WHERE member_id=?', (member_id,))[0][0] == 0:
                    break
                time.sleep(.05)
            # Supply the result an external resource provisioner would persist.
            share_url = 'https://login.tailscale.com/admin/invite/browser-test'
            sql('control', "UPDATE member_access SET invite_id='browser',invite_url=?,invite_state='invited',key_state='ready',error='' WHERE member_id=?", (share_url, member_id))
            expect(page.locator('#share')).to_be_visible()
            expect(page.locator('#shareLink')).to_have_attribute('href', share_url)
            expect(page.locator('#percent')).to_have_text('100%')
            expect(page.locator('#retry')).to_be_hidden()
            page.reload()
            expect(page.locator('#share')).to_be_visible()
            expect(page.locator('#registration')).to_be_hidden()
            assert sql('control', 'SELECT count(*) FROM members')[0][0] == 1
            assert sql('control', 'SELECT used FROM member_invitations')[0][0] == 1

            # A gateway restart keeps both its binding and the existing browser session.
            registry.terminate()
            registry.wait(timeout=15)
            registry, restarted_url = start('registry', ['--registry', '--data-dir', str(root / 'registry'), '--port', registry_url.rsplit(':', 1)[1]])
            assert restarted_url == registry_url
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
        print('Registry browser checks passed: typed node creation, invalid credentials, status, masked token editing, token rotation, control/registry restart, registration progress, mobile layout and removal.')
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
