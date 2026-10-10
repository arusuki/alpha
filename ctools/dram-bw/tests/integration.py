#!/usr/bin/env python3
"""Real sockets/memfds across processes; mock hardware is always explicit."""
import array
import argparse
import ctypes as C
import errno
import json
import math
import mmap
import os
from pathlib import Path
import signal
import socket
import struct
import subprocess
import sys
import tempfile
import time

MAGIC, VERSION = 0x44525742, 1
MSG = struct.Struct("=IIIIiI")


def connect(path):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_SEQPACKET)
    s.settimeout(2)
    s.connect(path)
    return s


def exchange(s, op, capacity=0):
    s.send(MSG.pack(MAGIC, VERSION, op, capacity, 0, 0))
    data, anc, flags, _ = s.recvmsg(MSG.size, socket.CMSG_SPACE(12))
    fds = []
    for level, kind, payload in anc:
        assert (level, kind) == (socket.SOL_SOCKET, socket.SCM_RIGHTS)
        a = array.array("i")
        a.frombytes(payload)
        fds.extend(a)
    return MSG.unpack(data), fds


def wait_ready(p, path):
    deadline = time.monotonic() + 5
    while time.monotonic() < deadline:
        if p.poll() is not None:
            raise AssertionError("daemon exited: " + str(p.returncode))
        try:
            with connect(path):
                return
        except (FileNotFoundError, ConnectionRefusedError):
            pass
        time.sleep(.005)
    raise AssertionError("daemon startup timeout")


def timer_recovery_test(lib, directory):
    """A stalled sampler must not catch up with sub-period measurements."""
    path = directory + "/timer.sock"
    diagnostics = directory + "/diagnostics.jsonl"
    command = [str(BUILD / "dram-bwd"), "--backend", "mock", "--socket", path,
               "--interval-us", "20000", "--diagnostics", diagnostics]
    p = subprocess.Popen(command)
    handle = C.c_void_p()
    try:
        wait_ready(p, path)
        assert lib.dbw_open(C.byref(handle), path.encode(), 64) == 0
        assert lib.dbw_start(handle) == 0
        records = []
        buf = C.create_string_buffer(64 * 64)
        frame = struct.Struct("=QQQddddII")

        def receive():
            assert lib.dbw_wait(handle, 2000) == 1
            n = lib.dbw_read_batch(handle, buf, 64)
            assert n > 0
            records.extend(frame.unpack_from(buf, i * 64) for i in range(n))

        receive()
        p.send_signal(signal.SIGSTOP)
        time.sleep(.11)  # 5.5 periods: periodic timers would soon catch up.
        p.send_signal(signal.SIGCONT)
        while len(records) < 16:
            receive()
        assert lib.dbw_stop(handle) == 0
        assert min(r[2] for r in records) >= 20000000
        assert any(r[7] >= 4 and r[8] & 8 for r in records)  # missed / LATE
        assert all(r[5] == 1.5e9 and math.isnan(r[6]) for r in records)
        assert all(r[8] & 64 and not r[8] & 2 for r in records)  # peak optional
        rows = [json.loads(line) for line in Path(diagnostics).read_text().splitlines()]
        assert rows[0]["type"] == "session"
        samples = [r for r in rows if r["type"] == "sample"]
        assert len(samples) >= len(records)
        assert all(r["interval_ns"] >= r["target_delay_ns"] for r in samples)
    finally:
        if handle.value:
            lib.dbw_close(handle)
        if p.poll() is None:
            p.send_signal(signal.SIGCONT)
            p.terminate()
            p.wait(timeout=5)
    # Diagnostic logs append across runs and refuse a symlink.
    before = Path(diagnostics).read_text()
    p = subprocess.Popen(command)
    try:
        wait_ready(p, path)
    finally:
        p.terminate()
        p.wait(timeout=5)
    assert Path(diagnostics).read_text().startswith(before)
    link = directory + "/link.jsonl"
    os.symlink(diagnostics, link)
    r = subprocess.run(command[:-1] + [link], capture_output=True, timeout=5)
    assert r.returncode != 0


