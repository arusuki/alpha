"""Disposable two-worker Docker integration. See docs/interconnect.md for image and privileges."""
import contextlib
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import os
from pathlib import Path
import subprocess
import tempfile
import time
import uuid
import urllib.error
import urllib.request

IMAGE = os.environ.get('PROJECT_ALPHA_INTERCONNECT_IMAGE', 'alpha-interconnect-test:local')
ROOT = Path(__file__).resolve().parents[1]
HELPER = '/usr/local/libexec/project-alpha/alpha-interconnect-helper'
TOKEN = 'interconnect-fixture-token-for-isolated-workers'


def run(*args, text=None):
    p = subprocess.run(args, input=text, text=True, capture_output=True, timeout=120)
    if p.returncode:
        raise RuntimeError(f'{args}: {p.stderr[-1000:]} {p.stdout[-300:]}')
    return p.stdout.strip()


def wait(probe, description):
    error = ''
    for _ in range(100):
        try:
            if probe():
                return
        except (RuntimeError, OSError) as exc:
            error = str(exc)
        time.sleep(.25)
    raise AssertionError(f'{description}: {error}')


class Worker:
    def __init__(self, name, index):
        self.name = name
        self.internal = '192.0.2.'+str(index)
        self.subnet = f'10.240.{index-1}.0/24'
        self.index = index
        self.endpoint = 'unix:///run/alpha-fixture.sock' if index == 2 else 'unix:///var/run/docker.sock'

    def exec(self, *args, text=None):
        return run('docker', 'exec', '-i', self.name, *args, text=text)

    def container(self, name, *args):
        return self.exec('docker', 'exec', name, *args)

    def boot(self):
        # This fixture has no systemd. Run the installed ExecStartPre before
        # dockerd, including when no Docker chains or endpoint interfaces exist.
        listeners = '-H unix:///var/run/docker.sock'
        if self.index == 2:
            listeners += ' -H '+self.endpoint
        script = f'exec > /tmp/dockerd.log 2>&1; if [ -x {HELPER} ]; then {HELPER} guard || exit 1; fi; exec dockerd {listeners} --storage-driver=vfs --exec-opt native.cgroupdriver=cgroupfs'
        run('docker','exec','-d',self.name,'sh','-c',script)
        wait(lambda: self.exec('docker','info','--format','{{.ID}}'), 'dockerd startup')
        self.exec('chgrp','nogroup',self.endpoint[len('unix://'):])

    def start_service(self):
        script = 'echo $$ > /qa/home/pid; exec /opt/alpha/project-alpha serve --worker --data-dir /qa/data --host 0.0.0.0 --port 8766 >> /qa/home/worker.log 2>&1'
        run('docker','exec','-d','--user','nobody','-e','HOME=/qa/home','-e','PROJECT_ALPHA_WORKER_TOKEN='+TOKEN,
            self.name,'sh','-c',script)
        wait(lambda: self.request(), 'worker startup')

    def request(self, body=None):
        req = urllib.request.Request('http://'+self.ip+':8766/api/containers/interconnect',
             data=None if body is None else json.dumps(body).encode(), headers={
             'Content-Type':'application/json', 'Authorization':'Bearer '+TOKEN, 'X-Alpha-Node':self.id,
             'X-Alpha-User':json.dumps(dict(id='e'*32,username='fixture',role='admin'))})
        try:
            return json.load(urllib.request.urlopen(req,timeout=45))
        except urllib.error.HTTPError as e:
            raise RuntimeError(e.read().decode()) from e

    def prepare(self, temp, archive, network):
        self.exec('mkdir','-p','/opt/alpha','/qa/home')
        for binary in ('project-alpha','alpha-interconnect-helper'):
            run('docker','cp',str(temp/binary),self.name+':/opt/alpha/'+binary)
        self.exec('sh','-c','printf \'#!/bin/sh\n[ "$1" = daemon-reload ]\n\' > /usr/bin/systemctl; chmod 755 /usr/bin/systemctl; mkdir -p /etc/sudoers.d')
        self.boot()
        with archive.open('rb') as f:
            subprocess.run(['docker','exec','-i',self.name,'docker','load'], stdin=f,check=True,stdout=subprocess.DEVNULL)
        self.exec('/opt/alpha/project-alpha','containers','import','--dry-run','--data-dir','/qa/data')
        self.exec('python3','-c',"""import sqlite3,sys
d=sqlite3.connect('/qa/data/platform.sqlite3')
d.execute("update container_settings set value=json_set(value,'$.endpoint',?)",(sys.argv[1],));d.commit()""",self.endpoint)
        self.id = self.exec('python3','-c',"import sqlite3;print(sqlite3.connect('/qa/data/platform.sqlite3').execute('select instance_id from service_identity').fetchone()[0])")
        self.ip = json.loads(run('docker','inspect',self.name))[0]['NetworkSettings']['Networks'][network]['IPAddress']
        daemon = self.exec('docker','info','--format','{{.ID}}')
        eps = []
        for i, name in enumerate(['one','two'] if self.index==1 else ['one']):
            self.exec('docker','run','-d','--restart','unless-stopped','--name',name,IMAGE,'sleep','infinity')
            c = json.loads(self.exec('docker','inspect',name))[0]
            raw = {k:c[k] for k in ('Id','Name','Image','Config','HostConfig','Mounts')}
            raw['Mounts'].sort(key=lambda m:m['Destination'])
            digest = hashlib.sha256(json.dumps(raw,sort_keys=True,separators=(',',':')).encode()).hexdigest()
            owner = f'user-{self.index}-{i}'
            self.exec('python3','-c',"""import sqlite3,sys
db=sqlite3.connect('/qa/data/platform.sqlite3')
db.execute("insert into managed_containers values(?,?,?,?,?,?,?,?,?,?,?)",
[sys.argv[1],sys.argv[6],sys.argv[2],sys.argv[3],sys.argv[4],'{"network":"bridge"}',sys.argv[5],'adopt','',1,1]);db.commit()""",c['Id'],daemon,name,owner,digest,self.endpoint)
            eps.append(dict(node=self.id,container=c['Id'],ip=f'10.240.{self.index-1}.{2+4*i}',internal_ip=self.internal,name=name,owner=owner))
        self.exec('chown','-R','nobody:nogroup','/qa')
        self.exec('/opt/alpha/project-alpha','interconnect','init','--user','nobody','--data-dir','/qa/data')
        self.start_service()
        return eps


