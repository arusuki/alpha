"""Real clipboard round trips on HTTP, in modals, and after API rejection."""
from playwright.sync_api import sync_playwright, expect
from browser_support import REPO, launch_options


with sync_playwright() as p:
    browser = p.chromium.launch(**launch_options())
    page = browser.new_page()
    errors = []
    page.on('pageerror', lambda error: errors.append(str(error)))
    markup = '''<!doctype html><meta charset="utf-8">
      <script src="/clipboard.js" defer></script>
      <script src="/test.js" defer></script>
      <button id="copy">Copy</button><textarea id="paste"></textarea>
      <dialog><input id="source" value="selection to preserve">
        <button id="modalCopy">Copy in dialog</button><textarea id="modalPaste"></textarea>
      </dialog><p id="result"></p>'''
    script = r'''const payload = 'Host 计算节点\n  HostName 100.64.0.2\n  User alpha-jump\n';
      async function copy() {
        try { await AlphaClipboard.writeText(payload); result.textContent = 'copied'; }
        catch (_) { result.textContent = 'failed'; }
      }
      document.querySelector('#copy').onclick = copy;
      document.querySelector('#modalCopy').onclick = copy;
      document.querySelector('#source').onkeydown = event => {
        if (event.key === 'F2') { event.preventDefault(); copy(); }
      };'''

    def route(r):
        if r.request.url.endswith('/clipboard.js'):
            r.fulfill(content_type='application/javascript', body=(REPO / 'dist/clipboard.js').read_text())
        elif r.request.url.endswith('/test.js'):
            r.fulfill(content_type='application/javascript', body=script)
        else:
            r.fulfill(content_type='text/html', body=markup, headers={
                'Content-Security-Policy': "default-src 'none'; script-src 'self'; style-src 'self'"})

    page.route('**/*', route)
    payload = 'Host 计算节点\n  HostName 100.64.0.2\n  User alpha-jump\n'
    for origin in ('http://alpha.test', 'https://alpha.test'):
        page.goto(origin)
        if origin.startswith('http:'):
            assert page.evaluate('!isSecureContext && !navigator.clipboard')
        else:
            # Exercise both the native API and a rejected native write below.
            page.context.grant_permissions(['clipboard-read', 'clipboard-write'], origin=origin)
        page.locator('#copy').click()
        expect(page.locator('#result')).to_have_text('copied')
        page.locator('#paste').focus()
        page.keyboard.press('Control+V')
        expect(page.locator('#paste')).to_have_value(payload)

        if origin.startswith('https:'):
            page.evaluate("() => { navigator.clipboard.writeText = async () => { throw new DOMException('Denied', 'NotAllowedError'); }; }")
        page.evaluate("document.querySelector('dialog').showModal(); result.textContent = ''")
        page.locator('#modalCopy').click()
        expect(page.locator('#result')).to_have_text('copied')
        expect(page.locator('#modalCopy')).to_be_focused()
        page.locator('#modalPaste').focus()
        page.keyboard.press('Control+V')
        expect(page.locator('#modalPaste')).to_have_value(payload)
        page.locator('#source').focus()
        page.evaluate("source.setSelectionRange(2, 8); result.textContent = ''")
        page.keyboard.press('F2')
        expect(page.locator('#result')).to_have_text('copied')
        expect(page.locator('#source')).to_be_focused()
        assert page.evaluate('[source.selectionStart, source.selectionEnd]') == [2, 8]
        assert page.locator('textarea').count() == 2

        # Failure must not report success or leak the temporary field.
        page.evaluate("document.execCommand = () => false; result.textContent = ''")
        page.locator('#modalCopy').click()
        expect(page.locator('#result')).to_have_text('failed')
        expect(page.locator('#modalCopy')).to_be_focused()
        assert page.locator('textarea').count() == 2
    assert not errors, errors
    browser.close()
print('Clipboard browser checks passed: HTTP, HTTPS, denied API, modals, focus, selection and failure.')
