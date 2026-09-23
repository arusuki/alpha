"""Opt-in real OverlayFS cleanup; only disposable, labelled Docker fixtures are mutated."""
import contextlib
import json
import os
from pathlib import Path
import queue
import subprocess
import tempfile
import threading
import uuid


def main():
    if os.environ.get('PROJECT_ALPHA_TEST_OVERLAY_CLEANUP') != '1':
        print('SKIP: set PROJECT_ALPHA_TEST_OVERLAY_CLEANUP=1 for disposable Docker tests')
        return
    repo = Path(__file__).resolve().parents[1]
    token = 'project-alpha-cleanup-test-' + uuid.uuid4().hex[:12]

    def docker(*args, check=True):
        result = subprocess.run(['docker', *args], text=True, capture_output=True, timeout=60)
        if check and result.returncode:
            raise RuntimeError(f'docker {args[0]} failed: {result.stderr}')
        return result.stdout.strip()

    def remove_container(name):
        docker('rm', '-f', name, check=False)

    base = docker('image', 'inspect', '--format', '{{.Id}}',
                  os.environ.get('PROJECT_ALPHA_CLEANUP_TEST_IMAGE', 'ubuntu:latest'))
    limits = ['--pull=never', '--runtime=runc', '--network=none', '--cap-drop=ALL',
              '--security-opt=no-new-privileges', '--pids-limit=64',
              '--label', 'project-alpha.cleanup-test=' + token]
    with tempfile.TemporaryDirectory(prefix=token) as directory, contextlib.ExitStack() as stack:
        binary = Path(directory) / 'cleanup-helper'
        subprocess.run(['go', 'build', '-ldflags=-linkmode=external -extldflags=-static',
                        '-o', str(binary), './cmd/project-alpha'], cwd=repo,
                       env={**os.environ, 'CGO_ENABLED': '1'}, check=True, timeout=180)
        seed = token + '-seed'
        stack.callback(remove_container, seed)
        docker('run', '--name', seed, *limits, '--entrypoint=/bin/sh', base, '-ec',
               'mkdir -p /root/.cache/pip /outside; '
               'printf image-pip > /root/.cache/pip/image-file; '
               'printf image-cache > /root/.cache/lower-only; '
               'printf keep > /outside/keep')
        fixture_image = docker('commit', seed)
        stack.callback(lambda: docker('image', 'rm', fixture_image, check=False))
        target = token + '-target'
        stack.callback(remove_container, target)
        docker('run', '-d', '--name', target, *limits, '--entrypoint=/bin/sh', fixture_image,
               '-ec', 'rm -rf /root/.cache/pip; '
               'dd if=/dev/zero of=/root/.cache/payload bs=1048576 count=8 status=none; '
               'mkfifo /root/.cache/fifo; ln -s /outside /root/.cache/link; '
               'ln -s /outside /root/parent-link; mkdir /root/.cache/mounted; '
               'touch /ready; exec sleep 600')
        # A blocking exec waits for initialization without racing the fixture's rm/dd.
        docker('exec', target, '/bin/sh', '-ec', 'while [ ! -f /ready ]; do sleep 0.05; done')
        graph = json.loads(docker('inspect', '--format', '{{json .GraphDriver.Data}}', target))
        upper, merged = graph['UpperDir'], graph['MergedDir']
        physical_cache = upper + '/root/.cache'

        def raw(*args):
            return docker('run', '--rm', *limits, '--read-only',
                          '--mount', f'type=bind,src={upper},dst=/upper,readonly',
                          '--entrypoint=/usr/bin/' + args[0], fixture_image, *args[1:])

        assert raw('stat', '-c', '%F %t:%T %b', '/upper/root/.cache/pip') == 'character special file 0:0 0'
        before = int(raw('du', '-s', '-B1', '/upper').split()[0])
        print('fixture: real 0:0 whiteout plus 8 MiB cache', flush=True)
        helper_count = 0

        def cleanup(paths, *, mount_view=True, read_only=False, nested_mount=False):
            nonlocal helper_count
            helper_count += 1
            helper_name = token + '-helper-' + str(helper_count)
            args = ['docker', 'run', '--rm', '-i', '--name', helper_name, *limits, '--read-only',
                    '--mount', f'type=bind,src={binary},dst=/cleanup-helper,readonly']
            if mount_view:
                args += ['--mount', f'type=bind,src={merged},dst=/fixture' + (',readonly' if read_only else '')]
            if nested_mount:
                args += ['--mount', f'type=bind,src={directory},dst=/fixture/root/.cache/mounted,readonly']
            args += ['--entrypoint=/cleanup-helper', fixture_image, 'cleanup-helper']
            request = dict(paths=paths, writable_layers=[upper], protected_roots=[upper], protected_trees=[])
            with tempfile.TemporaryFile(mode='w+') as stderr:
                proc = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=stderr, text=True)
                events = queue.Queue()

                def read_events():
                    for line in proc.stdout:
                        events.put(line)
                    events.put(None)

                reader = threading.Thread(target=read_events, daemon=True)
                reader.start()

                def event():
                    line = events.get(timeout=20)
                    if line is None:
                        stderr.seek(0)
                        raise AssertionError('helper exited early: ' + stderr.read())
                    return json.loads(line)

                try:
                    assert event()['type'] == 'ready'
                    proc.stdin.write(json.dumps(request) + '\n')
                    proc.stdin.flush()  # Keep the control pipe open until done.
                    results = []
                    while True:
                        item = event()
                        if item['type'] == 'done':
                            break
                        assert item['type'] == 'result'
                        results.append(item)
                    assert proc.wait(timeout=10) == 0
                    assert [item['path'] for item in results] == paths
                    return results
                finally:
                    proc.stdin.close()
                    remove_container(helper_name)
                    try:
                        proc.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait(timeout=5)
                    reader.join(timeout=5)
                    proc.stdout.close()

        for label, options in [('unmounted', dict(mount_view=False)),
                               ('read-only', dict(read_only=True)),
                               ('nested mount', dict(nested_mount=True))]:
            result = cleanup([physical_cache], **options)[0]
            assert result['status'] == 'failed' and result['code'] == 'preflight_failed', result
            assert raw('stat', '-c', '%s', '/upper/root/.cache/payload') == '8388608'
            print('protected before deletion:', label, flush=True)

        result = cleanup([upper + '/root/parent-link/keep'])[0]
        assert result['status'] == 'failed', result
        assert docker('exec', target, 'cat', '/outside/keep') == 'keep'
        print('parent symlink rejected', flush=True)

        result = cleanup([physical_cache])[0]
        assert result['status'] == 'deleted', result
        docker('exec', target, '/bin/sh', '-ec', 'test ! -e /root/.cache; test -f /outside/keep')
        marker = raw('stat', '-c', '%F %t:%T %i', '/upper/root/.cache')
        assert marker.startswith('character special file 0:0 '), marker
        after = int(raw('du', '-s', '-B1', '/upper').split()[0])
        assert before - after >= 8 * 1024 * 1024, (before, after)
        assert cleanup([physical_cache])[0]['status'] == 'deleted'
        assert raw('stat', '-c', '%F %t:%T %i', '/upper/root/.cache') == marker
        docker('run', '--rm', *limits, '--read-only', '--entrypoint=/bin/sh', fixture_image,
               '-ec', 'test -f /root/.cache/pip/image-file; test -f /root/.cache/lower-only; test -f /outside/keep')
        print(f'PASS: cache removed through merged view; {before - after} bytes released; '
              'whiteout preserved on retry; image and outside files unchanged', flush=True)


if __name__ == '__main__':
    main()