def attach_lan(node, switch):
    # Keep the other worker's NIC and its static routes alive when this worker reboots.
    port='qa'+str(node.index)
    run('docker','run','--rm','--privileged','--network','container:'+node.name,'--pid','container:'+switch,
        IMAGE,'ip','link','add','alphaqa','type','veth','peer','name',port,'netns','1')
    run('docker','exec',switch,'ip','link','set',port,'master','lan')
    run('docker','exec',switch,'ip','link','set',port,'up')
    node.exec('ip','link','set','alphaqa','address',f'02:00:00:00:00:0{node.index}')
    node.exec('ip','addr','add',node.internal+'/30','dev','alphaqa')
    node.exec('ip','link','set','alphaqa','up')


def ref(e):
    return {k:e[k] for k in ('node','container')}


def check_stopped_endpoint(a, eps, revision):
    # The A/one -> B/one link is independent of the local A/two lifecycle.
    def applied_with_second_endpoint(running):
        ledger = json.loads(a.exec('cat', '/var/lib/project-alpha-interconnect/ledger.json'))
        sources = ledger['Sources']
        return (not ledger['Guarded'] and eps[0]['ip'] in sources
                and (eps[1]['ip'] in sources) == running)

    a.exec('docker','exec','-d','one','sh','-c', '''
while [ ! -e /tmp/stop-sampling ]; do
    ping -c 1 -W 1 "$1" >/dev/null || touch /tmp/link-interrupted
    touch /tmp/link-sampled
    sleep .1
done
touch /tmp/sampling-done
''','sample',eps[2]['ip'])
    wait(lambda:a.container('one','ls','/tmp/link-sampled'), 'first independent ping')
    a.exec('docker','stop','--timeout','1','two')
    wait(lambda: applied_with_second_endpoint(False), 'stopped endpoint isolated')
    a.container('one','touch','/tmp/stop-sampling')
    wait(lambda:a.container('one','ls','/tmp/sampling-done'), 'last independent ping')
    a.container('one','test','!','-e','/tmp/link-interrupted')

    # Withdraw the connection while the endpoint still has its reservation.
    config=dict(protocol=1,revision=revision,pool='10.240.0.0/16',subnet=a.subnet,
                internal_ip=a.internal,node=a.id,endpoints=eps,connections=[dict(
                    id='2'*32,a=ref(eps[0]),b=ref(eps[2]),enabled=True,rules=[dict(
                        protocol='icmp',direction='both',first=0,last=0,enabled=True)])])
    assert a.request(dict(operation='apply',config=config))['applied']==revision
    assert a.request(dict(operation='check',config=config))['applied']==revision
    a.exec('docker','start','two')
    wait(lambda: applied_with_second_endpoint(True), 'restarted endpoint recovered')
    a.container('one','ping','-c','1','-W','1',eps[2]['ip'])
    assert a.container('one','sh','-c','ping -c 1 -W 1 "$1" >/dev/null && echo allowed || echo denied',
                       'ping',eps[1]['ip'])=='denied'


