"""Two share accounts, real SSH key management, HTTP proxy and inverse install.

Uses an existing disposable Docker image. No host accounts/configuration change,
no image pull and no external API calls. Uses explicitly supplied existing keys.
"""
import http.cookiejar
import json
import os
from pathlib import Path
import pwd
import re
import socket
import sqlite3
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
    assert (result.returncode == 0) == success, (args, result.stdout, result.stderr)
    return result


def wait_for(fn):
    deadline = time.monotonic() + 15
    last = None
    while time.monotonic() < deadline:
        try:
            last = fn()
            if last:
                return last
        except (urllib.error.URLError, AssertionError, OSError):
            pass
        time.sleep(.1)
    raise AssertionError(('condition timed out', last))


def inside():
    assert os.geteuid() == 0 and Path('/.dockerenv').exists()
    assert os.environ.get('PROJECT_ALPHA_BASTION_TEST_CONTAINER') == '1'
    binary = '/alpha-test/project-alpha'
    share_ip = socket.gethostbyname(socket.gethostname())
    assert not share_ip.startswith('127.')
    control_port, proxy_port, ssh_port = 18765, 19765, 18222
    processes, logs = [], []
    fixtures = Path('/alpha-test-keys')

    def copy_key(source, target, owner=None):
        for suffix, mode in [('', 0o600), ('.pub', 0o644)]:
            path = Path(str(target) + suffix)
            path.write_bytes((fixtures / (source + suffix)).read_bytes())
            path.chmod(mode)
            if owner:
                os.chown(path, owner.pw_uid, owner.pw_gid)

    for name, key_name in [('alpha-control-test', 'control_ed25519'), ('alpha-client-test', 'member_ed25519')]:
        run('useradd', '--system', '--user-group', '--create-home', name)
        home = Path(pwd.getpwnam(name).pw_dir)
        run('runuser', '-u', name, '--', 'mkdir', '-m', '700', str(home / '.ssh'))
        copy_key(key_name, home / '.ssh/id_ed25519', pwd.getpwnam(name))
    control_key = Path(pwd.getpwnam('alpha-control-test').pw_dir) / '.ssh/id_ed25519.pub'
    member_home = Path(pwd.getpwnam('alpha-client-test').pw_dir)
    member_key = ' '.join((member_home / '.ssh/id_ed25519.pub').read_text().split()[:2])
    copy_key('free_ed25519', member_home / '.ssh/free_key', pwd.getpwnam('alpha-client-test'))
    free_key = ' '.join((member_home / '.ssh/free_key.pub').read_text().split()[:2])
    Path('/run/sshd').mkdir(parents=True, exist_ok=True)
    copy_key('host_ed25519', Path('/tmp/share-host-key'))
    config = Path('/etc/ssh/sshd_config')
    original = (f'Port {ssh_port}\nListenAddress 0.0.0.0\nHostKey /tmp/share-host-key\n'
                'UsePAM no\nLogLevel VERBOSE\nPidFile /tmp/share-sshd.pid\n')
    config.write_text(original)
    for name in ['alpha-control-test', 'alpha-client-test']:
        u = pwd.getpwnam(name)
        known = Path(u.pw_dir) / '.ssh/known_hosts'
        known.write_text(f'[{share_ip}]:{ssh_port} ' + Path('/tmp/share-host-key.pub').read_text())
        os.chown(known, u.pw_uid, u.pw_gid)
        known.chmod(0o600)
    init = [binary, 'share-node', '--control-key-file', str(control_key), '--listen-host', share_ip,
            '--control-url', f'http://{share_ip}:{control_port}', '--status-port', str(proxy_port),
            '--no-reload', '--no-service']
    # Earlier Match rules must be detected for either role, without partial installs.
    config.write_text(original + 'Match User alpha-worker\n AllowTcpForwarding yes\n')
    assert 'AllowTcpForwarding' in run(*init, success=False).stderr
    assert not Path('/var/lib/project-alpha-jump').exists()
    for name in ['alpha-worker', 'alpha-jump']:
        try:
            pwd.getpwnam(name)
        except KeyError:
            pass
        else:
            raise AssertionError('failed installation retained an account')
    config.write_text(original)
    # Reload failure restores configuration and account creation.
    systemctl = Path('/usr/local/bin/systemctl')
    systemctl.write_text('#!/bin/sh\n[ "$1" = is-active ] && exit 0\nexit 1\n')
    systemctl.chmod(0o755)
    assert '重载失败' in run(*(a for a in init if a != '--no-reload'), success=False).stderr
    assert config.read_text() == original
    systemctl.unlink()
    run(*init)
    installation = Path('/var/lib/project-alpha-jump/installation.json')
    manifest = json.loads(installation.read_text())
    jump, worker = pwd.getpwnam('alpha-jump'), pwd.getpwnam('alpha-worker')
    assert worker.pw_uid > 0 and jump.pw_uid > 0 and worker.pw_uid != jump.pw_uid
    assert worker.pw_shell == '/bin/sh' and jump.pw_shell == '/bin/false'
    assert manifest['worker_uid'] == worker.pw_uid and manifest['jump_uid'] == jump.pw_uid
    assert 'User=alpha-worker' in Path('/etc/systemd/system/project-alpha-share-node.service').read_text()
    keys = Path('/var/lib/project-alpha-jump/keys/keys.json')

    def start(args, name=None, label='service'):
        log = Path('/tmp/share-' + label + '.log').open('w+')
        logs.append(log)
        kwargs = {}
        if name:
            u = pwd.getpwnam(name)
            def drop():
                os.initgroups(name, u.pw_gid)
                os.setgid(u.pw_gid)
                os.setuid(u.pw_uid)
            kwargs.update(preexec_fn=drop, env={**os.environ, 'HOME': u.pw_dir})
        process = subprocess.Popen(args, stdout=log, stderr=log, **kwargs)
        processes.append(process)
        return process

    def ssh(name, account, command, *, data=None, forward=None, success=True):
        args = ['runuser', '-u', name, '--', 'ssh', '-T', '-o', 'BatchMode=yes',
                '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=3', '-p', str(ssh_port)]
        if forward:
            args.extend(['-W', forward])
        args.append(account + '@' + share_ip)
        if command:
            args.append(command)
        return run(*args, input=data, success=success)

    def management(operation, **fields):
        return json.loads(ssh('alpha-control-test', 'alpha-worker', 'alpha-worker cmd',
                             data=json.dumps(dict(version=2, operation=operation, **fields))).stdout)

    directory = Path('/var/lib/alpha-share-control-test')
    directory.mkdir(mode=0o700)
    control_user = pwd.getpwnam('alpha-control-test')
    os.chown(directory, control_user.pw_uid, control_user.pw_gid)
    base = f'http://127.0.0.1:{control_port}'
    cookie = http.cookiejar.CookieJar()
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), urllib.request.HTTPCookieProcessor(cookie))
    csrf = ''

    def api(path, body=None, method=None, *, expected=200, form=False, token=None, proxy=False, origin=None):
        headers = {}
        if body is not None:
            headers['Content-Type'] = 'application/x-www-form-urlencoded' if form else 'application/json'
            headers['X-CSRF-Token'] = csrf
        if token:
            headers['Authorization'] = 'Bearer ' + token
        if origin:
            headers['Origin'] = origin
        data = (urllib.parse.urlencode(body) if form else json.dumps(body)).encode() if body is not None else None
        host = f'http://{share_ip}:{proxy_port}' if proxy else base
        request = urllib.request.Request(host + path, data=data, method=method, headers=headers)
        try:
            with opener.open(request, timeout=35) as response:
                content = response.read().decode()
                assert response.status == expected, content
                return content if form else json.loads(content)
        except urllib.error.HTTPError as error:
            content = error.read().decode()
            assert error.code == expected, (path, error.code, content)
            return json.loads(content)

    try:
        start(['/usr/sbin/sshd', '-D', '-e'], label='sshd')
        wait_for(lambda: Path('/tmp/share-sshd.pid').exists())
        assert management('inspect')['keys'] == {}
        ssh('alpha-control-test', 'alpha-worker', 'sh -c id', data='', success=False)
        ssh('alpha-client-test', 'alpha-worker', 'alpha-worker cmd', data='{}', success=False)
        ssh('alpha-control-test', 'alpha-worker', None, forward=f'127.0.0.1:{control_port}', success=False)
        management('ensure', keys=[member_key, free_key])
        assert keys.stat().st_uid == worker.pw_uid and keys.stat().st_gid == jump.pw_gid
        assert keys.stat().st_mode & 0o777 == 0o640
        run('runuser', '-u', 'alpha-jump', '--', 'test', '-r', str(keys))
        run('runuser', '-u', 'alpha-jump', '--', 'test', '-w', str(keys), success=False)
        before = keys.read_bytes()
        run(*init)
        assert keys.read_bytes() == before and pwd.getpwnam('alpha-worker').pw_uid == worker.pw_uid
        assert config.read_text().count('Match User alpha-worker') == 1
        start([binary, '--control', '--host', '0.0.0.0', '--port', str(control_port), '--data-dir', str(directory)],
              name='alpha-control-test', label='control')
        wait_for(lambda: api('/api/session'))
        csrf = api('/api/setup', dict(username='operator', password='Integration-test-password-123'))['csrf']
        with sqlite3.connect(directory / 'platform.sqlite3') as db:
            db.execute("INSERT INTO bastion_tailscale VALUES(?,?,?,?,?,?,?)",
                       ('share', 'Share', 1, share_ip, ssh_port, proxy_port, f'http://{share_ip}:{control_port}'))
        page = api('/admin/member-invitations/create', dict(label='SSH test', quota=2), form=True)
        invitation = re.search(r'data-issued-invitation value="([a-f0-9]+)"', page).group(1)
        members = []
        for name in ['alice', 'bobby']:
            member = api('/api/members/register', dict(username=name, ssh_public_key=member_key,
                         invitation_code=invitation, schema_revision=1, profile={}), expected=201)
            members.append(member)
            wait_for(lambda: api('/api/members/' + member['id'] + '/resources')['access']['key_state'] == 'ready')
        proxy_process = start([binary, 'share-node', '--serve'], name='alpha-worker', label='proxy')
        wait_for(lambda: api('/api/status/alice', token=members[0]['resource_token'], proxy=True))
        view = api('/api/status/alice', token=members[0]['resource_token'], proxy=True)
        assert view['control']['status_url'] == f'http://{share_ip}:{proxy_port}/status/alice'
        assert view['access']['share_host'] == share_ip and view['access']['share_ssh_port'] == ssh_port
        api('/api/members/me/retry', {}, expected=403, token=members[0]['resource_token'], proxy=True,
            origin='http://evil.example')
        api('/api/members/me/retry', {}, expected=202, token=members[0]['resource_token'], proxy=True,
            origin=f'http://{share_ip}:{proxy_port}')
        forwarded = ssh('alpha-client-test', 'alpha-jump', None, forward=f'127.0.0.1:{control_port}',
                        data=f'GET /api/session HTTP/1.0\r\nHost: localhost:{control_port}\r\n\r\n')
        assert '200 OK' in forwarded.stdout
        ssh('alpha-client-test', 'alpha-jump', 'id', success=False)
        ssh('alpha-control-test', 'alpha-jump', 'id', success=False)
        for name, account in [('alpha-control-test', 'alpha-worker'), ('alpha-client-test', 'alpha-jump')]:
            run('runuser', '-u', name, '--', 'ssh', '-N', '-o', 'BatchMode=yes', '-o', 'ExitOnForwardFailure=yes',
                '-p', str(ssh_port), '-R', f'{share_ip}:20765:127.0.0.1:{control_port}', account + '@' + share_ip,
                success=False)
        pool = api('/api/bastion/keys')['keys']
        unused = next(k for k in pool if k['public_key'] == free_key)
        assert unused['state'] == 'free' and unused['node_id'] == 'share'
        api('/api/bastion/keys/share/' + unused['id'], {}, 'DELETE')
        assert free_key not in management('inspect')['keys'].values()
        for member in members:
            api('/api/members/' + member['id'], {}, 'DELETE')
            remaining = management('inspect')['keys'].values()
            assert (member_key in remaining) == (member == members[0])
        assert management('inspect')['keys'] == {}
        assert '仍有进程' in run(binary, 'share-node', '--uninstall', '--no-reload', '--no-service', success=False).stderr
        proxy_process.terminate()
        proxy_process.wait(timeout=15)
        wait_for(lambda: run('pgrep', '-u', str(worker.pw_uid), success=False).returncode == 1)
        # A late systemd failure must leave the manifest for a verified retry.
        systemctl.write_text('#!/bin/sh\n[ "$1" = daemon-reload ] && exit 1\nexit 0\n')
        systemctl.chmod(0o755)
        uninstall_error = run(binary, 'share-node', '--uninstall', '--no-reload', success=False).stderr
        assert 'daemon-reload' in uninstall_error, uninstall_error
        assert installation.exists() and not installation.parent.joinpath('keys').exists()
        systemctl.unlink()
        run(binary, 'share-node', '--uninstall', '--no-reload', '--no-service')
        assert config.read_text() == original
        assert not installation.parent.exists()
        assert not Path('/etc/systemd/system/project-alpha-share-node.service').exists()
        assert not Path('/usr/local/libexec/project-alpha-jump').exists()
        for name in ['alpha-worker', 'alpha-jump']:
            try:
                pwd.getpwnam(name)
            except KeyError:
                pass
            else:
                raise AssertionError('uninstall retained an account')
        run(binary, 'share-node', '--uninstall', '--no-reload', '--no-service')
        run(*init)
        run(binary, 'share-node', '--uninstall', '--no-reload', '--no-service')
        print('Share node integration passed: two non-root roles, SSH restrictions, atomic remote keys, '
              'HTTP status proxy, Host/Origin checks, shared-key revocation, rollback, reinstall and uninstall.')
    finally:
        failed = sys.exc_info()[0] is not None
        if failed:
            print(run('ps', '-eo', 'user,pid,ppid,state,comm').stdout, file=sys.stderr)
        for process in reversed(processes):
            if process.poll() is None:
                process.terminate()
                try:
                    process.wait(timeout=15)
                except subprocess.TimeoutExpired:
                    process.kill()
                    process.wait()
        for log in logs:
            log.seek(0)
            if failed:
                print(log.read()[-8000:], file=sys.stderr)
            log.close()