def server_wrapper_test(directory):
    """The relocated wrapper works outside the tool directory and owns cleanup."""
    root = Path(__file__).resolve().parents[1]
    server_directory = Path(directory) / "server"
    command = [sys.executable, str(root / "scripts/server.py"),
               "--directory", str(server_directory), "--backend", "mock",
               "--interval-us", "1000"]
    # Exercise the default path for normal builds and the override for others.
    if BUILD != root / "build":
        command.extend(["--build", str(BUILD)])
    process = subprocess.Popen(command, cwd=directory)
    path = str(server_directory / "control.sock")
    try:
        wait_ready(process, path)
        deadline = time.monotonic() + 5
        metadata_path = server_directory / "current.json"
        while not metadata_path.exists():
            assert process.poll() is None and time.monotonic() < deadline
            time.sleep(.01)
        metadata = json.loads(metadata_path.read_text())
        assert metadata["socket"] == path and not metadata["reference"]
        inode = os.stat(path).st_ino
        second = subprocess.run(command, cwd=directory, capture_output=True, timeout=5)
        assert second.returncode != 0 and os.stat(path).st_ino == inode
        output = subprocess.check_output([str(BUILD / "dram-bw-consume"), path, "5"], timeout=5)
        assert len(output.splitlines()) == 6
    finally:
        process.terminate()
        process.wait(timeout=10)
    assert process.returncode == 0 and not os.path.exists(path)
    assert (Path(metadata["session"]) / "diagnostics.jsonl").exists()


def persisted_settings_test(directory):
    settings = Path(directory) / "service-settings"
    path = directory + "/settings.sock"
    # Persisted settings override the deployment flags, on every restart.
    command = [str(BUILD / "dram-bwd"), "--backend", "auto", "--interval-us", "100000",
               "--socket", path, "--settings", str(settings)]
    for interval, peak in [(20000, 3), (50000, 0)]:
        settings.write_text(f"mock {interval} {peak}\n")
        process = subprocess.Popen(command)
        try:
            wait_ready(process, path)
            with connect(path) as client:
                response, fds = exchange(client, 1, 8)
                assert response[4] == 0
                try:
                    with mmap.mmap(fds[0], 4096, prot=mmap.PROT_READ) as header:
                        assert struct.unpack_from("=Qd", header, 16) == (interval * 1000, peak * 1e9)
                finally:
                    for fd in fds:
                        os.close(fd)
            output = subprocess.check_output([str(BUILD / "dram-bw-consume"), path, "3"], timeout=5)
            assert len(output.splitlines()) == 4
        finally:
            process.terminate()
            assert process.wait(timeout=5) == 0
        assert not os.path.exists(path)
    for content in ["mock 0 3", "mock -1 3", "mock 10000001 3", "auto 1000 nan",
                    "mock 1000 -1", "mock 1000 1000001", "unknown 1000 3",
                    "mock 1000 3 extra", "mock 1000 3\nextra", "[]", ""]:
        settings.write_text(content)
        result = subprocess.run(command, capture_output=True, timeout=5)
        assert result.returncode != 0 and b"Invalid DRAM settings" in result.stderr
        assert settings.read_text() == content and not os.path.exists(path)
    settings.unlink()
    # First deployment has no saved settings; explicit CLI flags still work.
    process = subprocess.Popen(command + ["--backend", "mock"])
    try:
        wait_ready(process, path)
    finally:
        process.terminate()
        assert process.wait(timeout=5) == 0


