"""Member status page: identity, node states, synchronous creation and stale responses."""
import copy
import json
import os
import threading
import time
from urllib.parse import urlsplit, parse_qs
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
member = 'a' * 32
username = 'alice'
token = 'b' * 64
password = 'Member-password-123'
endpoint = '/api/status/' + username
nodes = []
for index, (name, online, state) in enumerate([
    ('已有账号', True, 'ready'), ('可申请节点', True, 'unallocated'),
    ('离线节点', False, 'unallocated'), ('<img src=x onerror=alert(1)>', True, 'failed'),
    ('管理员分配', True, 'unallocated'), ('等待分配', True, 'pending'),
]):
    nodes.append(dict(node_id=str(index + 1) * 32, node_name=name, online=online, state=state,
                      internal_ip='10.0.0.' + str(11 + index), host='compute-one', container_count=5,
                      observed_at='', scanning=False, containers=[], container_id='', name='',
                      port=0, error='默认镜像不可用' if state == 'failed' else ''))
nodes[0].update(container_id='c' * 64, name='alpha-existing', port=2222)
nodes[4]['containers'] = [dict(id='d' * 64, name='manual-alice', state='running', managed=True)]
nodes[1]['candidates'] = [
    dict(id='f' * 64, name='pytorch-workspace', owner='legacy', claimed=False),
    dict(id='9' * 64, name='another-workspace', claimed_by='bob', claimed=True),
]
public_key = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f'
second_key = 'ecdsa-sha2-nistp256 AAAAE2VjZHNhLXNoYTItbmlzdHAyNTYAAAAIbmlzdHAyNTYAAABBBIvR3cOir2XFsX4NiA4QO1JKQ7c87emaiV0rBXS3fiseEt0seHFTvuv2Tl0Zz5jQJS1Ko0oVLFAQZ4BtLtn6hKg='
member_keys = public_key
key_writes = []
writes = []
fail = True
hold = threading.Event()
started = threading.Event()
hold.set()
gpu_hold = threading.Event()
gpu_started = threading.Event()
gpu_hold.set()
gpu_fail = False
gpu_calls = []
disk_calls = []
disk_hold = threading.Event()
disk_started = threading.Event()
disk_hold.set()
disk_fail = False


def disk_view(query):
    if 'container' in query:
        if 'path' in query:
            path = query['path'][0]
            if path == '/mine':
                entries = [dict(name='workspace', path='/mine/workspace', known=True, allocated=2048, expandable=True),
                           dict(name='<img src=x onerror=alert(1)>', path='/mine/file', known=True, allocated=1024, expandable=False),
                           dict(name='尚未统计', path='/mine/unknown', known=False, allocated=0, expandable=True)]
                return dict(node=dict(known=True, allocated=8192, partial=True), entries=entries if query.get('offset') == ['0'] else [],
                            self_and_omitted_allocated=1024, other_entries_allocated=4096, has_more=query.get('offset') == ['0'])
            if path == '/mine/workspace':
                return dict(node=dict(known=True, allocated=2048, partial=False),
                            entries=[dict(name='checkpoints', path='/mine/workspace/checkpoints', known=True, allocated=2048, expandable=True)],
                            self_and_omitted_allocated=0, other_entries_allocated=0, has_more=False)
            if path == '/mine/workspace/checkpoints':
                return dict(node=dict(known=True, allocated=2048, partial=False),
                            entries=[dict(name='model.bin', path=path+'/model.bin', known=True, allocated=2048, expandable=False)],
                            self_and_omitted_allocated=0, other_entries_allocated=0, has_more=False)
            return dict(node=dict(known=False, allocated=0, partial=True), entries=[], self_and_omitted_allocated=0, other_entries_allocated=0, has_more=False)
        return dict(sources=[dict(label='可写层 /', path='/mine', known=True, allocated=8192, expandable=True)], writable_layer=dict(allocated=8192))
    return dict(observed_at='2026-10-09T12:00:00+08:00', updated_at='', exclusive=2048, shared=1024, unrelated=1024,
                filesystems=[dict(mount='/data', fs='ext4', total=8192, used=4096, available=4096)],
                containers=[dict(id='mine', name='alice-workspace', owner='alice', exclusive=1024, shared=1024, known=True, partial=False, expandable=True),
                            dict(id='other', name='bob-workspace', owner='bob', exclusive=1024, shared=1024, known=True, partial=False, expandable=False)])


