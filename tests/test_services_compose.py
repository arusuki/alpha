"""Smoke-test the built dram-bw Compose service with mock PMU data.

Requires Docker access, project-alpha-dram-bw:local, and the C client build.
Uses an isolated Compose project and socket directory; never starts Tetragon.
"""
import csv
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import tempfile
import time

repo = Path(__file__).resolve().parents[1]
with tempfile.TemporaryDirectory(prefix='alpha-services-') as directory:
    root = Path(directory)
    project = root.name
    override = root / 'override.json'
    override.write_text(json.dumps({'services': {'dram-bw': {
        'container_name': project,
        'volumes': [f'{root}:/run/dram-bw'],
        'command': ['--backend', 'mock', '--interval-us', '1000', '--peak-gbps', '3',
                    '--socket', '/run/dram-bw/control.sock', '--socket-mode', '0660'],
    }}}))
    env = dict(os.environ, DRAM_BW_GID=str(os.getgid()))
    compose = ['docker', 'compose', '-p', project, '-f', str(repo / 'deploy/services.yaml'), '-f', str(override)]

    def run(args, **kwargs):
        return subprocess.run(args, env=env, check=True, text=True, capture_output=True, timeout=45, **kwargs).stdout

    def consume():
        path = root / 'control.sock'
        for _ in range(100):
            if path.exists():
                break
            time.sleep(.05)
        assert path.is_socket(), run(['docker', 'logs', project])
        assert stat.S_IMODE(path.stat().st_mode) == 0o660
        assert path.stat().st_gid == os.getgid()
        result = run([str(repo / 'ctools/dram-bw/build/dram-bw-consume'), str(path), '3'])
        rows = list(csv.DictReader(io.StringIO(result)))
        assert len(rows) == 3, result
        assert all(float(row['total_GBps']) == 1.5 for row in rows), result

    try:
        run(compose + ['up', '-d', '--no-build', '--no-deps', 'dram-bw'])
        inspection = json.loads(run(['docker', 'inspect', project]))[0]
        assert inspection['HostConfig']['Privileged']
        assert inspection['HostConfig']['NetworkMode'] == 'none'
        assert inspection['Config']['Labels']['project-alpha.service'] == 'dram-bw'
        consume()
        template = '{"name":{{json (.Label "project-alpha.service")}},"state":{{json .State}},"detail":{{json .Status}}}'
        args = ['docker', 'container', 'ls', '--all', '--filter', f'label=com.docker.compose.project={project}', '--filter', 'label=project-alpha.service', '--format', template]
        assert json.loads(run(args))['state'] == 'running'
        run(compose + ['stop', 'dram-bw'])
        assert not (root / 'control.sock').exists(), 'normal stop must clean up socket'
        assert json.loads(run(args))['state'] == 'exited'
        run(compose + ['start', 'dram-bw'])
        consume()
        print('Compose: privileged daemon, group access, host client samples, live/stopped states and restart passed')
    finally:
        run(compose + ['down', '--timeout', '5'])
