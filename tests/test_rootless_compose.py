"""Opt-in real-host test; snapshots/restores an idle existing rootless service.

Set ROOTLESS_HOST_INTEGRATION=1, ROOTLESS_ORIGINAL_BINARY (original rootless CLI)
and ROOTLESS_CLIENT_BINARY (new CLI), both absolute host paths. Requires built
project-alpha-rootless-docker:local and local alpine:latest/docker:28-cli images.
Refuses running rootless workloads. Tetragon/DRAM and existing data are preserved.
"""
from contextlib import contextmanager
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import time

if os.getenv('ROOTLESS_HOST_INTEGRATION') != '1':
    print('Skipped: set ROOTLESS_HOST_INTEGRATION=1 on an idle development host')
    raise SystemExit(0)
repo = Path(__file__).resolve().parents[1]
original = str(Path(os.environ['ROOTLESS_ORIGINAL_BINARY']).resolve())
client = str(Path(os.environ['ROOTLESS_CLIENT_BINARY']).resolve())

def run(args, check=True):
    result = subprocess.run(args, check=False, text=True, capture_output=True, timeout=150)
    if check and result.returncode:
        raise RuntimeError(f'{args}: {result.stdout}\n{result.stderr}')
    return result

class StartupFailure(RuntimeError):
    pass

def wait(check, label, timeout=90):
    deadline = time.monotonic() + timeout
    last = None
    while time.monotonic() < deadline:
        try:
            if check():
                return
        except StartupFailure:
            raise
        except Exception as error:
            last = error
        time.sleep(.3)
    raise RuntimeError(f'Timed out: {label}: {last}')

@contextmanager
def test_directory():
    directory = tempfile.mkdtemp(prefix='alpha-rootless-')
    try:
        yield directory
    finally:
        if (Path(directory) / 'backup').exists():
            print(f'Recovery backup retained: {directory}/backup', flush=True)
        else:
            shutil.rmtree(directory)

with test_directory() as temp:
    directory = Path(temp)
    name, target = directory.name, directory.name + '-target'
    backup = str(directory / 'backup')
    override = directory / 'compose.json'
    override.write_text(json.dumps({'services': {'rootless-docker': {
        'container_name': name, 'restart': 'no',
        'command': ['daemon', '--host-namespaces', '--socket-gid', str(os.getgid())]}}}))
    compose = ['docker', 'compose', '-p', name, '-f', str(repo / 'deploy/services.yaml'), '-f', str(override)]
    def host(mode):
        return run(['docker', 'run', '--rm', '--privileged', '--pid=host', '--network=host',
                    '-v', '/:/host', 'alpine:latest', 'chroot', '/host', '/usr/bin/python3',
                    str(repo / 'tests/rootless_host_fixture.py'), mode, backup, original])
    def call(*args, check=True):
        return run([client, *args], check=check)
    def ready():
        result = call('status', check=False)
        if result.returncode:
            state = run(['docker', 'inspect', '--format', '{{.State.Running}}', name], check=False)
            if state.stdout.strip() == 'false':
                raise StartupFailure('Management container exited before becoming ready')
        return result.returncode == 0
    def connected():
        return run(['docker', 'exec', target, 'docker', '--host', 'unix:///run/test-rootless.sock',
                    'info', '--format', '{{.ID}}'], check=False).returncode == 0
    snapped = False
    try:
        print(host('snapshot').stdout, end='', flush=True); snapped = True
        run(compose + ['up', '-d', '--no-build', '--no-deps', 'rootless-docker'])
        wait(ready, 'daemon startup')
        inspection = json.loads(run(['docker', 'inspect', name]).stdout)[0]
        assert inspection['HostConfig']['Privileged'] and inspection['HostConfig']['PidMode'] == 'host'
        run(['docker', 'run', '-d', '--name', target, '--entrypoint', 'tail', 'docker:28-cli', '-f', '/dev/null'])
        run(['docker', 'exec', target, 'sh', '-c', 'printf preserve > /run/preserve'])
        assert call('add', target, '--socket-path', '/run/preserve', check=False).returncode != 0
        assert run(['docker', 'exec', target, 'cat', '/run/preserve']).stdout == 'preserve'
        call('remove', target, '--socket-path', '/run/preserve')
        call('add', target, '--socket-path', '/run/test-rootless.sock'); wait(connected, 'attachment')
        first = json.loads(call('list').stdout)[0]['receipt']['mount_id']
        call('add', target, '--socket-path', '/run/test-rootless.sock')
        assert json.loads(call('list').stdout)[0]['receipt']['mount_id'] == first
        assert 'rootless' in call('docker', 'info', '--format', '{{json .SecurityOptions}}').stdout
        call('remove', target, '--socket-path', '/run/test-rootless.sock'); assert not connected()
        assert call('remove', target, '--socket-path', '/run/test-rootless.sock', check=False).returncode != 0
        call('add', target, '--socket-path', '/run/test-rootless.sock')
        run(['docker', 'restart', target]); wait(connected, 'target restart')
        call('restart'); wait(connected, 'dockerd restart')
        print(host('restart').stdout, end='', flush=True)
        wait(connected, 'socket recovery after external dockerd restart')
        processes = run(['docker', 'top', name, '-eo', 'pid,args']).stdout
        assert ' events' not in processes, 'event subscriptions spawned a Docker CLI process'
        print(host('inventory').stdout, end='', flush=True)
        run(compose + ['stop', 'rootless-docker']); wait(lambda: host('check-stopped'), 'normal stop')
        assert not connected()
        assert run(['docker', 'inspect', '--format', '{{.State.ExitCode}}', name]).stdout.strip() == '0'
        run(compose + ['start', 'rootless-docker']); wait(ready, 'manager restart'); wait(connected, 'saved binding')
        run(['docker', 'kill', '--signal', 'KILL', name]); wait(lambda: host('check-stopped'), 'SIGKILL lease EOF')
        run(compose + ['start', 'rootless-docker']); wait(ready, 'restart after kill'); wait(connected, 'stale mount recovery')
        print(host('inventory').stdout, end='', flush=True)
        call('remove', target, '--socket-path', '/run/test-rootless.sock')
        print('PASS: Compose, protocol, add/remove, idempotency, target/dockerd/external restart, HTTP event streams, normal stop, SIGKILL and data preservation', flush=True)
    except Exception:
        logs = run(['docker', 'logs', '--tail', '60', name], check=False)
        print(logs.stdout + logs.stderr, flush=True)
        raise
    finally:
        run(compose + ['down', '--timeout', '120'], check=False)
        run(['docker', 'rm', '-f', target], check=False)
        if snapped:
            print(host('restore').stdout, end='', flush=True)
