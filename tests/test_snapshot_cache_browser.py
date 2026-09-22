"""Raw snapshot caching with real IndexedDB, Workers and conditional HTTP requests."""
import hashlib
import json
import mimetypes
import os
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlparse
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
record = 'c' * 32
sample = json.loads((repo / 'tests/fixtures/snapshot.json').read_text())
sample.update(job_id=record, revision=0)
user = dict(id='reader-one', username='reader', role='viewer')
job = dict(id=record, status='completed', trigger='manual', created_by='admin',
           created_at=1, finished_at=2, snapshot_revision=0, allocated=sample['tree']['allocated'])
requests = []
response_status = 200
cache_failure = ''


def encoded():
    return json.dumps(sample, ensure_ascii=False).encode()


class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def respond(self, body, status=200, headers=None):
        raw = body if isinstance(body, bytes) else json.dumps(body).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        if status != 304:
            self.wfile.write(raw)

    def do_GET(self):
        path = urlparse(self.path).path
        if path == '/api/session':
            return self.respond(dict(user=user, csrf='test', setup_required=False))
        if path == '/api/state':
            return self.respond(dict(jobs=[job], directory_jobs=[], latest_id=record, active=None, interval_minutes=0))
        if path.endswith('/events'):
            self.send_response(204)
            self.end_headers()
            return
        if path.endswith('/changes'):
            return self.respond(dict(job_id=record, base_revision=sample['revision'], revision=sample['revision'],
                metadata={k: v for k, v in sample.items() if k != 'tree'}, replacements=[], ancestors=[]))
        if path.endswith('/snapshot'):
            raw = encoded()
            etag = '"' + hashlib.sha256(raw).hexdigest() + '"'
            match = self.headers.get('If-None-Match')
            status = response_status if response_status != 200 else 304 if match == etag else 200
            requests.append((match, status))
            return self.respond(raw if status == 200 else b'' if status == 304 else dict(error='会话已过期'), status, {'ETag': etag})
        file = repo / 'dist' / ('index.html' if path == '/' else path.lstrip('/'))
        if not file.is_file():
            self.send_error(404)
            return
        raw = file.read_bytes()
        if path == '/snapshot-cache.js' and cache_failure:
            raw += ('\nSnapshotCache.' + cache_failure + '=async()=>{throw Error("storage unavailable")};').encode()
        self.send_response(200)
        self.send_header('Content-Type', mimetypes.guess_type(str(file))[0] or 'text/plain')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
