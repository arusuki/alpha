"""Root-only reversible fixture used by test_rootless_compose.py."""
import ctypes
import json
import os
from pathlib import Path
import pwd
import shutil
import stat
import subprocess
import sys

mode, directory, original = sys.argv[1:]
root = Path(directory)
account = pwd.getpwnam('docker-rootless')
home = Path(account.pw_dir)
base = home / '.docker-rootless'
unit = home / '.config/systemd/user/docker-rootless.service'
service = ['runuser', '-u', account.pw_name, '--', 'env', '-i', 'PATH=/usr/bin:/bin:/usr/sbin:/sbin',
           f'HOME={home}', f'XDG_RUNTIME_DIR=/run/user/{account.pw_uid}',
           f'DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/{account.pw_uid}/bus', 'systemctl', '--user']
docker = ['docker', '--host', f'unix://{base}/run/docker.sock']

def run(args, check=True):
    result = subprocess.run(args, capture_output=True, text=True, timeout=120)
    if check and result.returncode:
        raise RuntimeError(f'{args}: {result.stdout}\n{result.stderr}')
    return result

def inventory():
    return {key: sorted(run(docker + args).stdout.splitlines()) for key, args in {
        'containers': ['ps', '-aq', '--no-trunc'], 'volumes': ['volume', 'ls', '-q'],
        'images': ['image', 'ls', '-q', '--no-trunc'], 'id': ['info', '--format', '{{.ID}}']}.items()}

def make_original_mount_private(container, dest):
    pid = int(run(['docker', 'inspect', '--format', '{{.State.Pid}}', container]).stdout)
    lines = [line.split() for line in Path(f'/proc/{pid}/mountinfo').read_text().splitlines() if line.split()[4] == dest]
    source = str(base / 'run/docker.sock')
    if len(lines) != 1 or lines[0][3] not in (source, source + '//deleted'):
        raise RuntimeError('Original socket mount identity changed; backup retained')
    if not any(field.startswith('shared:') for field in lines[0][6:]):
        return
    # Old CLI versions inherited shared propagation from the source mount.
    # Confine restoration to this recorded mount before replacing its socket.
    child = os.fork()
    if child == 0:
        try:
            proc = os.open('/proc', os.O_RDONLY | os.O_DIRECTORY)
            ns = os.open(f'/proc/{pid}/ns/mnt', os.O_RDONLY)
            target = os.open(f'/proc/{pid}/root{dest}', os.O_PATH)
            info = os.fstat(target)
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != account.pw_uid:
                raise RuntimeError('Original socket ownership changed')
            libc = ctypes.CDLL(None, use_errno=True)
            if libc.setns(ns, 0x20000) != 0:
                raise OSError(ctypes.get_errno(), 'setns')
            os.fchdir(proc)
            if libc.mount(None, f'self/fd/{target}'.encode(), None, 1 << 18, None) != 0:
                raise OSError(ctypes.get_errno(), 'make private')
            os._exit(0)
        except BaseException as error:
            print(error, flush=True)
            os._exit(1)
    if os.waitpid(child, 0)[1]:
        raise RuntimeError('Failed to isolate original mount for restoration')

if mode == 'snapshot':
    if Path('/run/rootless-docker/control.sock').exists() or run(docker + ['ps', '-q']).stdout.strip():
        raise RuntimeError('Refusing an existing management daemon or running rootless workloads')
    if not unit.read_text().startswith('# Managed by rootless-docker\n'):
        raise RuntimeError('Service not managed by rootless-docker')
    root.mkdir(mode=0o700)
    state = {'files': [], 'inventory': inventory(), 'mounts': [],
             'active': run(service + ['is-active', 'docker-rootless.service'], False).returncode == 0,
             'enabled': run(service + ['is-enabled', 'docker-rootless.service'], False).returncode == 0,
             'runtime_exists': Path('/run/rootless-docker').exists(),
             'state_exists': Path('/var/lib/rootless-docker').exists(),
             'libexec_exists': Path('/usr/local/libexec/rootless-docker').exists()}
    for i, path in enumerate([unit, base / 'launch.sh', base / 'config/daemon.json', base / 'config/rootlesskit.json',
                              Path('/var/lib/rootless-docker/bindings.json'), Path('/usr/local/libexec/rootless-docker/supervisor')]):
        entry = {'path': str(path), 'exists': path.exists()}
        if entry['exists']:
            info = path.stat()
            entry.update(uid=info.st_uid, gid=info.st_gid, mode=info.st_mode & 0o777)
            shutil.copyfile(path, root / str(i)); os.chmod(root / str(i), 0o600)
        state['files'].append(entry)
    source = (base / 'run/docker.sock').stat()
    ids = run(['docker', 'ps', '-q', '--no-trunc']).stdout.split()
    for item in json.loads(run(['docker', 'inspect'] + ids).stdout) if ids else []:
        pid = item['State']['Pid']
        for line in Path(f'/proc/{pid}/mountinfo').read_text().splitlines():
            dest = line.split()[4].replace('\\040', ' ').replace('\\134', '\\')
            try:
                st = Path(f'/proc/{pid}/root{dest}').stat()
            except OSError:
                continue
            if (st.st_dev, st.st_ino) == (source.st_dev, source.st_ino):
                state['mounts'].append([item['Id'], dest])
    (root / 'state.json').write_text(json.dumps(state))
    print('Saved service/configuration, data inventory and original socket mounts')
elif mode == 'check-stopped':
    properties = dict(line.split('=', 1) for line in run(service + ['show', 'docker-rootless.service', '-p', 'ActiveState', '-p', 'MainPID']).stdout.splitlines())
    if properties.get('ActiveState') not in ('inactive', 'failed') or properties.get('MainPID') != '0' or run(docker + ['info'], False).returncode == 0:
        raise RuntimeError('host rootless dockerd is still running')
    print('host dockerd stopped')
elif mode == 'inventory':
    assert inventory() == json.loads((root / 'state.json').read_text())['inventory'], 'Existing rootless data inventory changed'
    print('Existing rootless data inventory preserved')
elif mode == 'restart':
    run(service + ['restart', 'docker-rootless.service'])
    print('Restarted host dockerd outside the management API')
elif mode == 'restore':
    state = json.loads((root / 'state.json').read_text())
    run(service + ['stop', 'docker-rootless.service'])
    for i, entry in enumerate(state['files']):
        path = Path(entry['path'])
        if entry['exists']:
            temp = path.with_name(path.name + '.test-restore')
            shutil.copyfile(root / str(i), temp)
            os.chown(temp, entry['uid'], entry['gid']); os.chmod(temp, entry['mode'])
            os.replace(temp, path)
        else:
            path.unlink(missing_ok=True)
    run(service + ['daemon-reload'])
    run(service + ['enable' if state['enabled'] else 'disable', 'docker-rootless.service'])
    if state['active']:
        run(service + ['start', 'docker-rootless.service'])
        for container, dest in state['mounts']:
            make_original_mount_private(container, dest)
            run([original, 'add', container, '--socket-path', dest])
            make_original_mount_private(container, dest)
        assert inventory() == state['inventory'], 'Restored inventory differs'
    for key, path in [('runtime_exists', '/run/rootless-docker'), ('state_exists', '/var/lib/rootless-docker'), ('libexec_exists', '/usr/local/libexec/rootless-docker')]:
        if not state[key] and Path(path).exists():
            shutil.rmtree(path)
    shutil.rmtree(root)
    print('Restored prior service, configuration and socket mounts; data preserved')
else:
    raise RuntimeError('Unknown fixture operation')
