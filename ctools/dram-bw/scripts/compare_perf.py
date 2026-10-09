#!/usr/bin/env python3
"""Run bounded memory loads and compare dram-bw with the prepared perf session.

Requires `sudo make server SERVER_REFERENCE=1` in another terminal.
Run this program as the permitted ordinary user, never via sudo.
"""
import argparse
import csv
import json
import math
import os
from pathlib import Path
import select
import shutil
import signal
import statistics
import subprocess
import time


def cpu_list(value):
    result = []
    for item in value.split(","):
        ends = [int(part) for part in item.split("-")]
        result.extend(range(ends[0], ends[-1] + 1))
    return result


def control(metadata, command):
    ack = os.open(metadata["perf_ack"], os.O_RDWR | os.O_NONBLOCK)
    ctl = os.open(metadata["perf_control"], os.O_WRONLY | os.O_NONBLOCK)
    try:
        # There must be only one controlling process for this session.
        while select.select([ack], [], [], 0)[0]:
            os.read(ack, 4096)
        os.write(ctl, command.encode() + b"\n")
        if not select.select([ack], [], [], 5)[0]:
            raise RuntimeError("perf control timed out; check the foreground server")
        response = os.read(ack, 64)
        if response not in (b"ack\n", b"ack\n\0"):
            raise RuntimeError(f"unexpected perf acknowledgement: {response!r}")
    finally:
        os.close(ctl)
        os.close(ack)


def summary(metadata, phase, diagnostics, perf_text):
    # Exclude startup/stop edges. perf's epoch is bounded by launch and ACK;
    # only use intervals wholly inside the steady window for all such epochs.
    low, high = phase["start_ns"] + 2_000_000_000, phase["end_ns"] - 2_000_000_000
    with Path(phase["client_csv"]).open() as source:
        rows = list(csv.DictReader(source))
    rows = [r for r in rows if int(r["host_monotonic_ns"]) <= high
            and int(r["host_monotonic_ns"]) - int(r["interval_ns"]) >= low]
    if not rows:
        raise RuntimeError("no complete daemon windows (check sample count and time namespace)")
    if any(int(r["flags"]) & 1 for r in rows):
        raise RuntimeError("refusing to compare mock samples")
    valid = [r for r in rows if not int(r["flags"]) & 2 and math.isfinite(float(r["total_GBps"]))]
    if not valid:
        raise RuntimeError("no valid hardware samples")
    elapsed = sum(int(r["interval_ns"]) for r in valid)
    rate = sum(float(r["total_GBps"]) * int(r["interval_ns"]) for r in valid) / elapsed
    sequences = {int(r["sequence"]): int(r["interval_ns"]) for r in valid}
    counters = [r for r in diagnostics if r["type"] == "counter" and r["sequence"] in sequences]
    samples = [r for r in diagnostics if r["type"] == "sample" and r["sequence"] in sequences]
    per_cpu, fractions = {}, []
    for row in counters:
        if row["flags"] & 2 or not row["delta"] or not row["delta"][2]:
            raise RuntimeError("valid client sample has invalid diagnostic counter")
        cpu = str(row["cpu"])
        per_cpu[cpu] = per_cpu.get(cpu, 0) + row["delta"][0] * row["bytes_per_count"] / row["delta"][2] * sequences[row["sequence"]] / elapsed
        fractions.append(row["running_fraction"])
    result = dict(phase, daemon_GBps=rate, daemon_valid=len(valid),
                  daemon_invalid=len(rows) - len(valid), daemon_window_seconds=elapsed / 1e9,
                  daemon_per_cpu_GBps=per_cpu,
                  daemon_sample_cv=statistics.pstdev(float(r["total_GBps"]) for r in valid) / rate if rate else None,
                  daemon_running_fraction_min=min(fractions) if fractions else None,
                  daemon_running_fraction_median=statistics.median(fractions) if fractions else None,
                  scan_us_max=max(r["scan_ns"] for r in samples) / 1000 if samples else None,
                  daemon_skewed=sum(bool(int(r["flags"]) & 32) for r in rows))
    if not phase["reference"]:
        return result
    groups = {}
    for fields in csv.reader(perf_text.splitlines(), delimiter=";"):
        if len(fields) < 7 or not fields[1].startswith("CPU"):
            continue
        groups.setdefault(float(fields[0]), []).append(fields)
    expected = {(f"CPU{cpu}", event) for cpu in cpu_list(metadata["cpumask"]) for event in metadata["events"]}
    previous, seconds, total, valid_count, invalid_count = 0.0, 0.0, 0.0, 0, 0
    perf_cpu, running = {}, []
    for timestamp, records in sorted(groups.items()):
        duration = timestamp - previous
        inside = (metadata["perf_launch_before_ns"] + previous * 1e9 >= low
                  and metadata["perf_ready_ns"] + timestamp * 1e9 <= high)
        previous = timestamp
        if not inside:
            continue
        keys = {(r[1], r[4]) for r in records}
        if duration <= 0 or len(records) != len(expected) or keys != expected or any("<" in r[2] for r in records):
            invalid_count += 1
            continue
        if any(float(r[5]) <= 0 or not math.isfinite(float(r[2])) for r in records):
            invalid_count += 1
            continue
        seconds += duration
        valid_count += 1
        for row in records:
            # perf's default output is ALREADY scaled for enabled/running.
            count = float(row[2])
            total += count * 64 / 1e9
            cpu = row[1][3:]
            perf_cpu[cpu] = perf_cpu.get(cpu, 0) + count * 64 / 1e9
            running.append(float(row[6]) / 100)
    if not seconds:
        raise RuntimeError("no complete valid perf windows; inspect perf.csv and perf.stderr")
    perf_rate = total / seconds
    result.update(perf_GBps=perf_rate, perf_valid=valid_count, perf_invalid=invalid_count,
                  perf_window_seconds=seconds, perf_per_cpu_GBps={k: v / seconds for k, v in perf_cpu.items()},
                  perf_running_fraction_min=min(running), perf_running_fraction_median=statistics.median(running),
                  relative_difference_pct=100 * (rate / perf_rate - 1) if perf_rate else None)
    return result