def check_pmtu(a, b, eps, both, rule):
    """Exercise generated and forwarded errors, then withdraw live TCP grants."""
    payload_hash = hashlib.sha256(bytes(range(256))*8192).hexdigest()
    transfer = """import hashlib,socket,sys
s=socket.create_connection((sys.argv[1],18000),timeout=8);s.settimeout(8)
greeting=b''
while len(greeting)<len(sys.argv[2]):
 part=s.recv(80)
 if not part:raise AssertionError('missing greeting')
 greeting+=part
assert greeting.decode()==sys.argv[2]
s.sendall(sys.argv[3].encode().ljust(8,b' '))
if sys.argv[3]=='upload':
 s.sendall(bytes(range(256))*8192);s.shutdown(socket.SHUT_WR)
 result=b''
 while True:
  part=s.recv(80)
  if not part:break
  result+=part
 print(result.decode())
else:
 h=hashlib.sha256()
 while True:
  part=s.recv(65536)
  if not part:break
  h.update(part)
 print(h.hexdigest())
"""

    def transfers(chain):
        for direction in ('upload', 'download'):
            # A previous transfer must not lower the MSS advertised by the
            # next handshake and hide that direction's feedback path.
            flush_pmtu()
            assert a.container('one','python3','-c',transfer,eps[2]['ip'],eps[0]['ip'],direction)==payload_hash, direction
            sender=a if direction=='upload' else b
            count=errors(sender,chain)
            if count==0:
                for node in (a,b):
                    print(node.exec('iptables-save','-c','-t','mangle'),flush=True)
            assert count>0, f'{chain} {direction} TCP PMTU feedback: {count}'
            print(f'{chain} {direction}: 2 MiB SHA256 verified, {count} TCP PMTU errors',flush=True)

    def flush_pmtu():
        for node in (a,b):
            node.exec('ip','route','flush','cache')
            pid=node.exec('docker','inspect','--format','{{.State.Pid}}','one')
            node.exec('nsenter','-t',pid,'-n','ip','route','flush','cache')
            for chain in ('ALPHA-LINK-OUT','ALPHA-LINK-GUARD'):
                node.exec('iptables','-t','mangle','-Z',chain)

    def errors(node, chain):
        total=0
        for row in node.exec('iptables-save','-c','-t','mangle').splitlines():
            if (' -A '+chain+' ') in row and '--ctstate RELATED' in row and ('--ctproto 6 ' in row or '--ctproto tcp ' in row):
                total+=int(row.split(']')[0][1:].split(':')[0])
        return total

    bridges=[(a,'alb0af00002'),(b,'alb0af00102')]
    original_nics=[node.exec('cat','/sys/class/net/alphaqa/mtu') for node in (a,b)]
    original_bridges=[node.exec('cat','/sys/class/net/'+bridge+'/mtu') for node,bridge in bridges]
    try:
        # Both TCP data directions need host OUTPUT feedback, even though only
        # A -> B initiation is authorized. No ping primes the TCP PMTU cache.
        for node in (a,b):
            node.exec('ip','link','set','alphaqa','mtu','1200')
        transfers('ALPHA-LINK-OUT')
        flush_pmtu()
        ping=a.container('one','sh','-c','ping -M do -s 1300 -c 1 -W 1 "$1" 2>&1 || true','ping',eps[2]['ip'])
        assert 'Frag needed' in ping and 'mtu = 1200' in ping, ping

        # Put the bottleneck after the LAN on each receiver. Its worker emits
        # an error through the LAN and the other worker must FORWARD it back.
        for node,mtu in zip((a,b),original_nics):
            node.exec('ip','link','set','alphaqa','mtu',mtu)
        for (node,bridge),mtu in zip(bridges,('1100','1200')):
            node.exec('ip','link','set',bridge,'mtu',mtu)
        transfers('ALPHA-LINK-GUARD')
    finally:
        for node,mtu in zip((a,b),original_nics):
            node.exec('ip','link','set','alphaqa','mtu',mtu)
        for (node,bridge),mtu in zip(bridges,original_bridges):
            node.exec('ip','link','set',bridge,'mtu',mtu)
        flush_pmtu()

    # Keep two real TCP conntrack entries alive while changing policy. Crafted
    # errors use their exact tuples; the capture runs inside the destination
    # container, beyond both the mangle and Docker access gates.
    path='/tmp/alpha-pmtu-'+uuid.uuid4().hex
    keep="""import json,os,pathlib,socket,sys,time,traceback
pathlib.Path(sys.argv[3]+'.pid').write_text(str(os.getpid()))
try:
 sockets=[];ports={}
 for port in (18000,18001):
  s=socket.create_connection((sys.argv[1],port),timeout=4)
  greeting=b''
  while len(greeting)<len(sys.argv[2]):
   part=s.recv(80)
   if not part:raise AssertionError('missing greeting')
   greeting+=part
  assert greeting.decode()==sys.argv[2],greeting
  sockets.append(s);ports[port]=s.getsockname()[1]
 pathlib.Path(sys.argv[3]).write_text(json.dumps(ports))
 time.sleep(180)
except Exception:
 pathlib.Path(sys.argv[3]+'.error').write_text(traceback.format_exc())
"""
    a.exec('docker','exec','-d','one','python3','-c',keep,eps[2]['ip'],eps[0]['ip'],path)
    def ready():
        status=a.container('one','python3','-c',"import pathlib,sys;p=pathlib.Path(sys.argv[1]);e=pathlib.Path(sys.argv[1]+'.error');print(e.read_text() if e.exists() else 'ready' if p.exists() else 'waiting')",path)
        assert status in ('ready','waiting'), status
        return status=='ready'
    wait(ready,'persistent PMTU TCP flows')
    ports=json.loads(a.container('one','cat',path))

    def related(port, reply=False, local=False, mac='', foreign=False, quoted_port=None):
        target,receiver=(eps[2],b) if reply else (eps[0],a)
        sender=receiver if local else (a if reply else b)
        source=sender.internal
        if local:
            source=target['ip'].rsplit('.',1)[0]+'.1'
        if foreign:
            source=eps[1]['ip']
        quoted_port=port if quoted_port is None else quoted_port
        quoted=(eps[2]['ip'],quoted_port,eps[0]['ip'],ports[str(port)]) if reply else (eps[0]['ip'],ports[str(port)],eps[2]['ip'],quoted_port)
        marker=1+uuid.uuid4().int % 65535
        ready=path+'-'+str(marker)
        capture="""import pathlib,socket,struct,sys,time
s=socket.socket(socket.AF_INET,socket.SOCK_RAW,socket.IPPROTO_ICMP);s.settimeout(1)
pathlib.Path(sys.argv[1]).touch();deadline=time.monotonic()+1
received=False
while time.monotonic()<deadline:
 try:p=s.recv(65535)
 except socket.timeout:break
 n=(p[0]&15)*4
 if struct.unpack('!H',p[4:6])[0]==int(sys.argv[2]) and p[n:n+2]==b'\\x03\\x04':
  received=True;break
print('received' if received else 'blocked')
"""
        send="""import errno,socket,struct,sys,time
src,dst,qs,sp,qd,dp,marker,mac,dmac=sys.argv[1:]
def checksum(data):
 data+=b'\\0'*(len(data)%2);n=sum(struct.unpack('!%dH'%(len(data)//2),data))
 while n>>16:n=(n&65535)+(n>>16)
 return (~n)&65535
def ip(s,d,proto,payload):
 h=struct.pack('!BBHHHBBH4s4s',69,0,20+len(payload),int(marker),0,64,proto,0,socket.inet_aton(s),socket.inet_aton(d))
 return h[:10]+struct.pack('!H',checksum(h))+h[12:]+payload
quote=ip(qs,qd,6,struct.pack('!HHI',int(sp),int(dp),1))
icmp=struct.pack('!BBHHH',3,4,0,0,1400)+quote
icmp=icmp[:2]+struct.pack('!H',checksum(icmp))+icmp[4:]
packet=ip(src,dst,1,icmp)
if mac:
 s=socket.socket(socket.AF_PACKET,socket.SOCK_RAW);s.bind(('alphaqa',0))
 packet=bytes.fromhex(dmac.replace(':',''))+bytes.fromhex(mac.replace(':',''))+b'\\x08\\x00'+packet
else:s=socket.socket(socket.AF_INET,socket.SOCK_RAW,socket.IPPROTO_RAW)
for _ in range(3):
 if mac:s.send(packet)
 else:
  try:s.sendto(packet,(dst,0))
  except PermissionError as error:
   # A local OUTPUT DROP reports EPERM to its raw socket. The receiver
   # capture below still determines whether the packet crossed the gate.
   if error.errno!=errno.EPERM:raise
 time.sleep(.05)
"""
        with ThreadPoolExecutor(max_workers=1) as executor:
            result=executor.submit(receiver.container,'one','python3','-c',capture,ready,str(marker))
            wait(lambda:receiver.container('one','ls',ready),'ICMP capture ready')
            args=('python3','-c',send,source,target['ip'],quoted[0],str(quoted[1]),quoted[2],str(quoted[3]),str(marker),mac,f'02:00:00:00:00:0{receiver.index}')
            if foreign:
                a.container('two',*args)
            else:
                sender.exec(*args)
            return result.result(timeout=5)=='received'

    try:
        for reply in (False,True):
            assert related(18000,reply=reply), 'authorized forwarded error'
            assert related(18000,reply=reply,local=True), 'authorized local error'
        assert not related(18000,mac='02:00:00:00:00:ff'), 'wrong ingress MAC'
        assert not related(18000,foreign=True), 'wrong ingress interface'
        assert not related(18000,quoted_port=18002), 'unrelated TCP tuple'
        print('PMTU errors: authorized tuples pass; wrong MAC, interface and tuple are denied',flush=True)
        both(2,[rule('tcp',18001,18001),rule('icmp')])
        for reply in (False,True):
            assert not related(18000,reply=reply), 'removed port forwarded error'
            assert not related(18000,reply=reply,local=True), 'removed port local error'
            assert related(18001,reply=reply), 'retained TCP port'
        print('PMTU errors: narrowed port range revokes old TCP errors in both paths',flush=True)
        both(3,[rule('tcp',18001,18001,direction='ba'),rule('icmp')])
        for reply in (False,True):
            assert not related(18001,reply=reply), 'removed initiation direction forwarded error'
            assert not related(18001,reply=reply,local=True), 'removed initiation direction local error'
        print('PMTU errors: reversed initiation direction rejects old TCP tuples',flush=True)
        both(4,[rule('icmp')])
        for reply in (False,True):
            assert not related(18001,reply=reply), 'revoked TCP forwarded error'
            assert not related(18001,reply=reply,local=True), 'revoked TCP local error'
        print('PMTU errors: revoked TCP grants cannot use retained conntrack entries',flush=True)
    finally:
        a.container('one','python3','-c','import os,pathlib,signal,sys;os.kill(int(pathlib.Path(sys.argv[1]+".pid").read_text()),signal.SIGTERM)',path)


