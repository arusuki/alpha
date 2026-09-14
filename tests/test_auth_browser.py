"""Exercise the opening sequence and auth states in a real browser."""
import json
import mimetypes
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
artifacts = Path(os.environ.get('PROJECT_ALPHA_AUTH_ARTIFACTS', '/tmp/project-alpha-auth'))
artifacts.mkdir(parents=True, exist_ok=True)


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def do_GET(self):
        file = repo / 'dist' / ('index.html' if self.path == '/' else self.path.lstrip('/'))
        if not file.is_file():
            self.send_error(404)
            return
        body = file.read_bytes()
        self.send_response(200)
        self.send_header('Content-Type', mimetypes.guess_type(str(file))[0] or 'text/plain')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)


def mock_api(page, setup=False, authenticated=False):
    calls = []

    def respond(route):
        path = route.request.url.split('/api/')[1]
        calls.append(path)
        value, status = {}, 200
        if path == 'session':
            value = dict(user=dict(id='test', username='admin', role='admin') if authenticated else None,
                         csrf='test', setup_required=setup)
        elif path == 'state':
            value = dict(jobs=[], directory_jobs=[], latest_id=None, active=None, interval_minutes=0)
        elif path in ('login', 'setup'):
            value, status = dict(error='账号或密码不正确'), 401
        route.fulfill(status=status, content_type='application/json', body=json.dumps(value))

    page.route('**/api/**', respond)
    return calls


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        url = f'http://127.0.0.1:{server.server_port}'
        page = browser.new_page(viewport=dict(width=1440, height=1000))
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        calls = mock_api(page)
        page.clock.install()
        page.goto(url)
        page.wait_for_selector('#authSkip', state='visible')
        page.clock.run_for(650)
        early = page.locator('#authAlphaInk').get_attribute('d')
        assert early, 'the pen must have begun drawing'
        assert page.locator('#authCard').evaluate('(e) => e.inert')
        page.locator('#authSkip').focus()
        page.keyboard.press('Tab')
        assert page.evaluate('document.activeElement.id') not in ('authUsername', 'authPassword', 'authSubmit')
        page.screenshot(path=str(artifacts / '01-first-stroke.png'))
        page.clock.run_for(1050)
        middle = page.locator('#authAlphaInk').get_attribute('d')
        assert len(middle) > len(early) * 2, 'the outline must grow as it is drawn'
        page.screenshot(path=str(artifacts / '02-drawing.png'))
        emblem = page.locator('.auth-emblem').bounding_box()
        assert abs(emblem['x'] + emblem['width'] / 2 - 720) < 2
        assert page.locator('#authCard').is_hidden()
        page.clock.run_for(1000)
        page.screenshot(path=str(artifacts / '03-complete-alpha.png'))
        page.clock.run_for(1800)
        assert page.locator('#authCard').is_visible()
        assert not page.locator('#authCard').evaluate('(e) => e.inert')
        emblem = page.locator('.auth-emblem').bounding_box()
        card = page.locator('#authCard').bounding_box()
        assert emblem['x'] + emblem['width'] / 2 < 720 < card['x']
        page.screenshot(path=str(artifacts / '04-login-desktop.png'), animations='disabled')
        page.locator('#authUsername').fill('admin')
        page.locator('#authPassword').fill('wrong-password')
        page.locator('#authSubmit').click()
        page.wait_for_selector('#authError:not(:empty)')
        assert 'login' in calls
        assert page.locator('#authError').inner_text() == '账号或密码不正确'
        assert page.locator('#authCard').is_visible()
        assert page.locator('#authSubmit').is_enabled()
        page.locator('#authReplay').click()
        page.clock.run_for(500)
        page.keyboard.press('Escape')
        page.clock.run_for(1200)
        assert page.locator('#authUsername').input_value() == 'admin'
        assert page.locator('#authPassword').input_value() == 'wrong-password'
        assert page.locator('#authUsername').evaluate('(e) => e === document.activeElement')
        page.evaluate('showAuth(false, "会话已过期，请重新登录。")')
        assert page.locator('#authSkip').is_hidden()
        assert page.locator('#authCard').is_visible()
        page.locator('#authReplay').click()
        page.clock.run_for(300)
        page.emulate_media(reduced_motion='reduce')
        page.wait_for_function('!document.getElementById("authCard").inert')
        assert not page.locator('#authCard').evaluate('(e) => e.inert')
        assert page.locator('#authReplay').is_hidden()
        for width, height in [(390, 844), (320, 568), (768, 1024), (1024, 768), (1920, 1080)]:
            page.set_viewport_size(dict(width=width, height=height))
            assert page.evaluate('document.documentElement.scrollWidth <= innerWidth'), (width, 'horizontal overflow')
            assert page.locator('#authSubmit').is_visible()
            if width == 390:
                page.screenshot(path=str(artifacts / '05-login-mobile.png'), full_page=True)
        assert not errors, errors

        setup = browser.new_page(viewport=dict(width=390, height=844), reduced_motion='reduce')
        setup_calls = mock_api(setup, setup=True)
        setup.goto(url)
        setup.wait_for_selector('#authSubmit')
        assert setup.locator('#authSkip').is_hidden()
        assert setup.locator('#authPassword').get_attribute('autocomplete') == 'new-password'
        assert setup.locator('#authPassword').evaluate('(e) => e.minLength') == 12
        assert setup.locator('#authPasswordHint').is_visible()
        setup.locator('#authUsername').fill('admin')
        setup.locator('#authPassword').fill('short')
        setup.locator('#authSubmit').click()
        assert 'setup' not in setup_calls, 'native validation must block short setup passwords'
        setup.locator('#authPassword').fill('long-enough-password')
        setup.locator('#authSubmit').click()
        setup.wait_for_selector('#authError:not(:empty)')
        assert 'setup' in setup_calls

        restored = browser.new_page()
        mock_api(restored, authenticated=True)
        restored.goto(url)
        restored.wait_for_selector('#page-dashboard')
        assert restored.locator('#authPanel').is_hidden()
        restored.locator('#logoutButton').click()
        assert restored.locator('#authSkip').is_hidden()
        assert not restored.locator('#authCard').evaluate('(e) => e.inert')
        browser.close()
    print('Auth browser checks passed: progressive drawing, centered-to-split layout, focus, skip/replay, errors, setup, session restore and responsive/reduced-motion states.')
finally:
    server.shutdown()
