"""Registration choices, disabled claimed containers, refresh and submitted IDs."""
import json
import os
from pathlib import Path
from playwright.sync_api import sync_playwright, expect

repo = Path(__file__).resolve().parents[1]
node_a, node_b = 'a' * 32, 'b' * 32
free, taken = 'c' * 64, 'd' * 64
submitted = []
nodes = [dict(node_id=node_a, node_name='计算节点 A', containers=[
    dict(id=taken, name='另一位用户的容器', owner='bob', claimed=True),
    dict(id=free, name='已有训练环境', owner='legacy', claimed=False)]),
    dict(node_id=node_b, node_name='<img src=x onerror=alert(1)>', containers=[])]

with sync_playwright() as p:
    launch = dict(headless=True, args=['--no-sandbox'])
    if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
        launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
    browser = p.chromium.launch(**launch)
    page = browser.new_page(viewport=dict(width=1000, height=1000))
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))

    def route(r):
        path = r.request.url[len('http://alpha.test'):]
        status, mime = 200, 'application/json'
        if path == '/':
            mime = 'text/html'
            body = (repo / 'internal/registry/page.html').read_text().replace('{{.Base}}', '')
        elif path in ('/app.js', '/style.css'):
            mime = 'application/javascript' if path.endswith('.js') else 'text/css'
            body = (repo / 'internal/registry' / path[1:]).read_text()
        elif path == '/api/session':
            body = json.dumps(dict(csrf='token', schema=dict(revision=1, fields=[]), submitted=False, registered=False))
        elif path == '/api/options':
            body = json.dumps(dict(nodes=nodes))
        elif path == '/api/register':
            submitted.append(r.request.post_data_json)
            status, body = 409, json.dumps(dict(error='测试：保留表单供校验'))
        else:
            status, body = 404, '{}'
        r.fulfill(status=status, content_type=mime, body=body)

    page.route('http://alpha.test/**', route)
    page.goto('http://alpha.test/')
    expect(page.locator('#nodeChoices fieldset')).to_have_count(2)
    page.locator('[name=username]').fill('alice')
    page.locator('[name=password]').fill('Member-password-123')
    page.locator('[name=password_confirm]').fill('Member-password-123')
    page.locator('[name=ssh_public_key]').fill('ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIAABAgMEBQYHCAkKCwwNDg8QERITFBUWFxgZGhscHR4f')
    expect(page.locator('#submit')).to_be_enabled()
    claimed = page.locator('.container-option').filter(has=page.locator(f'input[value="{taken}"]'))
    expect(claimed.locator('input')).to_be_disabled()
    expect(claimed).to_contain_text('已领养')
    expect(claimed).to_contain_text('bob')
    expect(page.locator('.candidate-list').first.locator('input').first).to_have_value(free)
    expect(page.locator('.candidate-list').first.locator('input').last).to_have_value(taken)
    assert page.locator('#nodeChoices img').count() == 0
    page.locator('#submit').click()
    assert not submitted, 'submitted without per-node choices'
    page.locator(f'input[data-node="{node_a}"][value="{free}"]').check()
    page.locator(f'input[data-node="{node_b}"][value=create]').check()
    expect(page.locator('#choiceSummary')).to_have_text('已选择 2 / 2 个节点')
    page.locator('#nodeChoices').screenshot(path='/tmp/alpha-registration-choices-desktop.png')
    page.locator('#submit').click()
    expect(page.locator('#message')).to_contain_text('保留表单')
    assert submitted[-1]['containers'] == [
        dict(node_id=node_a, mode='adopt', container_id=free),
        dict(node_id=node_b, mode='create', container_id='')]
    # Another user took the selected container: refresh keeps it visible but
    # disables it and clears the now-invalid selection.
    nodes[0]['containers'][1]['claimed'] = True
    page.locator('#reloadNodes').click()
    expect(page.locator(f'input[value="{free}"]')).to_be_disabled()
    expect(page.locator(f'input[data-node="{node_a}"]:checked')).to_have_count(0)
    expect(page.locator(f'input[data-node="{node_b}"]:checked')).to_have_value('create')
    expect(page.locator('#choiceSummary')).to_have_text('已选择 1 / 2 个节点')
    page.set_viewport_size(dict(width=390, height=844))
    assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
    page.screenshot(path='/tmp/alpha-registration-choices.png', full_page=True)
    assert not errors, errors
    browser.close()
print('Registration container choice browser checks passed.')