threading.Thread(target=server.serve_forever, daemon=True).start()
try:
    with sync_playwright() as p:
        launch = dict(headless=True, args=['--no-sandbox'])
        if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
            launch['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
        browser = p.chromium.launch(**launch)
        page = browser.new_page()
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        base = 'http://127.0.0.1:' + str(server.server_port)
        page.goto(base)
        page.wait_for_function('platform.user !== null')
        page.locator('.platform-nav [data-page="overview"]').click()
        page.wait_for_function('platform.loaded !== null && platform.resultLoad === null')
        page.wait_for_function('document.getElementById("snapshotCacheSummary").textContent.startsWith("1 条")')
        assert requests == [(None, 200)], requests
        page.locator('#snapshotCachePanel summary').click()
        assert page.locator('[data-delete-cache]').count() == 1
        assert page.locator('#snapshotCacheRows .amount').get_attribute('title') == str(len(encoded())) + ' 字节'
        page.locator('#snapshotCachePanel').screenshot(path='/tmp/project-alpha-snapshot-cache.png')
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('#snapshotCachePanel').screenshot(path='/tmp/project-alpha-snapshot-cache-mobile.png')
        page.set_viewport_size(dict(width=1280, height=720))
        assert page.evaluate('async () => (await SnapshotCache.read(platform.user.id, `/api/jobs/${platform.loaded}/snapshot`)).blob.text()') == encoded().decode()
        page.reload()
        page.wait_for_function('platform.user !== null')
        page.locator('.platform-nav [data-page="overview"]').click()
        page.wait_for_function('platform.loaded !== null && platform.resultLoad === null')
        assert requests[-1][1] == 304 and len(requests) == 2, requests

        def load(scope='reader-one'):
            return page.evaluate('''async scope => {
                const progress=[];
                try {
                    const result=await SnapshotLoader.read('/api/jobs/'+'c'.repeat(32)+'/snapshot', {
                        scope, signal:new AbortController().signal, onProgress:p=>progress.push(p.detail)});
                    return {revision:result.data.revision, owner:result.data.containers[0].owner, progress, cacheError:result.cacheError};
                } catch(error) { return {status:error.status,error:error.message}; }
            }''', scope)

        assert any('本地缓存' in text for text in load()['progress'])
        sample['revision'] = 1
        sample['containers'][0]['owner'] = '新用户'
        result = load()
        assert requests[-1][1] == 200 and result['revision'] == 1 and result['owner'] == '新用户'
        assert len(page.evaluate('SnapshotCache.list("reader-one")')) == 1
        load('reader-two')
        assert requests[-1] == (None, 200), requests
        assert len(page.evaluate('SnapshotCache.list("reader-two")')) == 1
        assert len(page.evaluate('SnapshotCache.list("reader-one")')) == 1

        # Deletion invalidates writers from both this tab and other Workers.
        assert page.evaluate('''async () => {
            const url='/api/jobs/'+'c'.repeat(32)+'/snapshot';
            const old=await SnapshotCache.read('reader-one',url);
            await SnapshotCache.remove('reader-one',url);
            const saved=await SnapshotCache.save('reader-one',url,old.epoch,old.record.etag,old.blob,{job_id:'test'});
            return !saved && (await SnapshotCache.list('reader-one')).length===0;
        }''')
        load()
        assert requests[-1] == (None, 200)

        # Invalid cached JSON retries with an unconditional network request.
        page.evaluate('''async () => {
            const url='/api/jobs/'+'c'.repeat(32)+'/snapshot', state=await SnapshotCache.read('reader-one',url);
            await SnapshotCache.save('reader-one',url,state.epoch,state.record.etag,new Blob(['{broken']),{job_id:'broken'});
        }''')
        before = len(requests)
        assert load()['revision'] == 1
        assert [status for _, status in requests[before:]] == [304, 200]

        # Neither denied authorization nor network failures may serve cached data.
        response_status = 401
        assert load()['status'] == 401
        response_status = 200
        page.context.set_offline(True)
        assert 'error' in load()
        page.context.set_offline(False)
        assert load()['revision'] == 1

        # Read-only users can delete one record or all their local files.
        page.evaluate('refreshSnapshotCache()')
        page.locator('#snapshotCachePanel summary').click()
        page.locator('[data-delete-cache]').click()
        page.wait_for_function('document.getElementById("snapshotCacheSummary").textContent.startsWith("0 条")')
        load()
        assert requests[-1] == (None, 200)
        page.evaluate('refreshSnapshotCache()')
        page.locator('#clearSnapshotCache').click()
        page.wait_for_function('document.getElementById("snapshotCacheSummary").textContent.startsWith("0 条")')
        assert len(page.evaluate('SnapshotCache.list("reader-two")')) == 1
        assert page.locator('#clearSnapshotCache').is_disabled()
        assert page.evaluate('snapshot !== null'), 'Deleting a file must preserve the current view'
        for failure in ['read', 'save']:
            cache_failure = failure
            result = load()
            assert result['revision'] == 1 and result['cacheError'], result
        cache_failure = ''
        assert not errors, errors
        browser.close()
        print('Snapshot cache browser checks passed: raw bytes, reload/304, changed hash, account isolation, deletion, in-flight writes, corruption, auth/network errors and unavailable/full storage.')
finally:
    server.shutdown()
