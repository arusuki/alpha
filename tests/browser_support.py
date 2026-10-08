"""Local browser fixtures for the control UI and proxied node pages."""
import json
import mimetypes
import os
import threading
import subprocess
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from urllib.parse import urlsplit

REPO = Path(__file__).resolve().parents[1]
NODE_ID = 'e' * 32
NODE_PATH = f'/nodes/{NODE_ID}/'
PROXY_PATH = f'/api/cluster/nodes/{NODE_ID}'


def snapshot_view(data, path=''):
    result = subprocess.run(['node', str(REPO / 'tests/snapshot_view_fixture.js')],
                            input=json.dumps(dict(data=data, path=path)), text=True, capture_output=True, check=True)
    return json.loads(result.stdout)


def launch_options():
    options = dict(headless=True, args=['--no-sandbox'])
    if os.environ.get('PROJECT_ALPHA_BROWSER_EXECUTABLE'):
        options['executable_path'] = os.environ['PROJECT_ALPHA_BROWSER_EXECUTABLE']
    return options


def start_server(handler):
    server = ThreadingHTTPServer(('127.0.0.1', 0), handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    return server


class BrowserHandler(BaseHTTPRequestHandler):
    def log_message(self, *args):
        pass

    def respond(self, value, status=200):
        raw = json.dumps(value).encode()
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)

    def serve_asset(self):
        path = urlsplit(self.path).path
        target = REPO / 'dist' / ('index.html' if path == '/' else path.lstrip('/'))
        if not target.is_file() or REPO / 'dist' not in target.resolve().parents:
            self.send_error(404)
            return
        raw = target.read_bytes()
        self.send_response(200)
        self.send_header('Content-Type', mimetypes.guess_type(str(target))[0] or 'application/octet-stream')
        self.send_header('Content-Length', str(len(raw)))
        self.end_headers()
        self.wfile.write(raw)


class NodeHandler(BrowserHandler):
    def parse_request(self):
        if not super().parse_request():
            return False
        if self.path.startswith(PROXY_PATH + '/api/'):
            self.path = self.path[len(PROXY_PATH):]
        elif urlsplit(self.path).path == NODE_PATH:
            self.path = '/' + ('?' + urlsplit(self.path).query if urlsplit(self.path).query else '')
        return True

    def control_request(self):
        path = urlsplit(self.path).path
        if path == PROXY_PATH:
            self.respond(dict(id=NODE_ID, name='Test node', kind='worker', online=True))
        elif path == '/api/members':
            self.respond(dict(members=[dict(username='bob')]))
        elif path == '/api/cluster/overview':
            self.respond(dict(nodes=[], members=[], online=0, container_count=0, checked_at=1, partial=False))
        else:
            return False
        return True