def main():
    parser = argparse.ArgumentParser(
        description=__doc__, formatter_class=argparse.ArgumentDefaultsHelpFormatter,
        epilog="Designed for a two-node Rome host. Requires perf, taskset and the prepared server. "
               "Default conditions use up to four physical cores per node and about 4 GiB total. "
               "Writes client CSV, diagnostics, perf output, NUMA placement and results.json. "
               "Example: python3 scripts/compare_perf.py --output results/perf-comparison --solo",
    )
    parser.add_argument("--metadata", type=Path, default=Path("/run/dram-bw/current.json"),
                        help="live session metadata written by make server")
    parser.add_argument("--build", type=Path, default=Path(__file__).resolve().parents[1] / "build",
                        help="directory containing dram-bw-consume")
    parser.add_argument("--output", type=Path, required=True, help="new output directory; must not already exist")
    parser.add_argument("--samples", type=int, default=30, help="samples per phase, including discarded boundaries (at least 10)")
    parser.add_argument("--repeats", type=int, default=3, help="repetitions with perf enabled (at least 1)")
    parser.add_argument("--conditions", nargs="+", choices=("baseline", "node0", "node1", "both"),
                        default=["baseline", "node0", "node1", "both"],
                        help="baseline adds no load; other conditions run memory copies on the named NUMA nodes")
    parser.add_argument("--solo", action="store_true", help="also run once without perf for each condition")
    args = parser.parse_args()
    if args.samples < 10 or args.repeats < 1:
        parser.error("use at least 10 samples and one repetition")
    metadata = json.loads(args.metadata.read_text())
    if not metadata["reference"] or metadata["uid"] != os.getuid():
        parser.error("requires a live reference session permitting this user")

    def interrupted(signum, frame):
        raise KeyboardInterrupt

    signal.signal(signal.SIGTERM, interrupted)
    args.output.mkdir(parents=True, exist_ok=False)
    (args.output / "session.json").write_text(json.dumps(metadata, indent=2) + "\n")
    phases, workers, handles = [], [], []
    def stop_workers():
        for worker in workers:
            if worker.poll() is None:
                worker.terminate()
        for worker in workers:
            try:
                worker.wait(timeout=5)
            except subprocess.TimeoutExpired:
                worker.kill()
                worker.wait()
        workers.clear()
        for handle in handles:
            handle.close()
        handles.clear()

    try:
        schedule = [(False, 0, condition) for condition in args.conditions] if args.solo else []
        schedule += [(True, repeat, condition) for repeat in range(args.repeats) for condition in args.conditions]
        for reference, repeat, condition in schedule:
            name = f'{"parallel" if reference else "solo"}-{repeat}-{condition}'
            print("starting", name, flush=True)
            control(metadata, "enable" if reference else "disable")
            # Four physical cores spread across Rome CCDs per node. Each worker
            # first-touches two 256 MiB buffers after binding to its CPU.
            nodes = [0, 1] if condition == "both" else ([int(condition[-1])] if condition.startswith("node") else [])
            cpus = []
            for node in nodes:
                available = cpu_list(Path(f"/sys/devices/system/node/node{node}/cpulist").read_text().strip())
                physical = [cpu for cpu in available if cpu == min(cpu_list(Path(f"/sys/devices/system/cpu/cpu{cpu}/topology/thread_siblings_list").read_text().strip()))]
                cpus.extend(physical[1::max(1, len(physical) // 4)][:4])
            commands = []
            for cpu in cpus:
                if cpu not in os.sched_getaffinity(0):
                    raise RuntimeError(f"CPU {cpu} is outside this process's allowed affinity")
                command = ["taskset", "-c", str(cpu), "perf", "bench", "mem", "memcpy", "-f", "default", "-s", "256MB", "-l", "100000"]
                commands.append(command)
                handle = (args.output / f"{name}-cpu{cpu}.log").open("w")
                handles.append(handle)
                workers.append(subprocess.Popen(command, stdout=handle, stderr=subprocess.STDOUT))
            time.sleep(3)
            if any(worker.poll() is not None for worker in workers):
                raise RuntimeError("memory workload exited early; inspect worker logs")
            placement = {}
            for cpu, worker in zip(cpus, workers):
                placement[str(cpu)] = Path(f"/proc/{worker.pid}/numa_maps").read_text()
            (args.output / f"{name}-numa.json").write_text(json.dumps(placement, indent=2) + "\n")
            phase = {"name": name, "condition": condition, "repeat": repeat, "reference": reference,
                     "workload_commands": commands, "start_ns": time.monotonic_ns(),
                     "client_csv": str((args.output / f"{name}.csv").absolute())}
            with Path(phase["client_csv"]).open("w") as output, (args.output / f"{name}.stderr").open("w") as errors:
                subprocess.run([str(args.build / "dram-bw-consume"), metadata["socket"], str(args.samples)],
                               stdout=output, stderr=errors, check=True, timeout=args.samples * 3 + 15)
            phase["end_ns"] = time.monotonic_ns()
            if any(worker.poll() is not None for worker in workers):
                raise RuntimeError("memory workload ended during measurement")
            stop_workers()
            control(metadata, "disable")
            diagnostics = [json.loads(line) for line in (Path(metadata["session"]) / "diagnostics.jsonl").read_text().splitlines()]
            perf_text = Path(metadata["perf_output"]).read_text()
            result = summary(metadata, phase, diagnostics, perf_text)
            phases.append(result)
            (args.output / "results.json").write_text(json.dumps(phases, indent=2, allow_nan=False) + "\n")
            print(json.dumps({key: result[key] for key in ("name", "daemon_GBps", "daemon_valid", "daemon_invalid", "perf_GBps", "relative_difference_pct") if key in result}), flush=True)
    finally:
        stop_workers()
        control(metadata, "disable")
        for name in ("diagnostics.jsonl", "perf.csv", "perf.stderr"):
            shutil.copyfile(Path(metadata["session"]) / name, args.output / name)


if __name__ == "__main__":
    try:
        main()
    except KeyboardInterrupt:
        raise SystemExit("measurement interrupted; owned workloads stopped") from None
