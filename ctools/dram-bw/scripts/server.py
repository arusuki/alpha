#!/usr/bin/env python3
"""Foreground daemon with an optional, independently controlled perf reference."""
import argparse
from datetime import datetime, timezone
import fcntl
import json
import os
from pathlib import Path
import select
import shlex
import signal
import stat
import subprocess
import tempfile
import time


def main():
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.ArgumentDefaultsHelpFormatter,
        epilog="Usual entry point: make -j, then sudo make server. "
               "Use SERVER_REFERENCE=1 for perf comparison. Ctrl-C stops owned processes; "
               "session logs are retained. Requires a built dram-bwd; hardware mode needs perf permissions.",
    )
    parser.add_argument("--build", type=Path, default=Path(__file__).resolve().parents[1] / "build",
                        help="directory containing dram-bwd")
    parser.add_argument("--directory", type=Path, default=Path("/run/dram-bw"),
                        help="directory for control.sock, current.json and session logs")
    parser.add_argument("--backend", choices=("amd-rome", "mock"), default="amd-rome",
                        help="hardware backend, or explicit synthetic data")
    parser.add_argument("--interval-us", type=int, default=1000000,
                        help="daemon delay after each scan, in microseconds (100..10000000)")
    parser.add_argument("--uid", type=int, default=os.getuid(), help="host UID allowed to connect")
    parser.add_argument("--gid", type=int, default=os.getgid(), help="group owning the socket and logs")
    parser.add_argument("--reference", type=int, choices=(0, 1), default=0,
                        help="1 starts perf stat with fixed 1-second windows, initially disabled; amd-rome only")
    args = parser.parse_args()
    if not 100 <= args.interval_us <= 10000000 or min(args.uid, args.gid) < 0:
        parser.error("invalid interval or uid/gid")
    if args.reference and args.backend != "amd-rome":
        parser.error("perf reference requires the real amd-rome backend")
    if os.geteuid() != 0 and (args.uid != os.getuid() or args.gid != os.getgid()):
        parser.error("changing the client uid/gid requires root")

    os.umask(0o027)
    # Keep PMU privileges, but give socket/logs the invoking user's group.
    if os.geteuid() == 0:
        os.setgroups([])
        os.setgid(args.gid)
    directory = args.directory.absolute()
    directory.mkdir(mode=0o750, exist_ok=True)
    info = directory.lstat()
    if (not stat.S_ISDIR(info.st_mode) or info.st_uid != os.geteuid()
            or info.st_mode & 0o022):
        raise RuntimeError("server directory must be owned by the server uid, without group/other writes")
    lock = os.open(directory / "server.lock", os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o640)
    try:
        fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
    except BlockingIOError:
        raise RuntimeError("another make server is already running") from None
    os.chown(directory, -1, args.gid)
    os.chmod(directory, 0o750)
    socket = directory / "control.sock"
    if socket.exists() or socket.is_symlink():
        raise RuntimeError(f"refusing to replace existing socket: {socket}")
    session = Path(tempfile.mkdtemp(prefix="session-", dir=directory))
    session.chmod(0o750)
    daemon_command = [str(args.build.resolve() / "dram-bwd"),
                      "--backend", args.backend, "--socket", str(socket),
                      "--interval-us", str(args.interval_us), "--socket-mode", "0660",
                      "--allow-uid", str(args.uid), "--diagnostics", str(session / "diagnostics.jsonl")]
    metadata = {"created_utc": datetime.now(timezone.utc).isoformat(),
                "socket": str(socket), "session": str(session), "uid": args.uid,
                "gid": args.gid, "daemon_command": daemon_command, "reference": bool(args.reference)}
    children, files, fifo_fds = [], [], []
    stopping = False

    def stop(signum, frame):
        nonlocal stopping
        stopping = True

    signal.signal(signal.SIGINT, stop)
    signal.signal(signal.SIGTERM, stop)
    try:
        print("daemon:", shlex.join(daemon_command), flush=True)
        daemon = subprocess.Popen(daemon_command, start_new_session=True)
        children.append(daemon)
        deadline = time.monotonic() + 10
        while not socket.exists():
            if stopping:
                return 0
            if daemon.poll() is not None:
                raise RuntimeError(f"daemon exited ({daemon.returncode})")
            if time.monotonic() >= deadline:
                raise RuntimeError("daemon startup timed out")
            time.sleep(0.02)

        if args.reference:
            cpus = Path("/sys/bus/event_source/devices/amd_df/cpumask").read_text().strip()
            # Native perf aliases use perf's own Zen 2 event table and encoding.
            events = [f"dram_channel_data_controller_{i}" for i in range(8)]
            control, ack = session / "perf-control.fifo", session / "perf-ack.fifo"
            for path in (control, ack):
                os.mkfifo(path, 0o660)
                path.chmod(0o660)
                fifo_fds.append(os.open(path, os.O_RDWR | os.O_NONBLOCK))
            perf_command = ["perf", "stat", "-a", "-A", "-C", cpus,
                            "--no-big-num", "-x", ";", "-I", "1000", "-D", "-1",
                            "--control", f"fd:{fifo_fds[0]},{fifo_fds[1]}",
                            "--log-fd", "1"]
            for event in events:
                perf_command.extend(["-e", event])
            output = (session / "perf.csv").open("x")
            errors = (session / "perf.stderr").open("x")
            files.extend([output, errors])
            environment = dict(os.environ, LC_ALL="C")
            # Bounds cover process launch, not perf's internal interval epoch.
            metadata["perf_launch_before_ns"] = time.monotonic_ns()
            print("reference:", shlex.join(perf_command), flush=True)
            reference = subprocess.Popen(perf_command, stdout=output, stderr=errors,
                                         pass_fds=tuple(fifo_fds), env=environment,
                                         start_new_session=True)
            children.append(reference)
            metadata["perf_launch_after_ns"] = time.monotonic_ns()
            os.write(fifo_fds[0], b"disable\n")
            deadline = time.monotonic() + 10
            while not select.select([fifo_fds[1]], [], [], 0.1)[0]:
                if stopping:
                    return 0
                if reference.poll() is not None or time.monotonic() >= deadline:
                    raise RuntimeError("perf failed to become ready:\n" + (session / "perf.stderr").read_text())
            response = os.read(fifo_fds[1], 64)
            # perf 5.15 writes sizeof("ack\n"), including the trailing NUL.
            if response not in (b"ack\n", b"ack\n\0"):
                raise RuntimeError(f"unexpected perf control acknowledgement: {response!r}")
            metadata.update(perf_command=perf_command, cpumask=cpus, events=events,
                            perf_control=str(control), perf_ack=str(ack),
                            perf_output=str(session / "perf.csv"), bytes_per_count=64,
                            perf_ready_ns=time.monotonic_ns())

        (session / "session.json").write_text(json.dumps(metadata, indent=2) + "\n")
        # Protected directory + atomic replacement keeps the discovery file safe.
        current = directory / "current.json.tmp"
        fd = os.open(current, os.O_WRONLY | os.O_CREAT | os.O_TRUNC | os.O_NOFOLLOW, 0o640)
        with os.fdopen(fd, "w") as output:
            json.dump(metadata, output, indent=2)
            output.write("\n")
        os.replace(current, directory / "current.json")
        print(f"READY socket={socket} uid={args.uid} session={session}", flush=True)
        if args.reference:
            print("perf is ready with counters disabled; enable through perf-control.fifo for comparison.", flush=True)
        print("Ctrl-C stops both processes; logs are retained.", flush=True)
        while not stopping:
            for child in children:
                if child.poll() is not None:
                    raise RuntimeError(f"child {child.args[0]} exited ({child.returncode}); see {session}")
            time.sleep(0.2)
        return 0
    finally:
        for child in reversed(children):
            if child.poll() is None:
                child.send_signal(signal.SIGINT)
        for child in reversed(children):
            try:
                child.wait(timeout=5)
            except subprocess.TimeoutExpired:
                child.kill()
                child.wait()
        for fd in fifo_fds:
            os.close(fd)
        for output in files:
            output.close()
        os.close(lock)


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, RuntimeError) as error:
        raise SystemExit(f"server: {error}") from None
