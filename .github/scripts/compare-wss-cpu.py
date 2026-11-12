"""Compare verified WSS/WSSMux workloads in alternating independent processes."""
import json
import os
import pathlib
import re
import statistics
import sys

root = pathlib.Path(sys.argv[1])
reports = {}
for kind in ("baseline", "candidate"):
    samples = []
    for sample in range(1, 6):
        path = root / f"wss-cpu-{kind}-{sample}.txt"
        text = path.read_text()
        if "--- PASS: TestWSMuxResourceProfile" not in text:
            raise SystemExit(f"Incomplete verified measurement: {path}")
        row = re.search(r"RESOURCE_MIXED (.+)", text)
        if row is None:
            raise SystemExit(f"Missing mixed workload: {path}")
        values = {key: float(value) for key, value in re.findall(r"(\w+)=(-?[\d.]+)", row[1])}
        for key in ("app_bytes", "echo_mbps", "cpu_seconds_per_gib", "probe_p99_ms"):
            if key not in values or values[key] <= 0:
                raise SystemExit(f"Invalid {key}: {path}")
        samples.append(values)
    reports[kind] = {"samples": samples, "median": {
        key: statistics.median(sample[key] for sample in samples) for key in samples[0]
    }}

base, candidate = (reports[kind]["median"] for kind in ("baseline", "candidate"))
if len({sample["app_bytes"] for report in reports.values() for sample in report["samples"]}) != 1:
    raise SystemExit("Compared workloads transferred different verified payload volumes")
checks = {
    "cpu_per_gib_within_20_percent": candidate["cpu_seconds_per_gib"] <= base["cpu_seconds_per_gib"] * 1.20,
    "verified_goodput_at_least_85_percent": candidate["echo_mbps"] >= base["echo_mbps"] * .85,
    "fresh_user_p99_within_runner_tolerance": candidate["probe_p99_ms"] <= base["probe_p99_ms"] * 1.5 + 2,
}
result = {
    "scope": "combined client/server/test process on loopback; 64 idle users, eight verified bulk workers and fresh-user latency probes; five alternating independent samples",
    "transport": os.environ["BP_RESOURCE_TRANSPORT"],
    "baseline_sha": os.environ["BP_RESOURCE_BASELINE_SHA"],
    "candidate_sha": os.environ["GITHUB_SHA"],
    "reports": reports,
    "checks": checks,
    "cpu_per_gib_change_percent": 100 * (candidate["cpu_seconds_per_gib"] / base["cpu_seconds_per_gib"] - 1),
    "verified_goodput_change_percent": 100 * (candidate["echo_mbps"] / base["echo_mbps"] - 1),
}
print(json.dumps(result, indent=2))
if not all(checks.values()):
    raise SystemExit("CPU/quality comparison failed; inspect the preserved independent samples")