gpu_devices = [dict(uuid='GPU-1', index=0, name='NVIDIA Test GPU', utilization=42, memory_used_mib=1024,
                    memory_total_mib=24576, temperature=58, compute_mode='Default', mig=False,
                    processes=[dict(pid=1234, name='python train.py', kind='C', memory_mib=1024,
                                    container_id='bob-container', container='bob-workspace', owner='bob', started_at=time.time()-3600)]),
               dict(uuid='GPU-2', index=1, name='<img src=x onerror=alert(1)>', utilization=None,
                    memory_used_mib=None, memory_total_mib=None, temperature=None, compute_mode='Default', mig=False, processes=[])]


def gpu_overview(query):
    now = time.time()
    node_id = query['node_id'][0]
    end = min(float(query.get('end', [now])[0]), now)
    step = float(query.get('step', [60])[0])
    start = end - float(query.get('hours', [6])[0])*3600
    devices = [] if node_id == nodes[1]['node_id'] else copy.deepcopy(gpu_devices)
    stale = node_id == nodes[3]['node_id']
    for device in devices:
        device.update(state='unknown' if stale else 'busy' if device['processes'] else 'idle',
                      process_count=len(device['processes']), owners=sorted(set(p['owner'] for p in device['processes'])))
    return dict(now=now, sample_seconds=15,
                current=dict(at=now-120 if stale else now, stale=stale, devices=devices, error='采集异常' if stale else '', warning=''),
                history={'from': start, 'to': end, 'step': step, 'since': start,
                         'series': [dict(uuid=d['uuid'], name=d['name'], points=[dict(at=start, utilization=42, observed_seconds=step, owners={'bob':step}),dict(at=start+step, utilization=60, observed_seconds=step, owners={'alice':step,'bob':step})]) for d in devices],
                         'users': [dict(owner='bob', seconds=3600), dict(owner='alice', seconds=1800)] if devices else []})


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def send(self, code, value, content_type='application/json'):
        body = json.dumps(value).encode() if content_type == 'application/json' else value
        self.send_response(code)
        self.send_header('Content-Type', content_type)
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        try:
            self.wfile.write(body)
        except (BrokenPipeError, ConnectionResetError):
            pass

    def authorized(self):
        if self.headers.get('Authorization') != 'Bearer ' + token:
            self.send(401, dict(error='用户名或密码错误'))
            return False
        if not self.path.startswith(endpoint):
            self.send(403, dict(error='登录会话与页面使用者不匹配'))
            return False
        return True

    def do_GET(self):
        if urlsplit(self.path).path == endpoint + '/disk':
            if self.authorized():
                query = parse_qs(urlsplit(self.path).query)
                disk_calls.append(query)
                disk_started.set()
                disk_hold.wait(15)
                if query['node_id'][0] == nodes[1]['node_id']:
                    self.send(404, dict(error='暂无磁盘扫描结果'))
                elif query['node_id'][0] == nodes[2]['node_id']:
                    self.send(502, dict(error='节点不可达'))
                elif disk_fail:
                    self.send(503, dict(error='磁盘查询失败'))
                else:
                    self.send(200, disk_view(query))
            return
        if urlsplit(self.path).path == endpoint + '/gpu':
            if self.authorized():
                query = parse_qs(urlsplit(self.path).query)
                gpu_calls.append(query)
                gpu_started.set()
                gpu_hold.wait(15)
                if query['node_id'][0] == nodes[2]['node_id']:
                    self.send(502, dict(error='节点不可达'))
                elif gpu_fail:
                    self.send(503, dict(error='GPU 查询失败'))
                else:
                    self.send(200, gpu_overview(query))
            return
        if self.path.startswith('/api/status/'):
            if self.authorized():
                self.send(200, dict(member_id=member, username='alice', ssh_public_key=member_keys, control=dict(status_url='http://100.64.0.2:9765/status/alice'), access=dict(key_state='ready', invite_state='invited', share_host='100.64.0.2', share_ssh_port=2222, status_port=9765), nodes=copy.deepcopy(nodes), checked_at=1800000000))
            return
        filename = 'status.html' if self.path.startswith('/status/') else self.path.lstrip('/')
        if filename not in ('status.html', 'status.js', 'status.css', 'clipboard.js', 'gpu.js', 'gpu.css', 'usage.js', 'member-disk.js'):
            self.send(404, {})
            return
        mime = {'html': 'text/html', 'js': 'application/javascript', 'css': 'text/css'}[filename.split('.')[-1]]
        self.send(200, (repo / 'dist' / filename).read_bytes(), mime + '; charset=utf-8')

    def do_POST(self):
        global member_keys
        if self.path.endswith('/login'):
            body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
            if self.path != endpoint + '/login':
                self.send(401, dict(error='用户名或密码错误'))
            elif body.get('password') != password:
                self.send(401, dict(error='用户名或密码错误'))
            else:
                self.send(200, dict(session_token=token))
            return
        if self.path.endswith('/logout'):
            self.send(200, dict(ok=True))
            return
        if not self.authorized():
            return
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        if self.path == endpoint + '/keys':
            if body['ssh_public_key'] == 'invalid':
                self.send(400, dict(error='第 1 行：请提供有效公钥'))
                return
            key_writes.append(body)
            member_keys = body['ssh_public_key']
            nodes[0].update(key_state='failed', key_error='容器已停止，请启动后重试')
            self.send(202, dict(ok=True))
            return
        writes.append(body)
        started.set()
        hold.wait(15)
        node = next(n for n in nodes if n['node_id'] == body['node_id'])
        if fail:
            node.update(state='failed', error='默认镜像不可用', mode=body['mode'], target_id=body['container_id'])
            self.send(409, dict(error='默认镜像不可用'))
            return
        node.update(state='ready', error='', mode=body['mode'], target_id=body['container_id'], container_id='e' * 64, name='alpha-new', port=2223)
        self.send(200, dict(member_id=member, nodes=copy.deepcopy(nodes)))


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
thread = threading.Thread(target=server.serve_forever, daemon=True)
thread.start()
url = 'http://127.0.0.1:' + str(server.server_port)
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        page = browser.new_page(viewport=dict(width=1440, height=1080))
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.goto(url + '/status/' + username)
        expect(page.locator('#memberIdentity')).to_have_text('使用者 · alice')
        expect(page.locator('#loginPanel')).to_be_visible()
        expect(page.locator('#statusContent')).to_be_hidden()
        page.locator('#loginPassword').fill('invalid')
        page.locator('#loginSubmit').click()
        expect(page.locator('#statusError')).to_contain_text('用户名或密码错误')
        page.locator('#loginPassword').fill(password)
        page.locator('#loginSubmit').click()
        expect(page.locator('#totalNodes')).to_have_text('6')
        assert page.evaluate('sessionStorage.getItem("alpha.member-session.alice")') == token
        expect(page.locator('#toggleGPU')).to_be_visible()
        assert page.locator('#toggleGPU').bounding_box()['height'] >= 48
        expect(page.locator('#gpuPanel')).to_be_hidden()
        page.locator('#toggleGPU').click()
        expect(page.locator('#gpuPanel')).to_be_visible()
        expect(page.locator('#toggleGPU')).to_have_attribute('aria-expanded', 'true')
        expect(page.locator('#gpuCards')).to_contain_text('42%')
        expect(page.locator('#gpuCards')).to_contain_text('58 °C')
        expect(page.locator('#gpuProcessRows')).to_contain_text('bob')
        expect(page.locator('#gpuProcessRows')).to_contain_text('python train.py')
        expect(page.locator('#gpuProcessRows')).to_contain_text('bob-workspace')
        expect(page.locator('#gpuCharts .gpu-chart')).to_have_count(2)
        expect(page.locator('#gpuCharts')).to_contain_text('bob')
        expect(page.locator('#gpuRanking')).to_contain_text('alice')
        expect(page.locator('#gpuRanking')).to_contain_text('bob')
        # Disk and GPU panels have separate visibility, selection and requests.
        expect(page.locator('#diskPanel')).to_be_hidden()
        assert not disk_calls
        page.locator('#toggleDisk').click()
        expect(page.locator('#diskPanel')).to_be_visible()
        expect(page.locator('#gpuPanel')).to_be_visible()
        expect(page.locator('#diskFilesystems')).to_contain_text('50.0% 已用')
        expect(page.locator('#diskContainers')).to_contain_text('bob-workspace')
        expect(page.locator('#diskContainers details')).to_have_count(1)
        expect(page.locator('#diskContainers details[open]')).to_have_count(0)
        page.locator('#diskContainers summary').click()
        expect(page.locator('.disk-map')).to_be_visible()
        expect(page.locator('.disk-source-picker')).to_contain_text('可写层')
        expect(page.locator('.disk-map-tile')).to_have_count(4)
        # Tile areas retain the whole directory, including other pages and residual bytes.
        areas = page.locator('.disk-map-tile').evaluate_all("tiles => Object.fromEntries(tiles.map(t => [t.dataset.diskTile, parseFloat(t.style.width)*parseFloat(t.style.height)]))")
        assert abs(areas['0']/areas['1'] - 2) < .01
        assert abs(areas['-1']/areas['1'] - 4) < .01
        page.locator('[data-disk-tile="0"]').click()
        expect(page.locator('.disk-detail-path')).to_have_text('/mine/workspace')
        page.locator('[data-disk-tile="0"]').focus()
        page.keyboard.press('Enter')
        expect(page.locator('.disk-detail-path')).to_have_text('/mine/workspace/checkpoints')
        count = len(disk_calls)
        page.locator('[data-disk-tile="0"]').click()
        expect(page.locator('.disk-map-inspector')).to_contain_text('model.bin · 2 KiB')
        assert len(disk_calls) == count
        page.locator('[data-disk-crumb="1"]').click()
        expect(page.locator('.disk-detail-path')).to_have_text('/mine')
        page.locator('[data-disk-tile="-1"]').click()
        expect(page.locator('[data-disk-prev]')).to_be_visible()
        assert disk_calls[-1]['offset'] == ['50']
        page.locator('[data-disk-prev]').click()
        expect(page.locator('[data-disk-tile="0"]')).to_be_visible()
        page.locator('[data-disk-tile="-2"]').click()
        expect(page.locator('.disk-map-inspector')).to_contain_text('目录自身及未展开空间')
        page.locator('[data-disk-source="2"]').click()
        expect(page.locator('.disk-map-empty')).to_be_visible()
        page.locator('[data-disk-back]').click()
        expect(page.locator('.disk-source-list')).to_contain_text('<img src=x onerror=alert(1)>')
        assert page.locator('#diskPanel img').count() == 0
        assert all(q.get('container', ['mine']) == ['mine'] for q in disk_calls)
        page.locator('#diskPanel').screenshot(path='/tmp/alpha-status-disk-desktop.png')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('#diskPanel').screenshot(path='/tmp/alpha-status-disk-mobile.png')
        page.set_viewport_size(dict(width=1440, height=1080))
        disk_fail = True
        page.locator('#diskRefresh').click()
        expect(page.locator('#diskStatus')).to_contain_text('磁盘查询失败')
        expect(page.locator('#diskContainers')).to_have_text('')
        disk_fail = False
        page.locator('#diskRefresh').click()
        expect(page.locator('#diskContainers')).to_contain_text('alice-workspace')
        page.locator('#diskNode').select_option(nodes[1]['node_id'])
        expect(page.locator('#diskStatus')).to_contain_text('暂无磁盘扫描结果')
        expect(page.locator('#gpuNode')).to_have_value(nodes[0]['node_id'])
        page.locator('#diskNode').select_option(nodes[2]['node_id'])
        expect(page.locator('#diskStatus')).to_contain_text('节点不可达')
        page.locator('#diskNode').select_option(nodes[0]['node_id'])
        expect(page.locator('#diskContainers')).to_contain_text('alice-workspace')
        page.locator('#toggleDisk').click()
        expect(page.locator('#diskPanel')).to_be_hidden()
        expect(page.locator('#gpuPanel')).to_be_visible()
        count = len(disk_calls)
        page.evaluate("document.getElementById('diskRefresh').click()")
        page.wait_for_timeout(100)
        assert len(disk_calls) == count
        assert page.locator('#gpuCards img').count() == 0
        assert page.locator('#statusContent a[href^="/nodes/"], #statusContent a[href="/"]').count() == 0
        with page.expect_response(lambda r: '/gpu?' in r.url and 'hours=72' in r.url):
            page.locator('[data-gpu-hours="72"]').click()
        expect(page.locator('#gpuPan')).to_be_disabled()
        with page.expect_response(lambda r: '/gpu?' in r.url and 'hours=36' in r.url):
            page.locator('#gpuZoomIn').click()
        with page.expect_response(lambda r: '/gpu?' in r.url and 'end=' in r.url):
            page.locator('#gpuPan').fill('18')
        with page.expect_response(lambda r: '/gpu?' in r.url and 'step=300' in r.url):
            page.locator('#gpuStep').select_option('300')
        assert gpu_calls[-1]['node_id'] == [nodes[0]['node_id']]
        page.locator('.gpu-entry').screenshot(path='/tmp/alpha-status-gpu-button.png')
        page.locator('#gpuPanel').screenshot(path='/tmp/alpha-status-gpu-desktop.png')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('#gpuPanel').screenshot(path='/tmp/alpha-status-gpu-mobile.png')
        page.set_viewport_size(dict(width=1440, height=1080))
        gpu_fail = True
        page.locator('#gpuRefresh').click()
        expect(page.locator('#gpuStatus')).to_contain_text('GPU 查询失败')
        expect(page.locator('#gpuCards')).to_contain_text('42%')
        gpu_fail = False
        page.locator('#gpuNode').select_option(nodes[1]['node_id'])
        expect(page.locator('#gpuEmpty')).to_be_visible()
        expect(page.locator('#gpuProcessRows')).to_have_text('')
        expect(page.locator('#gpuRanking')).not_to_contain_text('bob')
        page.locator('#gpuNode').select_option(nodes[2]['node_id'])
        expect(page.locator('#gpuStatus')).to_contain_text('节点不可达')
        expect(page.locator('#gpuCards')).to_have_text('')
        page.locator('#gpuNode').select_option(nodes[3]['node_id'])
        expect(page.locator('#gpuStatus')).to_contain_text('采集异常')
        expect(page.locator('.gpu-state').first).to_have_text('状态未知')
        page.locator('#gpuNode').select_option(nodes[0]['node_id'])
        expect(page.locator('#gpuProcessRows')).to_contain_text('bob')
        page.locator('#toggleGPU').click()
        expect(page.locator('#gpuPanel')).to_be_hidden()
        count = len(gpu_calls)
        page.evaluate("document.getElementById('gpuRefresh').click()")
        page.wait_for_timeout(100)
        assert len(gpu_calls) == count
        expect(page.locator('#onlineNodes')).to_have_text('5')
        expect(page.locator('#allocatedNodes')).to_have_text('2')
        expect(page.locator('#statusNodes')).to_contain_text('alpha-existing')
        expect(page.locator('#statusNodes')).to_contain_text('manual-alice')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 100.64.0.2')
        expect(page.locator('#sshConfig')).to_contain_text('User alpha-jump')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 10.0.0.11')
        expect(page.locator('#sshConfig')).to_contain_text('Port 2222')
        expect(page.locator('#sshConfig')).to_contain_text('ProxyJump alpha-jump')
        page.context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=url)
        page.evaluate("() => { navigator.clipboard.writeText = async () => { throw new DOMException('Denied', 'NotAllowedError'); }; }")
        page.locator('#copySSHConfig').click()
        expect(page.locator('#sshCopyStatus')).to_contain_text('SSH 配置已复制')
        assert page.evaluate('navigator.clipboard.readText()') == page.locator('#sshConfig').inner_text()
        expect(page.locator('#controlStatusAddress')).to_contain_text('/status/alice')
        assert 'node.test' not in page.locator('#statusNodes').inner_text()
        assert page.locator('#sshCommands img').count() == 0
        assert page.locator('#statusNodes img').count() == 0
        assert page.locator('[data-node="' + nodes[4]['node_id'] + '"] [data-apply]').count() == 1
        expect(page.locator('[data-apply="' + nodes[2]['node_id'] + '"]')).to_be_disabled()
        assert page.locator('[data-node="' + nodes[5]['node_id'] + '"] [data-apply]').count() == 0
        assert token not in page.url
        assert page.locator('#loginPassword').input_value() == ''
        page.reload()
        expect(page.locator('#statusContent')).to_be_visible()
        expect(page.locator('#memberKeys')).to_have_value(public_key)
        page.locator('#memberKeys').fill('invalid')
        page.locator('#keysSubmit').click()
        expect(page.locator('#keysError')).to_contain_text('第 1 行')
        expect(page.locator('#memberKeys')).to_have_value('invalid')
        keys = public_key + '\n' + second_key
        page.locator('#memberKeys').fill(keys)
        page.locator('#refreshStatus').click()
        expect(page.locator('#refreshStatus')).to_be_enabled()
        expect(page.locator('#memberKeys')).to_have_value(keys)
        page.locator('#keysSubmit').click()
        expect(page.locator('#keysMessage')).to_contain_text('公钥已保存')
        expect(page.locator('#keysSyncState')).to_contain_text('下发失败 · 容器已停止')
        expect(page.locator('#memberKeys')).to_have_value(keys)
        assert key_writes == [dict(ssh_public_key=keys)]
        target = page.locator('[data-apply="' + nodes[1]['node_id'] + '"]')
        expect(page.locator('[data-choice][value="' + '9' * 64 + '"]')).to_be_disabled()
        expect(page.locator('[data-choice="' + nodes[2]['node_id'] + '"]')).to_be_disabled()
        target.click()
        expect(page.locator('#statusError')).to_contain_text('请先选择')
        assert not writes
        choice = page.locator('[data-choice="' + nodes[1]['node_id'] + '"][value="' + 'f' * 64 + '"]')
        choice.check()
        page.locator('#refreshStatus').click()
        expect(page.locator('#refreshStatus')).to_be_enabled()
        expect(choice).to_be_checked()
        expect(page.locator('[data-choice][value="' + '9' * 64 + '"]')).to_be_disabled()
        page.locator('#statusNodes').screenshot(path='/tmp/alpha-status-choices-desktop.png')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('[data-node="' + nodes[1]['node_id'] + '"]').screenshot(path='/tmp/alpha-status-choices-mobile.png')
        page.set_viewport_size(dict(width=1440, height=1080))
        target.click()
        expect(page.locator('#statusError')).to_contain_text('默认镜像不可用')
        expect(target).to_be_enabled()
        fail = False
        hold.clear()
        started.clear()
        target.click()
        assert started.wait(5)
        expect(target).to_be_disabled()
        expect(page.locator('#statusMessage')).to_contain_text('正在为 可申请节点 分配容器')
        assert len(writes) == 2
        assert all(w == dict(node_id=nodes[1]['node_id'], mode='adopt', container_id='f' * 64) for w in writes)
        hold.set()
        expect(page.locator('#statusMessage')).to_contain_text('分配成功 · alpha-new')
        expect(page.locator('#allocatedNodes')).to_have_text('3')
        expect(page.locator('#sshConfig')).to_contain_text('HostName 10.0.0.12')
        expect(page.locator('#sshConfig')).to_contain_text('Port 2223')
        assert target.count() == 0
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.screenshot(path='/tmp/alpha-status-mobile.png', full_page=True)
        page.set_viewport_size(dict(width=1440, height=1080))
        page.screenshot(path='/tmp/alpha-status-desktop.png', full_page=True)
        # Leaving while GPU and creation responses are outstanding cannot restore private data.
        gpu_hold.clear()
        gpu_started.clear()
        disk_hold.clear()
        disk_started.clear()
        page.locator('#toggleDisk').click()
        assert disk_started.wait(5)
        page.locator('#toggleGPU').click()
        assert gpu_started.wait(5)
        hold.clear()
        started.clear()
        page.locator('[data-choice="' + nodes[3]['node_id'] + '"][value=create]').check()
        page.locator('[data-apply="' + nodes[3]['node_id'] + '"]').click()
        assert started.wait(5)
        page.locator('#logout').click()
        hold.set()
        gpu_hold.set()
        disk_hold.set()
        expect(page.locator('#statusContent')).to_be_hidden()
        assert page.locator('#statusNodes').inner_text() == ''
        assert page.locator('#gpuCards').inner_text() == ''
        assert page.locator('#gpuProcessRows').inner_text() == ''
        assert 'bob' not in page.locator('#gpuCharts').inner_text()
        assert 'bob' not in page.locator('#gpuRanking').inner_text()
        expect(page.locator('#gpuPanel')).to_be_hidden()
        expect(page.locator('#diskPanel')).to_be_hidden()
        expect(page.locator('#diskContainers')).to_have_text('')
        expect(page.locator('#diskFilesystems')).to_have_text('')
        assert page.locator('#sshConfig').inner_text() == ''
        assert page.locator('#memberKeys').input_value() == ''
        assert page.evaluate('sessionStorage.length') == 0
        page.wait_for_timeout(200)
        expect(page.locator('#statusContent')).to_be_hidden()
        page.goto(url + '/status/bob')
        page.locator('#loginPassword').fill(password)
        page.locator('#loginSubmit').click()
        expect(page.locator('#statusError')).to_contain_text('用户名或密码错误')
        expect(page.locator('#statusContent')).to_be_hidden()
        assert not errors, errors
        browser.close()
    print('Member status browser checks passed.')
finally:
    hold.set()
    gpu_hold.set()
    disk_hold.set()
    server.shutdown()
    server.server_close()