if __name__ == '__main__':
    if sys.argv[1:] == ['--inside']:
        inside()
    else:
        assert not sys.argv[1:]
        repo = Path(__file__).resolve().parents[1]
        key_directory = os.environ.get('PROJECT_ALPHA_BASTION_KEY_DIR')
        if not key_directory:
            print('Skipped SSH integration: set PROJECT_ALPHA_BASTION_KEY_DIR to existing disposable key fixtures.')
            sys.exit(0)
        key_directory = Path(key_directory).resolve(strict=True)
        for name in ['control_ed25519', 'member_ed25519', 'free_ed25519', 'host_ed25519']:
            for suffix in ['', '.pub']:
                assert (key_directory / (name + suffix)).is_file(), 'Missing SSH key fixture: ' + name + suffix
        image = os.environ.get('PROJECT_ALPHA_BASTION_IMAGE', 'sc2025:1.0')
        with tempfile.TemporaryDirectory(prefix='alpha-share-ssh-') as temporary:
            root = Path(temporary)
            root.chmod(0o755)
            subprocess.run(['go', 'build', '-o', str(root / 'project-alpha'), './cmd/project-alpha'], cwd=repo, check=True)
            (root / 'test.py').write_bytes(Path(__file__).read_bytes())
            (root / 'test.py').chmod(0o644)
            subprocess.run(['docker', '--host', 'unix:///var/run/docker.sock', 'run', '--rm', '--init', '--pull=never',
                            '--network', 'bridge', '--user', '0:0', '--entrypoint', '/usr/bin/python3',
                            '--env', 'PROJECT_ALPHA_BASTION_TEST_CONTAINER=1',
                            '--mount', f'type=bind,src={root},dst=/alpha-test,readonly',
                            '--mount', f'type=bind,src={key_directory},dst=/alpha-test-keys,readonly',
                            image, '/alpha-test/test.py', '--inside'], check=True, timeout=240)
