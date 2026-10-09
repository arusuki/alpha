#!/usr/bin/env python3
"""Offline timing/window sensitivity analysis of compare_perf.py recordings.

Fractional-window integrals assume each reported interval has constant rate.
They are estimates, not reconstructed sub-second hardware measurements.
"""
import argparse
import csv
import json
from pathlib import Path
import statistics

from compare_perf import cpu_list


def quantile(values, fraction):
    values = sorted(values)
    point = (len(values) - 1) * fraction
    index = int(point)
    return values[index] + (values[min(index + 1, len(values) - 1)] - values[index]) * (point - index)


def describe(values):
    return dict(min=min(values), median=statistics.median(values),
                p95=quantile(values, .95), max=max(values))


def mean_over(records, start, end):
    total, covered = 0.0, 0.0
    for left, right, rate in records:
        overlap = max(0.0, min(right, end) - max(left, start))
        total += overlap * rate
        covered += overlap
    if abs(covered - (end - start)) > 1e-6:
        raise ValueError(f"window not fully covered: {covered} != {end - start}")
    return total / covered


def analyze(directory):
    metadata = json.loads((directory / "session.json").read_text())
    phases = json.loads((directory / "results.json").read_text())
    origin = metadata["perf_launch_before_ns"]
    uncertainty = (metadata["perf_ready_ns"] - origin) / 1e9
    midpoint = uncertainty / 2
    groups = {}
    with (directory / "perf.csv").open() as source:
        for row in csv.reader(source, delimiter=";"):
            if len(row) >= 7 and row[1].startswith("CPU"):
                groups.setdefault(float(row[0]), []).append(row)
    expected = {(f"CPU{cpu}", event) for cpu in cpu_list(metadata["cpumask"]) for event in metadata["events"]}
    perf, previous = [], 0.0
    for timestamp, rows in sorted(groups.items()):
        if (len(rows) == len(expected) and {(r[1], r[4]) for r in rows} == expected
                and all("<" not in r[2] and float(r[5]) > 0 for r in rows)):
            perf.append((previous, timestamp, sum(float(r[2]) * 64 / 1e9 for r in rows) / (timestamp - previous)))
        previous = timestamp
    results, all_offset, d_periods, p_periods, sequence_ids = [], [], [], [], set()
    for phase in phases:
        if not phase["reference"]:
            continue
        low = (phase["start_ns"] - origin) / 1e9 + 2
        high = (phase["end_ns"] - origin) / 1e9 - 2
        with (directory / Path(phase["client_csv"]).name).open() as source:
            rows = list(csv.DictReader(source))
        if any(int(r["flags"]) & 3 for r in rows):
            raise ValueError("requires real valid daemon samples")
        daemon = [((int(r["host_monotonic_ns"]) - int(r["interval_ns"]) - origin) / 1e9,
                   (int(r["host_monotonic_ns"]) - origin) / 1e9, float(r["total_GBps"])) for r in rows]
        chosen_d = [r for r in daemon if r[0] >= low and r[1] <= high]
        chosen_p = [r for r in perf if r[0] >= low and r[1] + uncertainty <= high]
        assert len(chosen_d) == phase["daemon_valid"]
        assert len(chosen_p) == phase["perf_valid"]
        sequence_ids.update(int(r["sequence"]) for r in rows)
        p_start, p_end = chosen_p[0][0], chosen_p[-1][1]
        d_start, d_end = chosen_d[0][0], chosen_d[-1][1]
        p_mean = mean_over(chosen_p, p_start, p_end)
        assert abs(p_mean - phase["perf_GBps"]) < 1e-8
        assert abs(mean_over(chosen_d, d_start, d_end) - phase["daemon_GBps"]) < 1e-8

        def difference(shift):
            return (mean_over(daemon, p_start + shift, p_end + shift) / p_mean - 1) * 100

        # Exact extrema under the piecewise-constant assumption occur at
        # interval breakpoints or endpoints of the allowed epoch range.
        shifts = [0.0, midpoint, uncertainty]
        for row in daemon:
            for edge in row[:2]:
                shifts.extend(s for s in (edge - p_start, edge - p_end) if 0 <= s <= uncertainty)
        aligned = [difference(s) for s in shifts]
        center_offsets, raw_errors, projected_errors = [], [], []
        for row in chosen_p:
            center = (row[0] + row[1]) / 2 + midpoint
            nearest = min(daemon, key=lambda d: abs((d[0] + d[1]) / 2 - center))
            center_offsets.append(((nearest[0] + nearest[1]) / 2 - center) * 1000)
            raw_errors.append(abs(nearest[2] / row[2] - 1) * 100)
            projected = mean_over(daemon, row[0] + midpoint, row[1] + midpoint)
            projected_errors.append(abs(projected / row[2] - 1) * 100)
        d_periods.extend((r[1] - r[0]) * 1000 for r in chosen_d)
        p_periods.extend((r[1] - r[0]) * 1000 for r in chosen_p)
        all_offset.extend(abs(x) for x in center_offsets)
        result = dict(name=phase["name"], condition=phase["condition"],
                      original_difference_pct=phase["relative_difference_pct"],
                      aligned_estimate_difference_pct=difference(midpoint),
                      aligned_epoch_range_pct=[min(aligned), max(aligned)],
                      alignment_change_pp=difference(midpoint) - phase["relative_difference_pct"],
                      aggregate_perf_start_later_ms=(p_start + midpoint - d_start) * 1000,
                      aggregate_perf_end_earlier_ms=(d_end - p_end - midpoint) * 1000,
                      aggregate_midpoint_offset_ms=((p_start + p_end - d_start - d_end) / 2 + midpoint) * 1000,
                      nearest_sample_center_offset_ms=describe(center_offsets),
                      nearest_sample_center_abs_offset_ms=describe([abs(x) for x in center_offsets]),
                      daemon_period_ms=describe([(r[1] - r[0]) * 1000 for r in chosen_d]),
                      perf_period_ms=describe([(r[1] - r[0]) * 1000 for r in chosen_p]),
                      unaligned_nearest_sample_abs_difference_pct=describe(raw_errors),
                      projected_sample_abs_difference_pct=describe(projected_errors),
                      shift_minus_500ms_difference_pct=difference(midpoint - .5),
                      shift_plus_500ms_difference_pct=difference(midpoint + .5))
        results.append(result)
    scans = []
    with (directory / "diagnostics.jsonl").open() as source:
        for line in source:
            row = json.loads(line)
            if row["type"] == "sample" and row["sequence"] in sequence_ids:
                scans.append(row["scan_ns"] / 1000)
    return dict(epoch_uncertainty_ms=uncertainty * 1000,
                epoch_midpoint_uncertainty_plus_minus_ms=midpoint * 1000,
                nearest_sample_center_abs_offset_ms=describe(all_offset),
                daemon_period_ms=describe(d_periods), perf_period_ms=describe(p_periods),
                parallel_daemon_scan_us=describe(scans),
                assumptions=["perf epoch lies between process launch and initial control ACK",
                             "perf timestamps precede counter scanning; per-counter perf read timestamps are unavailable",
                             "partial intervals use piecewise-constant rates; no sub-second traffic reconstruction",
                             "actual PMU multiplexing schedules are unavailable"], phases=results)


if __name__ == "__main__":
    parser = argparse.ArgumentParser(
        description=__doc__,
        epilog="No running daemon or PMU access needed. Writes DIRECTORY/timing/analysis.json "
               "(replaced on rerun) and prints a summary. "
               "Example: python3 scripts/analyze_timing.py results/perf-comparison",
    )
    parser.add_argument("directory", type=Path,
                        help="compare_perf.py output directory containing session.json, results.json, client CSV, perf.csv and diagnostics.jsonl")
    args = parser.parse_args()
    result = analyze(args.directory)
    output = args.directory / "timing"
    output.mkdir(exist_ok=True)
    (output / "analysis.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps({key: value for key, value in result.items() if key != "phases"}, indent=2))
    for row in result["phases"]:
        print(row["name"], "offset ms", round(row["nearest_sample_center_abs_offset_ms"]["median"], 1),
              "original %", round(row["original_difference_pct"], 4),
              "aligned estimate %", round(row["aligned_estimate_difference_pct"], 4),
              "epoch range %", [round(x, 4) for x in row["aligned_epoch_range_pct"]])
