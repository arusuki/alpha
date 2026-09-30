"""Fixed alpha-jump installation and real cross-UID SSH in a disposable container.

Requires an existing image with OpenSSH, sudo, Python 3, useradd and runuser. Never
downloads an image or modifies the host's users, SSH configuration or data.
PROJECT_ALPHA_BASTION_IMAGE defaults to sc2025:1.0.
"""
import base64
import http.cookiejar
import json
import os
from pathlib import Path
import pwd
import re
import subprocess
import sys
import tempfile
import time
import urllib.error
import urllib.parse
import urllib.request


def run(*args, success=True, **kwargs):
    result = subprocess.run(args, text=True, stdout=subprocess.PIPE,
                            stderr=subprocess.PIPE, timeout=45, **kwargs)
    if success:
        assert result.returncode == 0, (args, result.stdout, result.stderr)
    else:
        assert result.returncode != 0, (args, result.stdout, result.stderr)
    return result


def inside():
    assert os.geteuid() == 0 and Path('/.dockerenv').exists()
    assert os.environ.get('PROJECT_ALPHA_BASTION_TEST_CONTAINER') == '1'
    binary = '/alpha-test/project-alpha'
    service = 'alpha-control-test'
    directory = '/var/lib/alpha-control-test'
    run('useradd', '--system', '--user-group', '--create-home', service)
    Path('/run/sshd').mkdir(parents=True, exist_ok=True)
    run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', '/tmp/alpha-host-key')
    original_ssh = (
        'Port 18222\nListenAddress 127.0.0.1\nHostKey /tmp/alpha-host-key\n'
        'UsePAM no\nLogLevel VERBOSE\nPidFile /tmp/alpha-sshd.pid\n')
    config = Path('/etc/ssh/sshd_config')
    conflicting = original_ssh + 'Match User alpha-jump\n    MaxSessions 10\n'
    config.write_text(conflicting)
    init = [binary, 'bastion', 'init', '--service-user', service,
            '--data-dir', directory, '--no-reload']
    # Adopt a pre-existing account without replacing its numeric identity/home.
    Path('/var/empty/alpha-jump').mkdir(parents=True)
    run('useradd', '--system', '--user-group', '--home-dir', '/var/empty/alpha-jump',
        '--no-create-home', '--shell', '/bin/false', '--password', '*', 'alpha-jump')
    existing_jump = pwd.getpwnam('alpha-jump')
    assert '选择接管' in run(*init, success=False).stderr
    adopt = [binary, 'bastion', 'adopt', *init[3:]]
    assert 'MaxSessions' in run(*adopt, success=False).stderr
    assert pwd.getpwnam('alpha-jump') == existing_jump
    manifest = Path('/var/lib/project-alpha-jump/installation.json')
    assert not json.loads(manifest.read_text())['ready']
    assert config.read_text() == conflicting, 'installed a conflicting SSH configuration'
    config.write_text(original_ssh)
    # A failed daemon reload must restore the old config and leave this first
    # installation incomplete. The fake service manager exists only here.
    systemctl = Path('/usr/bin/systemctl')
    systemctl.unlink(missing_ok=True)
    systemctl.write_text('#!/bin/sh\n[ "$1" = is-active ] && exit 0\necho test-reload-failed >&2\nexit 1\n')
    systemctl.chmod(0o755)
    assert 'SSH 重载失败' in run(*init[:-1], success=False).stderr
    assert config.read_text() == original_ssh
    assert not json.loads(manifest.read_text())['ready']
    # The successful initialization now goes through the real web API and sudo.
    password = 'Disposable-sudo-password-123'
    run('chpasswd', input=service + ':' + password + '\n')
    sudoers = Path('/etc/sudoers.d/alpha-control-test')
    sudoers.write_text(service + ' ALL=(root) PASSWD: ' + binary + ' bastion install-helper\n')
    sudoers.chmod(0o440)
    run('visudo', '-cf', str(sudoers))
    systemctl.write_text('#!/bin/sh\n[ "$1" = is-active ] && exit 0\n[ "$1" = reload ] && exec /bin/kill -HUP "$(cat /tmp/alpha-sshd.pid)"\nexit 1\n')
    uid = pwd.getpwnam(service).pw_uid
    jump_uid = pwd.getpwnam('alpha-jump').pw_uid
    assert Path(directory, 'platform.sqlite3').stat().st_uid == uid
    assert Path('/usr/local/libexec/project-alpha-jump').stat().st_uid == 0
    run('runuser', '-u', service, '--', *init, success=False)
    run('runuser', '-u', service, '--', 'test', '-w', '/var/empty/alpha-jump', success=False)
    run('runuser', '-u', 'alpha-jump', '--', 'test', '-w', '/var/lib/project-alpha-jump/keys', success=False)
    run('runuser', '-u', 'alpha-jump', '--', 'test', '-r', directory + '/platform.sqlite3', success=False)
    run('ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', '/tmp/alpha-member-key')
    public_key = Path('/tmp/alpha-member-key.pub').read_text().strip()
    reader = ['runuser', '-u', 'alpha-jump', '--', '/usr/local/libexec/project-alpha-jump',
              'bastion', 'authorized-keys', 'alpha-jump', str(jump_uid)]
    assert '初始化尚未完成' in run(*reader, success=False).stderr
    processes = []
    log = Path('/tmp/alpha-control.log').open('w+')
    ssh_log = Path('/tmp/alpha-sshd.log').open('w+')
    base = 'http://127.0.0.1:18765'
    cookies = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                                        urllib.request.HTTPCookieProcessor(cookies))
    csrf = ''

    def api(path, value=None, method=None, form=False, expected=200):
        headers = {'Origin': base, 'X-CSRF-Token': csrf}
        data = None
        if value is not None:
            headers['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
            data = (urllib.parse.urlencode(value) if form else json.dumps(value)).encode()
        request = urllib.request.Request(base + path, data=data, headers=headers, method=method)
        try:
            with opener.open(request, timeout=50) as response:
                body = response.read().decode()
                return body if form else json.loads(body)
        except urllib.error.HTTPError as error:
            body = error.read().decode()
            if error.code == expected:
                return json.loads(body)
            raise AssertionError((path, error.code, body)) from error

    def wait_for(fn):
        deadline = time.monotonic() + 15
        last = None
        while time.monotonic() < deadline:
            try:
                last = fn()
                if last:
                    return last
            except (urllib.error.URLError, AssertionError):
                pass
            time.sleep(.1)
        raise AssertionError(('condition timed out', last))

    def start_control(no_new_privileges=True, port='18765'):
        prefix = ['setpriv', '--no-new-privs'] if no_new_privileges else []
        process = subprocess.Popen(prefix + ['runuser', '-u', service, '--',
                                    binary, '--control', '--data-dir', directory, '--port', port],
                                   stdout=log, stderr=log)
        processes.append(process)
        wait_for(lambda: api('/api/session'))
        return process

    ssh = ['ssh', '-i', '/tmp/alpha-member-key', '-o', 'BatchMode=yes',
           '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=no',
           '-o', 'UserKnownHostsFile=/dev/null', '-o', 'ConnectTimeout=3', '-p', '18222']

    def forward(success=True):
        return run(*ssh, '-W', '127.0.0.1:18765', 'alpha-jump@127.0.0.1',
                   input='GET /api/session HTTP/1.0\r\nHost: 127.0.0.1:18765\r\n\r\n', success=success)

    try:
        control = start_control()
        csrf = api('/api/setup', {'username': 'operator', 'password': 'Integration-test-password-123'})['csrf']
        resources = api('/api/bastion/resources')
        assert not resources['jump_installation']['web']['available']
        assert 'NoNewPrivileges' in api('/api/bastion/install', {'action': 'init', 'sudo_password': password}, expected=409)['error']
        control.terminate()
        control.wait(timeout=10)
        control = start_control(no_new_privileges=False)
        assert api('/api/bastion/resources')['jump_installation']['web']['available']
        sshd = subprocess.Popen(['/usr/sbin/sshd', '-D', '-e'], stdout=ssh_log, stderr=ssh_log)
        processes.append(sshd)
        wait_for(lambda: Path('/tmp/alpha-sshd.pid').exists())
        wrong_password = 'Wrong-disposable-password'
        rejected = api('/api/bastion/install', {'action': 'init', 'sudo_password': wrong_password}, expected=403)
        assert wrong_password not in json.dumps(rejected)
        assert not json.loads(manifest.read_text())['ready']
        assert api('/api/bastion/install', {'action': 'init', 'sudo_password': password})['ok']
        assert json.loads(manifest.read_text())['ready']
        # Success must not let a later attempt reuse sudo's timestamp credentials.
        api('/api/bastion/install', {'action': 'init', 'sudo_password': wrong_password}, expected=403)
        run('runuser', '-u', service, '--', 'sudo', '-n', '--', binary, 'bastion', 'install-helper', success=False)
        for root in [Path(directory), Path('/var/lib/project-alpha-jump')]:
            for item in root.rglob('*'):
                if item.is_file():
                    raw = item.read_bytes()
                    assert password.encode() not in raw and wrong_password.encode() not in raw, item
        assert password not in Path('/tmp/alpha-control.log').read_text()
        resources = api('/api/bastion/resources')
        assert resources['jump_installation']['ready']
        page = api('/admin/member-invitations/create', {'label': 'ssh integration', 'quota': 2}, form=True)
        invitation = re.search(r'data-issued-invitation value="([a-f0-9]+)"', page).group(1)
        members = []
        for name in ['alice', 'bobby']:
            m = api('/api/members/register', {'username': name, 'ssh_public_key': public_key,
                    'invitation_code': invitation, 'schema_revision': 1, 'profile': {}})
            members.append(m['id'])
            wait_for(lambda: api('/api/members/' + m['id'] + '/resources')['access']['key_state'] == 'ready')
        keys = Path('/var/lib/project-alpha-jump/keys/keys.json')
        assert keys.stat().st_uid == uid and keys.stat().st_gid == pwd.getpwnam('alpha-jump').pw_gid
        assert keys.stat().st_mode & 0o777 == 0o640
        before = keys.read_bytes()
        assert len(run(*reader).stdout.splitlines()) == 1
        run(*init)
        assert keys.read_bytes() == before, 'reinitialization erased keys'
        wrong = [binary, 'bastion', 'init', '--service-user', service,
                 '--data-dir', '/var/lib/alpha-other-control', '--no-reload']
        assert '其他 control' in run(*wrong, success=False).stderr
        assert keys.read_bytes() == before
        tool = Path('/usr/local/libexec/project-alpha-jump')
        before_tool, before_config = tool.read_bytes(), config.read_bytes()
        before_manifest = manifest.read_bytes()
        blocked = api('/api/bastion/install', {'action': 'delete', 'confirm': 'alpha-jump',
                      'sudo_password': password}, expected=409)
        assert '先撤销' in blocked['error']
        assert not (manifest.parent / 'account-removed').exists()
        api('/api/bastion/install', {'action': 'release', 'sudo_password': password})
        status = api('/api/bastion/resources')['jump_installation']
        assert not status['ready'] and status['account']['released']
        assert keys.read_bytes() == before and tool.read_bytes() == before_tool
        assert config.read_bytes() == before_config
        assert manifest.read_bytes() == before_manifest
        assert len(run(*reader).stdout.splitlines()) == 1
        assert '选择接管' in run(*init, success=False).stderr
        api('/api/bastion/install', {'action': 'adopt', 'sudo_password': password})
        assert api('/api/bastion/resources')['jump_installation']['ready']
        assert keys.read_bytes() == before
        wait_for(lambda: 'HTTP/1.0 200' in forward().stdout)
        run(*ssh, 'alpha-jump@127.0.0.1', 'true', success=False)
        control.terminate()
        control.wait(timeout=10)
        assert len(run(*reader).stdout.splitlines()) == 1, 'reader depends on running control'
        control = start_control(no_new_privileges=False)
        for index, member in enumerate(members):
            api('/api/members/' + member, {}, 'DELETE')
            wait_for(lambda: all(a['member_id'] != member or a['key_state'] == 'deleted'
                                for a in api('/api/bastion/resources')['assignments']))
            if index == 0:
                assert 'HTTP/1.0 200' in forward().stdout, 'revoked another member sharing the same key'
        assert 'Permission denied' in forward(success=False).stderr
        # A key in alpha-jump's home cannot bypass centrally revoked authorization.
        home = Path('/var/empty/alpha-jump/.ssh')
        home.mkdir(mode=0o700)
        os.chown(home, jump_uid, pwd.getpwnam('alpha-jump').pw_gid)
        (home / 'authorized_keys').write_text(public_key + '\n')
        os.chown(home / 'authorized_keys', jump_uid, pwd.getpwnam('alpha-jump').pw_gid)
        (home / 'authorized_keys').chmod(0o600)
        assert 'Permission denied' in forward(success=False).stderr
        # Deletion and recreation preserve tool, data and home without userdel -r.
        empty_keys = keys.read_bytes()
        home_key = (home / 'authorized_keys').read_bytes()
        api('/api/bastion/install', {'action': 'delete', 'confirm': 'alpha-jump', 'sudo_password': password})
        status = api('/api/bastion/resources')['jump_installation']
        assert not status['account']['exists'] and status['account']['removed']
        assert not status['ready']
        assert keys.read_bytes() == empty_keys and tool.read_bytes() == before_tool
        assert (home / 'authorized_keys').read_bytes() == home_key
        assert config.read_bytes() == before_config
        # An already removed account can be retried safely.
        api('/api/bastion/install', {'action': 'delete', 'confirm': 'alpha-jump', 'sudo_password': password})
        api('/api/bastion/install', {'action': 'init', 'sudo_password': password})
        assert pwd.getpwnam('alpha-jump').pw_uid == existing_jump.pw_uid
        assert pwd.getpwnam('alpha-jump').pw_gid == existing_jump.pw_gid
        assert api('/api/bastion/resources')['jump_installation']['ready']
        assert keys.read_bytes() == empty_keys
        assert run(*reader).stdout == ''
        # Reproduce the actual failure: take over another control's populated
        # pool, first with the same service UID and then with a different UID.
        old_service, old_directory, old_base, old_opener, old_csrf = service, directory, base, opener, csrf

        def key_variant(value, mask):
            kind, blob = value.split()[:2]
            raw = bytearray(base64.b64decode(blob))
            raw[-1] ^= mask
            return kind + ' ' + base64.b64encode(raw).decode()

        key_a = ' '.join(public_key.split()[:2])
        key_b, key_c, key_d = (key_variant(key_a, n) for n in [1, 2, 3])
        inherited = json.loads(keys.read_text())
        inherited['keys'] = {'a' * 32: key_a, 'b' * 32: key_b}
        keys.write_text(json.dumps(inherited))
        inherited_bytes = keys.read_bytes()

        def new_client(port):
            nonlocal base, opener, csrf
            base = 'http://127.0.0.1:' + port
            opener = urllib.request.build_opener(urllib.request.ProxyHandler({}),
                       urllib.request.HTTPCookieProcessor(http.cookiejar.CookieJar()))
            csrf = ''

        def invitation_for_pool():
            page = api('/admin/member-invitations/create', {'label': 'pool takeover', 'quota': 5}, form=True)
            return re.search(r'data-issued-invitation value="([a-f0-9]+)"', page).group(1)

        def register_pool(name, key, invitation):
            return api('/api/members/register', {'username': name, 'ssh_public_key': key,
                       'invitation_code': invitation, 'schema_revision': 1, 'profile': {}})['id']

        directory = '/var/lib/alpha-other-control'
        new_client('18766')
        second_control = start_control(no_new_privileges=False, port='18766')
        csrf = api('/api/setup', {'username': 'operator', 'password': 'Integration-test-password-123'})['csrf']
        invitation = invitation_for_pool()
        reused = register_pool('reused', key_a, invitation)
        missing = register_pool('missing', key_c, invitation)
        wait_for(lambda: api('/api/members/' + missing + '/resources')['access']['error'])
        assert not api('/api/bastion/resources')['jump_installation']['ready']
        api('/api/bastion/install', {'action': 'adopt', 'sudo_password': password})
        assert api('/api/bastion/resources')['jump_installation']['ready']
        adopted = json.loads(keys.read_text())
        assert adopted['control_id'] != inherited['control_id']
        assert adopted['keys']['a' * 32] == key_a and adopted['keys']['b' * 32] == key_b
        pool = {k['public_key']: k for k in api('/api/bastion/keys')['keys']}
        assert len(pool) == 3 and pool[key_a]['members'][0]['id'] == reused
        assert pool[key_b]['state'] == 'free' and pool[key_c]['members'][0]['id'] == missing
        assert api('/api/members/' + missing + '/resources')['access']['key_state'] == 'ready'
        backups = list(Path('/var/lib').glob('project-alpha-jump.before-adopt-*'))
        assert any((p / 'keys/keys.json').read_bytes() == inherited_bytes for p in backups)
        with old_opener.open(old_base + '/api/bastion/resources') as response:
            assert not json.load(response)['jump_installation']['ready']
        api('/api/bastion/keys/' + pool[key_a]['id'], {}, 'DELETE', expected=409)
        api('/api/bastion/keys/' + pool[key_b]['id'], {}, 'DELETE')
        shared = register_pool('shared', key_a, invitation)
        wait_for(lambda: api('/api/members/' + shared + '/resources')['access']['key_state'] == 'ready')
        assert len(api('/api/bastion/keys')['keys']) == 2
        for member in [reused, shared]:
            api('/api/members/' + member, {}, 'DELETE')
            wait_for(lambda: all(a['member_id'] != member or a['key_state'] == 'deleted'
                                for a in api('/api/bastion/resources')['assignments']))
            remaining = {k['public_key']: k for k in api('/api/bastion/keys')['keys']}
            assert (key_a in remaining) == (member == reused)

        service, directory = 'alpha-new-control', '/var/lib/alpha-new-control'
        run('useradd', '--system', '--user-group', '--create-home', service)
        run('chpasswd', input=service + ':' + password + '\n')
        new_sudoers = Path('/etc/sudoers.d/alpha-new-control')
        new_sudoers.write_text(service + ' ALL=(root) PASSWD: ' + binary + ' bastion install-helper\n')
        new_sudoers.chmod(0o440)
        Path(directory).mkdir(mode=0o700)
        os.chown(directory, pwd.getpwnam(service).pw_uid, pwd.getpwnam(service).pw_gid)
        new_client('18767')
        third_control = start_control(no_new_privileges=False, port='18767')
        csrf = api('/api/setup', {'username': 'operator', 'password': 'Integration-test-password-123'})['csrf']
        newest = register_pool('newest', key_d, invitation_for_pool())
        wait_for(lambda: api('/api/members/' + newest + '/resources')['access']['error'])
        api('/api/bastion/install', {'action': 'adopt', 'sudo_password': password})
        new_uid = pwd.getpwnam(service).pw_uid
        assert json.loads(manifest.read_text())['service_uid'] == new_uid
        assert keys.stat().st_uid == new_uid and keys.parent.stat().st_uid == new_uid
        assert (keys.parent / '.lock').stat().st_uid == new_uid
        assert pwd.getpwnam('alpha-jump').pw_uid == jump_uid
        run('runuser', '-u', old_service, '--', 'test', '-w', str(keys), success=False)
        pool = {k['public_key']: k for k in api('/api/bastion/keys')['keys']}
        assert pool[key_c]['state'] == 'free' and pool[key_d]['members'][0]['id'] == newest
        api('/api/bastion/keys/' + pool[key_c]['id'], {}, 'DELETE')
        assert key_d in run(*reader).stdout

        # The departed service identity must not block explicit re-adoption.
        third_control.terminate()
        third_control.wait(timeout=10)
        run('userdel', service)
        service, directory, base, opener, csrf = old_service, old_directory, old_base, old_opener, old_csrf
        api('/api/bastion/install', {'action': 'adopt', 'sudo_password': password})
        pool = api('/api/bastion/keys')['keys']
        assert len(pool) == 1 and pool[0]['state'] == 'free' and pool[0]['public_key'] == key_d
        api('/api/bastion/keys/' + pool[0]['id'], {}, 'DELETE')
        assert api('/api/bastion/keys')['keys'] == []
        print('Cross-control/UID takeover, missing-key reconciliation, content indexing, free cleanup and departed-service takeover passed.')
        print('Bastion SSH checks passed: adoption, release, deletion/recreation, retained data,  configuration conflict, reload rollback, incomplete install recovery, '
              'web sudo initialization, wrong password, no credential cache/persistence, non-root publication, separate reader UID, '
              'NoNewPrivileges, repeat install, instance binding, TCP forwarding, shell denial, '
              'restart, shared-key revocation and no home fallback.')
    except Exception:
        for name in ['/tmp/alpha-control.log', '/tmp/alpha-sshd.log']:
            print(Path(name).read_text(), file=sys.stderr)
        raise
    finally:
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=10)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()


if __name__ == '__main__':
    if sys.argv[1:] == ['--inside']:
        inside()
    else:
        assert not sys.argv[1:]
        repo = Path(__file__).resolve().parents[1]
        image = os.environ.get('PROJECT_ALPHA_BASTION_IMAGE', 'sc2025:1.0')
        with tempfile.TemporaryDirectory(prefix='alpha-bastion-ssh-') as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            subprocess.run(['go', 'build', '-o', str(root / 'project-alpha'), './cmd/project-alpha'], cwd=repo, check=True)
            (root / 'test.py').write_bytes(Path(__file__).read_bytes())
            (root / 'test.py').chmod(0o644)
            subprocess.run(['docker', '--host', 'unix:///var/run/docker.sock', 'run', '--rm', '--pull=never',
                            '--network', 'none', '--user', '0:0', '--entrypoint', '/usr/bin/python3',
                            '--env', 'PROJECT_ALPHA_BASTION_TEST_CONTAINER=1',
                            '--mount', f'type=bind,src={root},dst=/alpha-test,readonly',
                            image, '/alpha-test/test.py', '--inside'], check=True, timeout=240)
