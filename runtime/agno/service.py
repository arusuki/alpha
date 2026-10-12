"""Local container service. The socket directory is the authentication boundary."""
import argparse
import fcntl
import importlib.util
import json
import os
from pathlib import Path
import select
import signal
import socket
import socketserver
import stat
import subprocess
import sys
import threading
import time

PROTOCOL = 2
RUNTIME = Path(__file__).with_name("runtime.py")
spec = importlib.util.spec_from_file_location("alpha_runtime", RUNTIME)
runtime = importlib.util.module_from_spec(spec)
spec.loader.exec_module(runtime)


def send(connection, value):
    connection.sendall((json.dumps(value) + "\n").encode())


class Service(socketserver.ThreadingMixIn, socketserver.UnixStreamServer):
    daemon_threads = False
    request_queue_size = 16

    def __init__(self, path):
        self.stopping = threading.Event()
        self.slots = threading.BoundedSemaphore(4)
        self.mutex = threading.Lock()
        self.active = 0
        super().__init__(path, Handler)

    def health(self):
        with self.mutex:
            return dict(protocol=PROTOCOL, ready=not self.stopping.is_set(),
                        agno_version=runtime.AGNO_VERSION, openai_version=runtime.OPENAI_VERSION, mcp_version=runtime._mcp.MCP_VERSION,
                        active_runs=self.active, capacity=4)

    def handle_error(self, request, client_address):
        # Never print request frames, model exceptions, or credentials.
        pass


class Handler(socketserver.BaseRequestHandler):
    def handle(self):
        connection = self.request
        connection.settimeout(3)
        # Unbuffered so a run frame following the greeting is not consumed here.
        with connection.makefile("rb", buffering=0) as reader:
            line = reader.readline(1025)
        if len(line) > 1024 or not line.endswith(b"\n"):
            return
        try:
            hello = json.loads(line)
        except (ValueError, UnicodeError):
            return
        if not isinstance(hello, dict) or set(hello) != {"operation", "protocol"} or hello["protocol"] != PROTOCOL:
            send(connection, dict(type="error", code="protocol"))
            return
        if hello["operation"] == "health":
            send(connection, dict(type="health", **self.server.health()))
            return
        if hello["operation"] != "run":
            send(connection, dict(type="error", code="protocol"))
            return
        if self.server.stopping.is_set():
            send(connection, dict(type="error", code="unavailable"))
            return
        if not self.server.slots.acquire(blocking=False):
            send(connection, dict(type="error", code="busy"))
            return
        child = None
        with self.server.mutex:
            self.server.active += 1
        try:
            # A child per run preserves isolation and makes cancellation reliable
            # even while the model client is blocked on a network read.
            connection.settimeout(None)
            child = subprocess.Popen(
                [sys.executable, "-I", "-u", str(RUNTIME)], stdin=connection, stdout=connection,
                stderr=subprocess.DEVNULL, start_new_session=True,
                env={"PATH": "/usr/local/bin:/usr/bin:/bin", "LANG": "C.UTF-8", "AGNO_TELEMETRY": "false"},
            )
            send(connection, dict(type="ready", **self.server.health()))
            deadline = time.monotonic() + 15 * 60
            while child.poll() is None and not self.server.stopping.is_set() and time.monotonic() < deadline:
                readable, _, _ = select.select([connection], [], [], 0.1)
                if readable:
                    try:
                        if not connection.recv(1, socket.MSG_PEEK | socket.MSG_DONTWAIT):
                            break  # The control cancelled or disconnected.
                    except BlockingIOError:
                        pass  # The child consumed the data after select().
                if readable:
                    # The child consumes input; avoid spinning on its pending data.
                    self.server.stopping.wait(0.02)
        finally:
            if child is not None:
                if child.poll() is None:
                    try:
                        os.killpg(child.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                child.wait()
            with self.server.mutex:
                self.server.active -= 1
            self.server.slots.release()


def serve(path):
    os.environ["AGNO_TELEMETRY"] = "false"
    runtime.check_dependencies()
    if not path.is_absolute():
        raise ValueError("Agno socket path must be absolute")
    # The deployment owner creates the directory with the control service's UID.
    # Never replace arbitrary files or another live service's socket.
    lock = os.open(str(path) + ".lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        if path.exists() or path.is_symlink():
            info = path.lstat()
            if not stat.S_ISSOCK(info.st_mode) or info.st_uid != os.getuid():
                raise ValueError("Refusing to replace an unowned or non-socket Agno path")
            with socket.socket(socket.AF_UNIX) as probe:
                probe.settimeout(1)
                try:
                    probe.connect(str(path))
                except ConnectionRefusedError:
                    pass
                else:
                    raise ValueError("Agno socket is already in use")
            path.unlink()
        os.umask(0o077)
        with Service(str(path)) as server:
            os.chmod(path, 0o600)
            def stop(*_):
                server.stopping.set()
                threading.Thread(target=server.shutdown, daemon=True).start()
            signal.signal(signal.SIGTERM, stop)
            signal.signal(signal.SIGINT, stop)
            try:
                server.serve_forever(poll_interval=0.1)
            finally:
                server.stopping.set()
                path.unlink(missing_ok=True)
    finally:
        os.close(lock)


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--socket", type=Path, default=Path("/run/agno/agno.sock"))
    args = parser.parse_args()
    try:
        serve(args.socket)
    except Exception:
        # Do not emit provider/import traces that could include configuration.
        sys.stderr.write("Agno service startup failed: check pinned dependencies, socket ownership and existing service.\n")
        sys.exit(1)
