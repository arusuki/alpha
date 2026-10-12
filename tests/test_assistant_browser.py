"""Control assistant navigation, history, memory, safe rendering and errors."""
import json
from browser_support import BrowserHandler, launch_options, start_server
from playwright.sync_api import sync_playwright, expect

session_id = 'b' * 32
sessions, memories, messages, writes = [], [], [], []
failure = False
health_status = "ready"
settings=dict(revision=1,value=dict(instructions="先按节点巡检",builtin_tools=["list_nodes"],mcp_servers=[]),has_tokens={},available_tools=["list_nodes","get_node_gpu"])
settings_writes=[]


class Handler(BrowserHandler):
    def do_GET(self):
        if self.path == '/api/session':
            return self.respond(dict(user=dict(id='a'*32, username='admin', role='admin'), csrf='test', setup_required=False))
        if self.path == '/api/cluster/overview':
            return self.respond(dict(nodes=[], members=[], online=0, container_count=0, checked_at=1, partial=False))
        if self.path == '/api/assistant/settings':
            return self.respond(settings)
        if self.path == '/api/assistant/health':
            return self.respond(dict(status=health_status, detail='Agno 容器服务不可用' if health_status != 'ready' else 'Agno 服务就绪', agno_version='3.1.2', active_runs=0, capacity=4, checked_at=1))
        if self.path == '/api/assistant/sessions':
            return self.respond(dict(sessions=sessions))
        if self.path == '/api/assistant/memories':
            return self.respond(dict(memories=memories))
        if self.path == '/api/assistant/sessions/' + session_id:
            return self.respond(dict(id=session_id, messages=messages))
        return self.serve_asset()

    def do_POST(self):
        assert self.headers['X-CSRF-Token'] == 'test'
        body = json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        assert list(body) == ['message']
        writes.append((self.path, body))
        if failure:
            return self.respond(dict(error='Agno 容器服务不可用，请检查部署'), 503)
        messages.extend([dict(role='user', content=body['message']),
                         dict(role='assistant', content='未注册节点。<img src=x onerror="window.injected=true">')])
        sessions[:] = [dict(id=session_id, title='巡检')]
        memories[:] = [dict(memory='先看 GPU', user_id='a'*32)]
        return self.respond(dict(id=session_id, messages=messages, tools=[]))

    def do_PUT(self):
        assert self.path == '/api/assistant/settings'
        assert self.headers['X-CSRF-Token'] == 'test'
        body=json.loads(self.rfile.read(int(self.headers['Content-Length'])))
        settings_writes.append(body)
        if body['revision'] != settings['revision']:
            return self.respond(dict(error='助理配置已被修改，请重新载入'),409)
        settings.update(revision=settings['revision']+1,value=body['value'],has_tokens={row['id']:bool(body['tokens'].get(row['id'])) for row in body['value']['mcp_servers']})
        self.respond(settings)

    def do_DELETE(self):
        assert self.path == '/api/assistant/memories'
        assert self.headers['X-CSRF-Token'] == 'test'
        memories.clear()
        self.respond(dict(ok=True))


server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1000), reduced_motion='reduce')
        errors = []
        page.on('pageerror', lambda e: errors.append(str(e)))
        page.goto(f'http://127.0.0.1:{server.server_port}/#assistant')
        expect(page.locator('#page-assistant')).to_be_visible()
        expect(page.locator('.platform-nav [data-page="assistant"]')).to_be_visible()
        expect(page.locator('#assistantHealth')).to_contain_text('Agno 服务就绪')
        health_status = 'unavailable'
        page.locator('#assistantHealthRefresh').click()
        expect(page.locator('#assistantHealth')).to_contain_text('Agno 容器服务不可用')
        health_status = 'ready'
        page.locator('#assistantHealthRefresh').click()
        expect(page.locator('#assistantHealth')).to_contain_text('Agno 服务就绪')
        page.locator('#assistantSettingsPanel summary').click()
        expect(page.locator('#assistantInstructions')).to_have_value('先按节点巡检')
        page.locator('#assistantInstructions').fill('优先检查 GPU，标注数据时间')
        page.locator('#assistantBuiltinTools input[value="get_node_gpu"]').check()
        page.locator('#assistantMCPAdd').click()
        row=page.locator('.assistant-mcp')
        row.locator('[data-field="id"]').fill('metrics')
        row.locator('[data-field="url"]').fill('http://127.0.0.1:8000/mcp')
        row.locator('[data-field="tools"]').fill('get_metrics, get_alerts')
        row.locator('[data-field="token"]').fill('browser-fixture-secret')
        row.locator('[data-field="enabled"]').check()
        page.locator('#assistantSettingsSave').click()
        expect(page.locator('#assistantSettingsStatus')).to_have_text('配置已保存，从下一轮对话生效。')
        expect(row.locator('[data-field="token"]')).to_have_value('')
        assert settings_writes[-1]['value']['mcp_servers'][0]['tools']==['get_metrics','get_alerts']
        assert settings_writes[-1]['tokens']=={'metrics':'browser-fixture-secret'}
        assert 'browser-fixture-secret' not in page.locator('#assistantSettingsPanel').inner_text()
        settings['revision']+=1
        page.locator('#assistantInstructions').fill('未保存的编辑')
        page.locator('#assistantSettingsSave').click()
        expect(page.locator('#assistantSettingsStatus')).to_contain_text('请重新载入')
        expect(page.locator('#assistantInstructions')).to_have_value('未保存的编辑')
        page.locator('#assistantSettingsReload').click()
        expect(page.locator('#assistantInstructions')).to_have_value('优先检查 GPU，标注数据时间')
        page.set_viewport_size(dict(width=390,height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'settings mobile overflow'
        page.set_viewport_size(dict(width=1440,height=1000))
        page.locator('#assistantSettingsPanel summary').click()
        expect(page.locator('#assistantMemories')).to_contain_text('暂无长期记忆')
        page.locator('#assistantPrompt').fill('检查节点，记住先看 GPU')
        page.locator('#assistantSend').click()
        expect(page.locator('#assistantStatus')).to_have_text('本轮已完成并保存。')
        expect(page.locator('#assistantMemories')).to_contain_text('先看 GPU')
        expect(page.locator('#assistantMessages .assistant-message')).to_have_count(2)
        assert not page.evaluate('window.injected || false')
        assert page.locator('#assistantMessages img').count() == 0
        page.locator('#assistantNew').click()
        expect(page.locator('#assistantMessages .assistant-message')).to_have_count(0)
        page.locator('#assistantSessions').select_option(session_id)
        expect(page.locator('#assistantMessages .assistant-message')).to_have_count(2)
        page.locator('#assistantPrompt').fill('继续')
        page.locator('#assistantSend').click()
        expect(page.locator('#assistantMessages .assistant-message')).to_have_count(4)
        assert writes[-1][0].endswith('/'+session_id+'/messages')
        page.once('dialog', lambda dialog: dialog.accept())
        page.locator('#assistantMemoryClear').click()
        expect(page.locator('#assistantMemories')).to_contain_text('暂无长期记忆')
        expect(page.locator('#assistantMessages .assistant-message')).to_have_count(4)
        failure = True
        page.locator('#assistantPrompt').fill('未安装时的错误')
        page.locator('#assistantSend').click()
        expect(page.locator('#assistantStatus')).to_contain_text('Agno 容器服务不可用')
        expect(page.locator('#assistantPrompt')).to_have_value('未安装时的错误')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= window.innerWidth'), 'mobile overflow'
        assert not errors, errors
        browser.close()
    print('Assistant browser checks passed.')
finally:
    server.shutdown()
    server.server_close()