def check_helper_upgrade(node, peer, eps):
    # Pause event reconciliation so the checks below can observe the invalidated
    # digest before a background retry repairs it. Live rules stay in place.
    node.exec('sh','-c','kill -STOP "$(cat /qa/home/pid)"')
    try:
        for interrupted in (False,True):
            before=json.loads(node.exec('cat','/var/lib/project-alpha-interconnect/ledger.json'))
            node.exec('python3','-c',"""import hashlib,json,pathlib,sys
binary=pathlib.Path(sys.argv[1]);record=pathlib.Path('/var/lib/project-alpha-interconnect/install.json')
installed=binary.read_bytes();previous=installed+b'fixture previous release'
binary.write_bytes(previous);value=json.loads(record.read_text())
old=hashlib.sha256(previous).hexdigest();current=hashlib.sha256(installed).hexdigest()
value['Digest']=current if sys.argv[2]=='True' else old
value['PreviousDigest']=old if sys.argv[2]=='True' else ''
record.write_text(json.dumps(value))""",HELPER,str(interrupted))
            node.exec('/opt/alpha/project-alpha','interconnect','init','--user','nobody','--data-dir','/qa/data')
            invalidated=json.loads(node.exec('cat','/var/lib/project-alpha-interconnect/ledger.json'))
            assert before['FirewallHash']
            assert invalidated==dict(before,FirewallHash=''), 'upgrade changed live resource state'
            node.container('one','ping','-c','1','-W','1',eps[2]['ip'])
            try:
                node.exec(HELPER,text=json.dumps(dict(operation='check',config=before['Config'])))
                raise AssertionError('new helper accepted the previous policy digest')
            except RuntimeError as error:
                assert '防火墙规则已改变' in str(error), error
            # The same desired revision is valid; policy repair needs no user
            # edit and retains the endpoint allocations and existing routes.
            node.exec(HELPER,text=json.dumps(dict(operation='apply',config=before['Config'])))
            repaired=json.loads(node.exec('cat','/var/lib/project-alpha-interconnect/ledger.json'))
            assert repaired['FirewallHash']
            node.exec('/opt/alpha/project-alpha','interconnect','init','--user','nobody','--data-dir','/qa/data')
            assert json.loads(node.exec('cat','/var/lib/project-alpha-interconnect/ledger.json'))==repaired, 'identical helper invalidated healthy policy'
    finally:
        node.exec('sh','-c','kill -CONT "$(cat /qa/home/pid)"')
    assert node.request(dict(operation='apply',config=before['Config'],force=True))['applied']==before['Revision']
    assert node.request(dict(operation='check',config=before['Config']))['applied']==before['Revision']
    node.container('one','ping','-c','1','-W','1',eps[2]['ip'])
    peer.container('one','ping','-c','1','-W','1',eps[0]['ip'])
    print('Helper upgrade: normal/interrupted replacement invalidates policy; same-revision retry recovers',flush=True)


