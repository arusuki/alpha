"""Server display views: lazy directory reads, failures, accounting and mobile."""
import json
from pathlib import Path
from urllib.parse import urlparse, parse_qs
from browser_support import NodeHandler, NODE_PATH, launch_options, start_server, snapshot_view
from playwright.sync_api import sync_playwright

repo = Path(__file__).resolve().parents[1]
sample = json.loads((repo / 'tests/fixtures/snapshot.json').read_text())
record = 'a' * 32
sample.update(job_id=record, revision=0)
def node(path, size, children=None):
    return dict(name=path.rsplit('/', 1)[-1], path=path, kind='directory', allocated=size,
                apparent=size, files=1, errors=0, children=children or [])
leaf = node('/host-data/nested', 4096, [node('/host-data/nested/file', 4096)])
sample['tree']['children'].append(node('/host-data', 4096, [leaf]))
sample['tree']['allocated'] += 4096
sample['tree']['apparent'] += 4096
job = dict(id=record, status='completed', trigger='manual', created_at=1, finished_at=2,
           created_by='admin', config={}, allocated=sample['tree']['allocated'], snapshot_revision=0)
requests = []
fail_directory = False

class Handler(NodeHandler):
    def do_GET(self):
        if self.control_request(): return
        parsed = urlparse(self.path)
        requests.append(self.path)
        if parsed.path == '/api/session':
            return self.respond(dict(user=dict(id='admin', username='admin', role='admin'), csrf='test'))
        if parsed.path == '/api/state':
            return self.respond(dict(jobs=[job], directory_jobs=[], latest_id=record, active=None, interval_minutes=0))
        if parsed.path.endswith('/events'):
            self.send_response(204); self.end_headers(); return
        if parsed.path.endswith('/view'):
            path = parse_qs(parsed.query).get('path', [''])[0]
            if fail_directory and path == '/host-data': return self.respond(dict(error='目录暂时不可用'), 503)
            return self.respond(snapshot_view(sample, path))
        return self.serve_asset()

server = start_server(Handler)
try:
    with sync_playwright() as p:
        browser = p.chromium.launch(**launch_options())
        page = browser.new_page(viewport=dict(width=1440, height=1000))
        errors = []
        page.on('pageerror', lambda error: errors.append(str(error)))
        page.goto('http://127.0.0.1:' + str(server.server_port) + NODE_PATH)
        page.locator('.platform-nav [data-page="overview"]').click()
        page.wait_for_function('platform.loaded !== null && !platform.resultLoad')
        assert page.evaluate('usage.remote')
        assert not page.evaluate('usage.nodes.has("/host-data/nested")')
        page.evaluate('() => {Usage.build=()=>{throw Error("browser accounting must not run")};}')
        page.locator('.host-explorer-entry').click()
        fail_directory = True
        page.locator('[data-storage-entry]').filter(has_text='/host-data').click()
        page.locator('[data-retry-directory]').wait_for()
        assert '目录暂时不可用' in page.locator('#explorerContent').inner_text()
        fail_directory = False
        page.locator('[data-retry-directory]').click()
        page.locator('[data-storage-entry]').filter(has_text='nested').wait_for()
        assert not page.evaluate('usage.nodes.has("/host-data/nested/file")')
        page.locator('[data-storage-entry]').filter(has_text='nested').click()
        page.locator('[data-storage-entry]').filter(has_text='file').wait_for()
        assert page.evaluate('explorer.trail.at(-1).path') == '/host-data/nested'
        assert page.evaluate('usage.nodes.has("/host-data/nested/file")')
        page.locator('[data-storage-up]').click()
        page.locator('[data-storage-entry]').filter(has_text='nested').wait_for()
        page.locator('#mapSearch').fill('nested')
        sample['revision'] += 1
        job['snapshot_revision'] += 1
        page.evaluate('loadSnapshotChanges()')
        assert page.evaluate('snapshot.revision') == 1
        assert page.locator('#mapSearch').input_value() == 'nested'
        assert page.evaluate('explorer.trail.at(-1).path') == '/host-data'
        # A late directory response must not pull the user back after switching sources.
        page.evaluate("""() => {
          window.originalRead=SnapshotLoader.read;
          SnapshotLoader.read=async (...args)=>{
            const result=await originalRead(...args);
            await new Promise(resolve=>window.releaseDirectory=resolve);
            return result;
          };
        }""")
        page.locator('[data-storage-entry]').filter(has_text='nested').click()
        page.wait_for_function('typeof releaseDirectory === "function"')
        page.locator('[data-storage-source="1"]').click()
        page.evaluate('() => {SnapshotLoader.read=originalRead;releaseDirectory();}')
        page.wait_for_function('directoryViewLoad === null')
        assert page.evaluate('explorer.trail.at(-1).path') == sample['tree']['path']
        page.set_viewport_size(dict(width=390, height=844))
        assert page.evaluate('document.documentElement.scrollWidth <= innerWidth')
        page.locator('#containerDialog').screenshot(path='/tmp/project-alpha-snapshot-view.png')
        assert not any('/snapshot' in path or '/changes' in path for path in requests if path.startswith('/api/'))
        assert not errors, errors
        browser.close()
        print('Server display browser checks passed: lazy reads, retry, no browser accounting, live refresh and mobile.')
finally:
    server.shutdown()
