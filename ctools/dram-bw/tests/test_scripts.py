"""Keep perf summary and offline timing analysis consistent on CPU ranges."""
import csv
import json
from pathlib import Path
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parents[1] / "scripts"))
from analyze_timing import analyze
from compare_perf import summary


class PerfAnalysisTest(unittest.TestCase):
    def test_cpu_range_recording(self):
        with tempfile.TemporaryDirectory(prefix="dbw-analysis-") as temporary:
            directory = Path(temporary)
            origin = 1_000_000_000
            event = "dram_channel_data_controller_0"
            metadata = {
                "cpumask": "0-1,4", "events": [event],
                "perf_launch_before_ns": origin, "perf_ready_ns": origin,
            }
            phase = {
                "name": "parallel-0-both", "condition": "both", "reference": True,
                "start_ns": origin, "end_ns": origin + 12_000_000_000,
                "client_csv": str(directory / "client.csv"),
            }
            diagnostics, perf_rows = [], []
            with (directory / "client.csv").open("w") as output:
                writer = csv.writer(output)
                writer.writerow(["sequence", "host_monotonic_ns", "interval_ns", "total_GBps", "flags"])
                for sequence in range(1, 13):
                    writer.writerow([sequence, origin + sequence * 1_000_000_000, 1_000_000_000, 3, 16])
                    diagnostics.append({"type": "sample", "sequence": sequence, "scan_ns": 1000})
                    for cpu in (0, 1, 4):
                        perf_rows.append(f"{sequence};CPU{cpu};15625000;;{event};1000000000;100.0")
                        diagnostics.append({
                            "type": "counter", "sequence": sequence, "cpu": cpu, "flags": 0,
                            "delta": [15_625_000, 1_000_000_000, 1_000_000_000],
                            "bytes_per_count": 64, "running_fraction": 1,
                        })
            perf_text = "\n".join(perf_rows) + "\n"
            result = summary(metadata, phase, diagnostics, perf_text)
            self.assertEqual(result["daemon_valid"], 8)
            self.assertEqual(result["perf_valid"], 8)
            self.assertEqual(result["perf_GBps"], 3)
            self.assertEqual(result["relative_difference_pct"], 0)
            (directory / "session.json").write_text(json.dumps(metadata))
            (directory / "results.json").write_text(json.dumps([result]))
            (directory / "perf.csv").write_text(perf_text)
            (directory / "diagnostics.jsonl").write_text("\n".join(map(json.dumps, diagnostics)) + "\n")
            analysis = analyze(directory)
            self.assertEqual(len(analysis["phases"]), 1)
            self.assertEqual(analysis["phases"][0]["aligned_estimate_difference_pct"], 0)
            self.assertEqual(analysis["parallel_daemon_scan_us"]["max"], 1)


if __name__ == "__main__":
    unittest.main()
