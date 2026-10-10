"""Real DRAM authorization sockets and event recovery, using isolated containers.

DRAM_TEST_BIN_DIR must contain static rootless-docker, dram-bwd and
 dram-bw-consume binaries. Uses the release image recipe and cached alpine.
No existing node service or user container is modified.
"""
import csv
import io
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time

repo = Path(__file__).resolve().parents[1]
binaries = Path(os.environ['DRAM_TEST_BIN_DIR']).resolve()

def run(args, **kwargs):
    return subprocess.run(args, check=True, text=True, capture_output=True, timeout=60, **kwargs).stdout

def eventually(check):
    last = None
    for _ in range(100):
        try:
            if check():
                return
        except (subprocess.CalledProcessError, FileNotFoundError) as exc:
            last = exc
        time.sleep(.1)
    raise AssertionError(f'condition did not become true: {last}')

with tempfile.TemporaryDirectory(prefix='dram-access-') as directory:
    root = Path(directory)
    name = root.name
    image = f'project-alpha-dram-access-test:{name}'
    manager, target = name+'-manager', name+'-target'
    recipe = json.loads((repo/'deploy/updater-services.json').read_text())['services'][1]['dockerfile']
    try:
        run(['docker','build','--network=none','--pull=false','-t',image,'-f','-',str(binaries)],input=recipe)
        run(['docker','run','-d','--name',manager,'--privileged','--pid=host','--userns=host','--network=none',
             '--user',f'0:{os.getgid()}',
             '-v',f'{root}/samples:/run/dram-bw',
             '-v',f'{root}/manager:/run/dram-bw-manager',
             '-v',f'{root}/state:/var/lib/dram-bw-manager',
             image,'--backend','mock','--interval-us','1000','--socket-mode','0660'])
        run(['docker','run','-d','--name',target,'--network=none',
             '-v',f'{binaries}/dram-bw-consume:/consume:ro','alpine:latest','sleep','infinity'])
        target_id = run(['docker','inspect','--format','{{.Id}}',target]).strip()
        socket = root/'manager/control.sock'
        env = dict(os.environ,ROOTLESS_CONTROL_SOCKET=str(socket))
        def control(*args):
            return run([str(binaries/'rootless-docker'),*args],env=env)
        eventually(lambda: socket.is_socket() and '验证' in control('status'))
        assert json.loads(control('bindings')) == []
        assert run(['docker','exec',target,'sh','-c','test ! -S /run/dram.sock && echo absent']).strip()=='absent'
        control('add',target_id,'--socket-path','/run/dram.sock')
        def mounted():
            entries=json.loads(control('bindings'))
            return len(entries)==1 and entries[0]['state']=='mounted'
        def consume():
            rows=list(csv.DictReader(io.StringIO(run(['docker','exec',target,'/consume','/run/dram.sock','3']))))
            assert len(rows)==3 and all(float(row['total_GBps'])==1.5 for row in rows),rows
        eventually(mounted);consume()
        assert run(['docker','exec',target,'sh','-c','test ! -e /run/dram-bw-manager/control.sock && echo isolated']).strip()=='isolated'
        control('add',target_id,'--socket-path','/run/dram.sock')
        assert len(json.loads(control('bindings')))==1
        run(['docker','restart',target]);eventually(mounted);consume()
        run(['docker','stop',target]);control('add',target_id,'--socket-path','/run/dram.sock')
        assert json.loads(control('bindings'))[0]['state']=='waiting'
        run(['docker','start',target]);eventually(mounted);consume()
        run(['docker','restart',manager]);eventually(mounted);consume()
        control('remove',target_id,'--socket-path','/run/dram.sock')
        assert json.loads(control('bindings'))==[]
        assert run(['docker','exec',target,'sh','-c','test ! -S /run/dram.sock && echo revoked']).strip()=='revoked'
        run(['docker','restart',target])
        assert run(['docker','exec',target,'sh','-c','test ! -S /run/dram.sock && echo revoked']).strip()=='revoked'
        print('DRAM real mounts: default deny, isolated sampling socket, samples, idempotence, target/service restart, stopped target and revocation passed')
    except Exception as exc:
        if isinstance(exc,subprocess.CalledProcessError):
            print(exc.stdout,exc.stderr,flush=True)
        subprocess.run(['docker','logs',manager],check=False,timeout=10)
        raise
    finally:
        subprocess.run(['docker','rm','-f',target,manager],capture_output=True,timeout=30)
        subprocess.run(['docker','image','rm',image],capture_output=True,timeout=30)
        # Only make this test's root-owned bind directories removable by its owner.
        run(['docker','run','--rm','--network=none','-v',f'{root}:/test','alpine:latest','chown','-R',f'{os.getuid()}:{os.getgid()}','/test'])
