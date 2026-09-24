"""Opt-in docker exec cleanup; only disposable, labelled fixtures are mutated."""
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
    token = 'project-alpha-cleanup-test-' + uuid.uuid4().hex[:10]

    def docker(*args, check=True):
        result = subprocess.run(['docker', '--host', 'unix:///var/run/docker.sock', *args],
                                text=True, capture_output=True, timeout=60)
        if check and result.returncode:
            raise RuntimeError(f'docker {args[0]} failed: {result.stderr}')
        return result.stdout.strip()

    def remove_container(name):
        docker('rm', '-f', name, check=False)

    base = docker('image', 'inspect', '--format', '{{.Id}}',
                  os.environ.get('PROJECT_ALPHA_CLEANUP_TEST_IMAGE', 'python:3.12-slim'))
    docker_root = docker('info', '--format', '{{.DockerRootDir}}')
    limits = ['--pull=never', '--runtime=runc', '--network=none', '--cap-drop=ALL',
              '--security-opt=no-new-privileges', '--pids-limit=64',
              '--label', 'project-alpha.cleanup-test=' + token]
    with tempfile.TemporaryDirectory(prefix=token) as directory, contextlib.ExitStack() as stack:
        binary = Path(directory) / 'cleanup-helper.test'
        subprocess.run(['go', 'test', '-c', '-o', str(binary), './internal/storage'],
                       cwd=repo, check=True, timeout=180)
        seed = token + '-seed'
        stack.callback(remove_container, seed)
        docker('run', '--name', seed, *limits, '--entrypoint=/bin/sh', base, '-ec',
               'rm -rf /root/.cache; mkdir -p /root/.cache/pip /root/.cache/mounted /outside; '
               'printf image-pip > /root/.cache/pip/image-file; '
               'printf image-cache > /root/.cache/lower-only; '
               'printf keep > /outside/keep')
        fixture_image = docker('commit', seed)
        stack.callback(lambda: docker('image', 'rm', fixture_image, check=False))
        target = token + '-target'
        stack.callback(remove_container, target)
        setup = '''import os,socket,time
os.makedirs('/root/.cache/ipc',exist_ok=True)
os.makedirs('/tmp/tmux-0',mode=0o700,exist_ok=True)
os.chmod('/tmp',0o1777)
for p in ['/root/.cache/pip/image-file']:
 os.unlink(p)
os.rmdir('/root/.cache/pip')
with open('/root/.cache/payload','wb') as f:f.write(b'x'*(8*1024*1024))
os.mkfifo('/root/.cache/fifo')
os.mknod('/root/.cache/ipc/null',0o20600,os.makedev(1,3))
os.symlink('/outside','/root/.cache/link')
os.symlink('/outside','/root/parent-link')
a=socket.socket(socket.AF_UNIX);a.bind('/root/.cache/ipc/socket');a.listen(10)
b=socket.socket(socket.AF_UNIX);b.bind('/tmp/tmux-0/default');b.listen(10)
open('/ready','w').close()
time.sleep(600)
'''
        docker('run', '-d', '--name', target, *limits, '--cap-add=MKNOD',
               '--entrypoint=python3', fixture_image, '-I', '-S', '-c', setup)
        docker('exec', target, '/bin/sh', '-ec', 'while [ ! -f /ready ]; do sleep 0.05; done')
        cid = docker('inspect', '--format', '{{.Id}}', target)
        graph = json.loads(docker('inspect', '--format', '{{json .GraphDriver.Data}}', target))
        upper = graph['UpperDir']
        physical_cache = upper + '/root/.cache'

        def raw(*args):
            return docker('run', '--rm', *limits, '--read-only',
                          '--mount', f'type=bind,src={upper},dst=/upper,readonly',
                          '--entrypoint=/usr/bin/' + args[0], fixture_image, *args[1:])

        marker = raw('stat', '-c', '%F %t:%T %i', '/upper/root/.cache/pip')
        assert marker.startswith('character special file 0:0 '), marker
        before = int(raw('du', '-s', '-B1', '/upper').split()[0])
        print('fixture: whiteout, listening sockets, char device, FIFO, 8 MiB data', flush=True)

        def launch(request):
            proc = subprocess.Popen([str(binary), '-test.run=^TestCleanupIntegrationProcess$'],
                                    stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                    stderr=subprocess.PIPE, text=True,
                                    env={**os.environ, 'PROJECT_ALPHA_CLEANUP_INTEGRATION_HELPER': '1'})
            events = queue.Queue()

            def read_events():
                for line in proc.stdout:
                    events.put(line)
                events.put(None)

            reader = threading.Thread(target=read_events, daemon=True)
            reader.start()

            def event():
                line = events.get(timeout=30)
                if line is None:
                    raise AssertionError('helper exited early: ' + proc.stderr.read())
                return json.loads(line)

            assert event()['type'] == 'ready'
            proc.stdin.write(json.dumps(request) + '\n')
            proc.stdin.flush()
            return proc, reader, event

        def cleanup(paths, container_id=cid, layer=upper):
            request = dict(paths=paths, containers=[dict(id=container_id, upper=layer)],
                           protected_roots=[layer], protected_trees=[], docker_root=docker_root)
            proc, reader, event = launch(request)
            try:
                results = []
                while True:
                    item = event()
                    if item['type'] == 'done':
                        break
                    assert item['type'] == 'result'
                    results.append(item)
                assert proc.wait(timeout=10) == 0, proc.stderr.read()
                assert [item['path'] for item in results] == paths
                return results
            finally:
                proc.stdin.close()
                if proc.poll() is None:
                    proc.kill()
                proc.wait(timeout=10)
                reader.join(timeout=5)
                proc.stdout.close()
                proc.stderr.close()

        host = Path(directory) / 'host-cache'
        host.mkdir()
        (host / 'keep').touch()
        for label, options in [('read-only', ['--read-only']),
                               ('nested mount', ['--mount', f'type=bind,src={directory},dst=/root/.cache/mounted,readonly'])]:
            name = token + '-' + label.replace(' ', '-')
            stack.callback(remove_container, name)
            docker('run', '-d', '--name', name, *limits, *options,
                   '--entrypoint=/bin/sleep', fixture_image, '600')
            other_id = docker('inspect', '--format', '{{.Id}}', name)
            other_upper = json.loads(docker('inspect', '--format', '{{json .GraphDriver.Data}}', name))['UpperDir']
            result = cleanup([str(host), other_upper + '/root/.cache'], other_id, other_upper)
            assert all(x['code'] == 'preflight_failed' for x in result), result
            assert ('只读' if label == 'read-only' else 'protected path or mount') in result[0]['error'], result
            assert (host / 'keep').exists()
            print('protected whole batch before deletion:', label, flush=True)
            docker('stop', '-t', '1', name)
            result = cleanup([other_upper + '/root/.cache'], other_id, other_upper)[0]
            assert result['code'] == 'preflight_failed', result

        result = cleanup([upper + '/root/parent-link/keep'])[0]
        assert result['code'] == 'preflight_failed', result
        assert docker('exec', target, 'cat', '/outside/keep') == 'keep'
        assert cleanup([physical_cache], layer=upper + '-changed')[0]['code'] == 'preflight_failed'
        identity = docker('exec', target, 'stat', '-c', '%d:%i:%a:%u:%g', '/root/.cache')
        result = cleanup([physical_cache])[0]
        assert result['status'] == 'deleted' and result['skipped_sockets'] == 1 and result['skipped_char_devices'] == 1, result
        assert docker('exec', target, 'stat', '-c', '%d:%i:%a:%u:%g', '/root/.cache') == identity
        verify = '''import os,socket,stat
assert os.listdir('/root/.cache')==['ipc']
assert stat.S_ISCHR(os.lstat('/root/.cache/ipc/null').st_mode)
s=socket.socket(socket.AF_UNIX);s.connect('/root/.cache/ipc/socket');s.close()
assert open('/outside/keep').read()=='keep'
open('/root/.cache/new-file','w').close()
'''
        docker('exec', target, 'python3', '-I', '-S', '-c', verify)
        after = int(raw('du', '-s', '-B1', '/upper').split()[0])
        assert before - after >= 8 * 1024 * 1024, (before, after)
        assert raw('stat', '-c', '%F %t:%T %i', '/upper/root/.cache/pip') == marker
        assert cleanup([physical_cache])[0]['skipped_sockets'] == 1
        tmp_identity = docker('exec', target, 'stat', '-c', '%d:%i:%a:%u:%g', '/tmp')
        assert cleanup([upper + '/tmp'])[0]['skipped_sockets'] == 1
        assert docker('exec', target, 'stat', '-c', '%d:%i:%a:%u:%g', '/tmp') == tmp_identity
        docker('exec', target, 'python3', '-I', '-S', '-c',
               "import socket;s=socket.socket(socket.AF_UNIX);s.connect('/tmp/tmux-0/default')")
        docker('exec', '-u', '65534:65534', target, '/bin/sh', '-ec',
               'touch /tmp/new-file; mkdir /tmp/new-dir; mkfifo /tmp/new-fifo')
        docker('run', '--rm', *limits, '--read-only', '--entrypoint=/bin/sh', fixture_image,
               '-ec', 'test -f /root/.cache/pip/image-file; test -f /root/.cache/lower-only; test -f /outside/keep')
        print(f'PASS: docker exec; {before - after} bytes released; sockets/char devices and parents preserved; '
              'whiteout/image unchanged; /tmp writable as non-root', flush=True)

        # Keep a direct real exec worker idle after its preflight, then close its
        # pipe. Docker must terminate that worker without an orphan deletion.
        program = (repo / 'internal/storage/cleanup_container.py').read_text()
        child = subprocess.Popen(['docker', '--host', 'unix:///var/run/docker.sock',
                                  'exec', '-i', '-u', '0:0', '-w', '/', cid,
                                  'python3', '-I', '-S', '-u', '-c', program],
                                 stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            child.stdin.write(json.dumps(dict(upper=upper, paths=['/root/.cache'], protected=[])) + '\n')
            child.stdin.flush()
            assert json.loads(child.stdout.readline())['type'] == 'prepared'
            child.stdin.close()
            assert child.wait(timeout=10) == 0, child.stderr.read()
        finally:
            if child.poll() is None:
                child.kill()
            child.wait(timeout=10)
            child.stdout.close()
            child.stderr.close()
        print('PASS: real docker exec worker exits on stdin disconnect', flush=True)


if __name__ == '__main__':
    main()