def run():
    with tempfile.TemporaryDirectory(prefix="dbw-test-") as directory:
        path = directory + "/control.sock"
        command = [str(BUILD / "dram-bwd"), "--backend", "mock", "--socket", path,
                   "--interval-us", "1000", "--peak-gbps", "3"]
        p = subprocess.Popen(command)
        try:
            wait_ready(p, path)
            # A second daemon must not delete or replace the original socket.
            inode = os.stat(path).st_ino
            second = subprocess.run(command, capture_output=True, timeout=5)
            assert second.returncode != 0 and os.stat(path).st_ino == inode
            clients = [subprocess.Popen([str(BUILD / "test-client"), path]) for _ in range(4)]
            for c in clients:
                assert c.wait(timeout=10) == 0
            fd_baseline = len(os.listdir(f"/proc/{p.pid}/fd"))
            for _ in range(30):
                with connect(path) as s:
                    response, fds = exchange(s, 1, 3)
                    assert response[4] == errno.EINVAL and not fds
                    response, fds = exchange(s, 99)
                    assert response[4] == errno.ENOTCONN
            # Invalid ancillary descriptors must be closed by the daemon.
            for _ in range(10):
                with connect(path) as s, open("/dev/null") as f:
                    s.sendmsg([MSG.pack(MAGIC, VERSION, 1, 8, 0, 0)],
                              [(socket.SOL_SOCKET, socket.SCM_RIGHTS, array.array("i", [f.fileno()]))])
                    assert s.recv(64) == b""
            with connect(path) as s:
                s.send(MSG.pack(MAGIC, VERSION + 1, 1, 8, 0, 0))
                assert s.recv(64) == b""
            # Hostile shared cursor: isolate and close this peer, preserve daemon.
            with connect(path) as s:
                response, fds = exchange(s, 1, 8)
                assert response[4] == 0 and len(fds) == 3
                try:
                    with mmap.mmap(fds[1], 4096) as cursor:
                        response, _ = exchange(s, 2)
                        assert response[4] == 0
                        struct.pack_into("=Q", cursor, 0, 2**64 - 1)
                        assert s.recv(64) == b""
                finally:
                    for fd in fds:
                        os.close(fd)
            time.sleep(.03)
            assert len(os.listdir(f"/proc/{p.pid}/fd")) <= fd_baseline
            output = subprocess.check_output([str(BUILD / "dram-bw-consume"), path, "5"], timeout=5)
            assert len(output.splitlines()) == 6
            # Build a C-API handle in this process, then test SIGKILL with queued data.
            lib = C.CDLL(str(BUILD / "libdram_bw.so"), use_errno=True)
            lib.dbw_open.argtypes = [C.POINTER(C.c_void_p), C.c_char_p, C.c_uint32]
            for name in ("dbw_start", "dbw_stop", "dbw_close"):
                getattr(lib, name).argtypes = [C.c_void_p]
            lib.dbw_wait.argtypes = [C.c_void_p, C.c_int]
            lib.dbw_read_batch.argtypes = [C.c_void_p, C.c_void_p, C.c_size_t]
            lib.dbw_read_batch.restype = C.c_ssize_t
            handle = C.c_void_p()
            assert lib.dbw_open(C.byref(handle), path.encode(), 8) == 0
            try:
                assert lib.dbw_start(handle) == 0
                assert lib.dbw_wait(handle, 1000) == 1
                p.kill()
                p.wait(timeout=5)
                assert lib.dbw_wait(handle, 100) == 1
                buf = C.create_string_buffer(8 * 64)
                assert lib.dbw_read_batch(handle, buf, 8) > 0
                assert lib.dbw_wait(handle, 100) == -1 and C.get_errno() == errno.EPIPE
            finally:
                lib.dbw_close(handle)
            # SIGKILL leaves a socket; explicit cleanup is intentional.
            os.unlink(path)
            p = subprocess.Popen(command)
            wait_ready(p, path)
            p.send_signal(signal.SIGTERM)
            assert p.wait(timeout=5) == 0 and not os.path.exists(path)
        finally:
            if p.poll() is None:
                p.terminate()
                p.wait(timeout=5)
        # Refuse unsupported backend and invalid numerical configuration.
        for args in (["--backend", "typo"], ["--interval-us", "0"],
                     ["--peak-gbps", "nan"], ["--socket-mode", "888"],
                     ["--interval-us", " -18446744073709451616"]):
            r = subprocess.run([str(BUILD / "dram-bwd")] + args, capture_output=True, timeout=5)
            assert r.returncode != 0
        timer_recovery_test(lib, directory)
        server_wrapper_test(directory)
        persisted_settings_test(directory)
    print("integration: concurrent clients, protocol, fd leaks, crash, timer recovery, diagnostics, server wrapper passed")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description="Run integration tests using temporary mock daemons and local sockets; no PMU access needed.",
        epilog="Build the required binaries and run this suite with: make test",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    parser.add_argument("build", type=Path, help="build directory containing dram-bwd, dram-bw-consume, test-client and libdram_bw.so")
    BUILD = parser.parse_args().build.resolve()
    run()
