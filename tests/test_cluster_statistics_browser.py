"""Control statistics distinguish registered members, unassigned containers and unknown nodes."""
import copy
from browser_support import BrowserHandler, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

node_id = 'e' * 32
containers = [dict(id=f'{i:064x}', name=f'container-{i:02d}', owner='alice' if i == 0 else '',
                   managed=True, state='running', observed_at='2026-10-07T08:00:00Z') for i in range(19)]
node = dict(id=node_id, name='GPU 01', kind='worker', url='http://10.0.0.11:8765',
            internal_ip='10.0.0.11', online=True, compatible=True,
            inventory=dict(host='gpu-01', containers=containers, active=None, observed_at='2026-10-07T08:00:00Z'))
data = dict(nodes=[node], members=[
    dict(id='a'*32, username='alice', registered=True, count=1,
         nodes=[dict(id=node_id, name='GPU 01', containers=containers[:1])]),
    dict(username='', registered=False, count=18,
         nodes=[dict(id=node_id, name='GPU 01', containers=containers[1:])])],
    online=1, container_count=19, partial=False, checked_at=1791360000)
role = 'admin'
failure = False


class Handler(BrowserHandler):
    def do_GET(self):
        if self.path == '/api/session':
            return self.respond(dict(user=dict(id='b'*32, username=role, role=role), csrf='test', setup_required=False))
        if self.path == '/api/cluster/overview':
            if failure:
                return self.respond(dict(error='统计刷新失败'), 503)
            return self.respond(data)
        self.serve_asset()


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1080), reduced_motion='reduce')
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        url = f'http://127.0.0.1:{server.server_port}/'
        page.goto(url)
        expect(page.locator('#clusterContainers')).to_have_text('19')
        expect(page.locator('#clusterOwners')).to_have_text('1')
        expect(page.locator('#clusterUnassigned')).to_have_text('18')
        expect(page.locator('.node-metrics strong').nth(1)).to_have_text('1')
        expect(page.locator('.node-details')).to_contain_text('没有使用者的容器18')
        page.locator('.platform-nav [data-page="allocations"]').click()
        expect(page.locator('#allocationContainerCount')).to_have_text('19')
        expect(page.locator('#allocationOwnerCount')).to_have_text('1')
        expect(page.locator('#allocationUnassignedCount')).to_have_text('18')
        expect(page.locator('#allocationNodeCount')).to_have_text('1')
        expect(page.locator('#allocationResultCount')).to_have_text('1 名使用者')
        expect(page.locator('#allocationRows > details')).to_have_count(2)
        expect(page.locator('#allocationRows')).not_to_contain_text('未登记使用者')
        expect(page.locator('#allocationStatus')).to_contain_text('1 个有使用者的容器 · 18 个没有使用者的容器')
        unassigned = page.locator('details[data-owner=""]')
        expect(unassigned.locator('summary')).to_contain_text('18 个容器 · 1 个节点')
        unassigned.locator('summary').click()
        expect(unassigned.locator('tbody tr')).to_have_count(18)
        expect(unassigned).not_to_contain_text('Invalid Date')
        expect(unassigned.locator('tbody tr').first).to_contain_text('2026')
        page.locator('#allocationsRefresh').click()
        expect(unassigned).to_have_attribute('open', '')
        # A container-name match must be visible without expanding a hidden outer group.
        page.locator('#allocationSearch').fill('container-18')
        expect(page.locator('#allocationRows > details')).to_have_count(1)
        expect(unassigned.locator('td strong').last).to_be_visible()
        expect(page.locator('#allocationResultCount')).to_have_text('0 / 1 名使用者')
        page.locator('#allocationSearch').fill('')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'allocation mobile overflow'
        page.screenshot(path='/tmp/project-alpha-statistics-mobile.png', full_page=True)
        page.set_viewport_size(dict(width=1440, height=1080))
        page.screenshot(path='/tmp/project-alpha-statistics.png', full_page=True)
        failure = True
        page.locator('#allocationsRefresh').click()
        expect(page.locator('#allocationStatus')).to_contain_text('上次结果')
        expect(page.locator('#allocationOwnerCount')).to_have_text('1')
        page.locator('#allocationSearch').fill('alice')
        expect(page.locator('#allocationStatus')).to_contain_text('上次结果')
        page.locator('#allocationSearch').fill('')
        failure = False
        page.locator('#allocationsRefresh').click()
        expect(page.locator('#allocationStatus')).to_contain_text('计算节点统计完整')
        # Both roles see the same statistics; only admins get management actions.
        role = 'viewer'
        page.reload()
        expect(page.locator('#allocationOwnerCount')).to_have_text('1')
        expect(page.locator('#allocationUnassignedCount')).to_have_text('18')
        expect(page.locator('[data-delete-member]')).to_have_count(0)
        role = 'admin'
        page.reload()
        # A connected worker with no readable inventory is unavailable, with unknown totals.
        bad = copy.deepcopy(node)
        bad.update(id='f'*32, name='GPU 02', error='容器详情读取失败')
        del bad['inventory']
        data['nodes'].append(bad)
        data.update(online=2, partial=True)
        data['members'].append(dict(id='c'*32, username='unused', registered=True, count=0, nodes=[]))
        page.locator('#allocationsRefresh').click()
        expect(page.locator('#allocationStatus')).to_contain_text('统计不完整')
        expect(page.locator('#allocationNodeCount')).to_have_text('1')
        expect(page.locator('#allocationOwnerCount')).to_have_text('1')
        expect(page.locator('details[data-owner="unused"]')).to_contain_text('其他节点尚未确认')
        page.locator('.platform-nav [data-page="cluster"]').click()
        expect(page.locator('#clusterOnline')).to_have_text('2 / 2')
        expect(page.locator('#clusterHealth')).to_have_text('1 个节点暂时不可用')
        page.locator('[data-node-filter="offline"]').click()
        expect(page.locator('.node-card')).to_have_count(1)
        expect(page.locator('.node-status')).to_have_text('详情不可用')
        expect(page.locator('.node-metrics strong').nth(1)).to_have_text('—')
        expect(page.locator('[data-reconnect-node]')).to_be_visible()
        page.locator('[data-node-filter="online"]').click()
        expect(page.locator('.node-card')).to_have_count(1)
        expect(page.locator('.node-card h3')).to_have_text('GPU 01')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), 'cluster mobile overflow'
        # An offline registry affects connection health but not worker statistics.
        data['nodes'][1] = dict(id='f'*32, name='Registry', kind='registry', online=False,
                                url='https://register.example', connection=dict(state='disconnected'))
        data.update(online=1, partial=False)
        page.locator('#clusterRefresh').click()
        expect(page.locator('#clusterPartial')).to_be_hidden()
        expect(page.locator('#clusterHealth')).to_have_text('1 个节点暂时不可用')
        assert not errors, errors
        browser.close()
    print('Cluster statistics browser checks passed: 19 containers / 1 member / 18 unassigned, both roles, search, dates, failed details, stale results and mobile layouts.')
finally:
    server.shutdown()
    server.server_close()