def exercise(a,b,eps,switch):
    def rule(protocol,lo=0,hi=0,direction='ab'):
        return dict(protocol=protocol,first=lo,last=hi,direction=direction,enabled=True)

    def apply(node,revision,rules,empty=False):
        targets=[1,2] if node is a else [2]
        connections=[dict(id=str(i)*32,a=ref(eps[0]),b=ref(eps[i]),enabled=True,rules=rules) for i in targets]
        config=dict(protocol=1,revision=revision,pool='10.240.0.0/16',subnet=node.subnet,internal_ip=node.internal,
                    node=node.id,endpoints=[] if empty else [eps[i] for i in [0,*targets]],connections=[] if empty else connections)
        return node.request(dict(operation='apply',config=config,force=True))

    def both(revision,rules):
        for node in (a,b):
            assert apply(node,revision,rules)['applied']==revision

    def ping(node,name,ip):
        return node.container(name,'sh','-c','ping -c 1 -W 1 "$1" >/dev/null && echo yes || echo no','ping',ip)=='yes'

    tcp=rule('tcp',18000,18001)
    rules=[tcp,rule('icmp'),rule('udp',18001,18002)]
    before=a.container('one','ip','route','show','default')
    both(1,rules)
    # The helper's sudo entry accepts only the installed binary with no arguments.
    rejected=subprocess.run(['docker','exec','--user','nobody',a.name,'sudo','-n','--',HELPER,'guard'],capture_output=True)
    assert rejected.returncode
    try:
        a.request(dict(operation='apply',pid=1))
        raise AssertionError('accepted a caller-supplied PID')
    except RuntimeError as error:
        assert 'pid' in str(error)
    assert a.container('one','ip','route','show','default')==before
    assert ping(a,'one',eps[1]['ip']) and ping(a,'one',eps[2]['ip'])
    assert not ping(b,'one',eps[0]['ip']) and not ping(a,'two',eps[2]['ip'])
    server="""import hashlib,socket,threading,time
def tcp(c,addr):
 with c:
  c.sendall(addr[0].encode());command=b''
  while len(command)<8:
   part=c.recv(8-len(command))
   if not part:return
   command+=part
  if command==b'upload  ':
   h=hashlib.sha256()
   while True:
    part=c.recv(65536)
    if not part:break
    h.update(part)
   c.sendall(h.hexdigest().encode())
  elif command==b'download':c.sendall(bytes(range(256))*8192)
def serve(p,t):
 s=socket.socket(type=t);s.bind(('',p))
 if t==2:
  while True:
   msg,addr=s.recvfrom(128);s.sendto(addr[0].encode(),addr)
 s.listen()
 while True:
  c,addr=s.accept();threading.Thread(target=tcp,args=(c,addr),daemon=True).start()
for p in (18000,18001,18002):
 for t in (1,2):threading.Thread(target=serve,args=(p,t),daemon=True).start()
time.sleep(600)"""
    b.exec('docker','exec','-d','one','python3','-c',server)
    def probe(proto,port):
        code="""import socket,sys
s=socket.socket(type=int(sys.argv[1]));s.settimeout(.5)
try:
 s.connect((sys.argv[2],int(sys.argv[3])))
 if int(sys.argv[1])==2:s.send(b'x')
 print(s.recv(80).decode())
except OSError:print('blocked')"""
        return a.container('one','python3','-c',code,str(proto),eps[2]['ip'],str(port))
    wait(lambda:probe(1,18000)==eps[0]['ip'],'TCP service')
    assert probe(1,18001)==probe(2,18002)==eps[0]['ip']
    assert probe(1,18002)==probe(2,18000)=='blocked'
    check_pmtu(a,b,eps,both,rule)
    revision=5
    # Reject foreign routes without removing them. A retry after manual repair
    # uses the same desired revision that was already persisted.
    a.exec('ip','route','add','blackhole','10.240.253.40/32','proto','static')
    try:
        try:
            apply(a,revision,[rule('all',direction='both')])
            raise AssertionError('foreign route accepted')
        except RuntimeError as error:
            assert '冲突' in str(error)
        assert 'blackhole' in a.exec('ip','route','show','exact','10.240.253.40/32')
        assert a.request()['applied']==revision-1
    finally:
        a.exec('ip','route','del','blackhole','10.240.253.40/32','proto','static')
    both(revision,[rule('all',direction='both')])
    assert ping(b,'one',eps[0]['ip']) and probe(1,18002)==eps[0]['ip']
    check_helper_upgrade(a,b,eps)
    # Overmount this disposable container's sandbox handle with the worker
    # namespace. The helper must reject it before executing namespace operations.
    sandbox=json.loads(a.exec('docker','inspect','one'))[0]['NetworkSettings']['SandboxKey']
    a.exec('mount','--bind','/proc/self/ns/net',sandbox)
    try:
        try:
            apply(a,revision,[rule('all',direction='both')])
            raise AssertionError('accepted the worker network namespace')
        except RuntimeError as error:
            assert 'namespace' in str(error)
    finally:
        a.exec('umount',sandbox)
    apply(a,revision,[rule('all',direction='both')])
    a.exec('docker','restart','--timeout','1','one')
    wait(lambda:ping(a,'one',eps[2]['ip']), 'container restart recovery')
    a.exec('sh','-c','kill -TERM "$(cat /qa/home/pid)"')
    wait(lambda: not a.exec('sh','-c','test -d /proc/$(cat /qa/home/pid) && echo running || true'),'worker stop')
    a.exec('sh','-c','kill -TERM "$(cat /var/run/docker.pid)"')
    wait(lambda: a.exec('sh','-c','test ! -e /var/run/docker.pid && test ! -S /var/run/docker.sock && echo stopped || true'),'daemon stop')
    a.boot()
    assert not ping(a,'one',eps[2]['ip'])
    a.start_service()
    wait(lambda:ping(a,'one',eps[2]['ip']), 'Docker and worker restart recovery')
    # A fresh outer netns loses its firewall and the veth. Recreate the same LAN
    # and run the pre-daemon guard before starting this node's worker.
    run('docker','restart','--timeout','3',a.name)
    attach_lan(a,switch)
    a.boot()
    assert not ping(a,'one',eps[2]['ip'])
    a.start_service()
    wait(lambda:ping(a,'one',eps[2]['ip']),'fresh network namespace recovery')
    # Pause only the new revision's first helper call. Blocking the shared
    # helper lock can instead trap an older Docker connect replay while it
    # holds Handler.mu, preventing the target intent from ever being stored.
    # This fixture wrapper is reached after linkApply commits its transaction,
    # and before the real helper can guard or mutate any network state.
    revision+=1
    real_helper=HELPER+'.fixture-real'
    interpreter=a.exec('python3','-c','import sys;print(sys.executable)')
    a.exec('ln',HELPER,real_helper)
    wrapper=f"""#!{interpreter}
import json,os,pathlib,signal,subprocess,sys
body=sys.stdin.buffer.read() if len(sys.argv)==1 else b''
request=json.loads(body) if body else {{}}
if request.get('operation')=='guard' and request.get('config',{{}}).get('revision')=={revision}:
 pathlib.Path('/qa/guard-paused.pid').write_text(str(os.getpid()))
 os.kill(os.getpid(),signal.SIGSTOP)
sys.exit(subprocess.run([{real_helper!r},*sys.argv[1:]],input=body).returncode)
"""
    a.exec('python3','-c',"import os,pathlib,sys;p=pathlib.Path(sys.argv[1]+'.fixture-wrapper');p.write_text(sys.argv[2]);p.chmod(0o755);os.replace(p,sys.argv[1])",HELPER,wrapper)
    try:
        with ThreadPoolExecutor(max_workers=1) as executor:
            delivery=executor.submit(apply,a,revision,[])
            def paused():
                if delivery.done():
                    try:
                        delivery.result()
                    except Exception as error:
                        raise AssertionError(f'delivery failed before target helper pause: {error}') from error
                    raise AssertionError('delivery acknowledged before target helper pause')
                return a.exec('cat','/qa/guard-paused.pid')
            wait(paused,'target helper paused after durable intention')
            assert a.exec('python3','-c',"""import sqlite3,sys
d=sqlite3.connect('/qa/data/platform.sqlite3')
assert d.execute('select desired from link_local').fetchone()[0]==int(sys.argv[1])
assert d.execute("select count(*) from link_operations where revision=? and result='pending'",(int(sys.argv[1]),)).fetchone()[0]==1
print('durable')""",str(revision))=='durable'
            assert ping(a,'one',eps[2]['ip'])
            a.exec('sh','-c','kill -KILL "$(cat /qa/home/pid)" "$(cat /qa/guard-paused.pid)"')
            try:
                delivery.result(timeout=10)
            except (RuntimeError,OSError,EOFError):
                pass
            else:
                raise AssertionError('interrupted delivery acknowledged')
    finally:
        a.exec('mv',real_helper,HELPER)
    a.start_service()
    wait(lambda:a.request()['applied']==revision and not a.request()['error'],'worker crash recovery')
    assert a.exec('python3','-c',"""import sqlite3,sys
d=sqlite3.connect('/qa/data/platform.sqlite3');print(d.execute("select count(*) from link_operations where revision=? and result='interrupted'",(int(sys.argv[1]),)).fetchone()[0])""",str(revision))=='1'
    both(revision,[])
    assert not ping(a,'one',eps[2]['ip'])
    both(revision+1,[rule('icmp',direction='both')])
    check_stopped_endpoint(a,eps,revision+2)
    b.exec('docker','stop','--timeout','1','one')
    for node in (a,b):
        apply(node,revision+3,[],True)
        attachments=json.loads(node.exec('docker','inspect','one'))[0]['NetworkSettings']['Networks']
        assert list(attachments)==['bridge']
        if node is b:
            node.exec('docker','start','one')
        assert 'alpha-link-' not in node.exec('docker','network','ls')
        assert 'proto 242' not in node.exec('ip','route')
        node.exec('/opt/alpha/project-alpha','interconnect','init','--uninstall')
        assert 'ALPHA-LINK-' not in node.exec('iptables','-S')


