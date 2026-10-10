"""Verify namespace entry and lease supervision in disposable containers.

Requires Docker and local alpine:latest. No host PID namespace, host filesystem
or host services are used. ROOTLESS_TEST_BINARY names a static rootless Go test
binary built with CGO_ENABLED=0 go test -c ./internal/rootless.
"""
import os
from pathlib import Path
import subprocess
import uuid

binary = str(Path(os.environ['ROOTLESS_TEST_BINARY']).resolve())
name = 'alpha-rootless-ns-' + uuid.uuid4().hex[:10]
mount = ['--mount', f'type=bind,src={binary},dst=/rootless.test,readonly']

def run(args):
    result = subprocess.run(args, text=True, capture_output=True, timeout=30)
    if result.returncode:
        raise RuntimeError(f'{args}: {result.stdout}\n{result.stderr}')
    return result.stdout

print(run(['docker', 'run', '--rm', '--network', 'none', *mount,
           '-e', 'ROOTLESS_LEASE_INTEGRATION=1', 'alpine:latest', '/rootless.test',
           '-test.run=^TestRealUserSupervisorLease$', '-test.v']), end='')
try:
    run(['docker', 'run', '-d', '--name', name, '--network', 'none', 'alpine:latest', 'sleep', 'infinity'])
    run(['docker', 'exec', name, 'touch', '/rootless-entry-marker'])
    print(run(['docker', 'run', '--rm', '--privileged', '--network', 'none', '--pid', 'container:' + name,
               *mount, '-e', 'ROOTLESS_ENTRY_INTEGRATION=1', 'alpine:latest', '/rootless.test',
               '-test.run=^TestEnterHostProcess$', '-test.v']), end='')
finally:
    subprocess.run(['docker', 'rm', '-f', name], capture_output=True, timeout=30)