def main():
    name='alpha-link-test-'+uuid.uuid4().hex[:10]
    with tempfile.TemporaryDirectory() as directory, contextlib.ExitStack() as stack:
        temp=Path(directory)
        for binary in ('project-alpha','alpha-interconnect-helper'):
            subprocess.run(['go','build','-o',str(temp/binary),'./cmd/'+binary],cwd=ROOT,check=True)
        archive=temp/'image.tar'
        run('docker','save','-o',str(archive),IMAGE)
        run('docker','network','create','--internal',name)
        stack.callback(run,'docker','network','rm',name)
        nodes=[Worker(name+'-'+str(i),i) for i in (1,2)]
        eps=[]
        # PID 1 must reap containerd and runc children across daemon restarts.
        init = """import os,signal,sys,time
signal.signal(signal.SIGTERM,lambda *_:sys.exit(0))
while True:
 try:os.wait()
 except ChildProcessError:time.sleep(.05)"""
        for node in nodes:
            run('docker','run','-d','--privileged','--tmpfs','/run:mode=0755','--network',name,'--cgroupns','private','--name',node.name,'--entrypoint','python3',IMAGE,'-c',init)
            stack.callback(run,'docker','rm','-fv',node.name)
            eps.extend(node.prepare(temp,archive,name))
        switch=name+'-lan'
        run('docker','run','-d','--network','none','--cap-add','NET_ADMIN','--name',switch,'--entrypoint','sleep',IMAGE,'infinity')
        stack.callback(run,'docker','rm','-fv',switch)
        run('docker','exec',switch,'ip','link','add','lan','type','bridge')
        run('docker','exec',switch,'ip','link','set','lan','up')
        for node in nodes:
            attach_lan(node,switch)
        try:
            exercise(*nodes,eps,switch)
        except Exception:
            for node in nodes:
                print(node.exec('sh','-c','tail -n 12 /qa/home/worker.log /tmp/dockerd.log'),flush=True)
            raise
    print('Isolated Docker interconnect network and restart checks passed.')


if __name__=='__main__':
    main()
