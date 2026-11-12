"""Check identical byte-verified short-connection workloads, including latency."""
import json
import math
import os
import pathlib
import re
import statistics
import sys

root = pathlib.Path(sys.argv[1])
reports = {}
required = ("connections", "app_bytes", "elapsed_seconds", "cpu_seconds", "cpu_us_per_connection", "p99_ms")
for kind in ("baseline", "candidate"):
    samples = []
    for index in range(1, 6):
        path = root / f"wss-churn-{kind}-{index}.txt"
        text = path.read_text()
        row = re.search(r"CHURN (.+)", text)
        if row is None or "--- PASS: TestWSMuxConnectionChurnProfile" not in text:
            raise SystemExit(f"Incomplete verified churn measurement: {path}")
        values = {key: float(value) for key, value in re.findall(r"(\w+)=(-?[\d.]+)", row[1])}
        if any(key not in values or not math.isfinite(values[key]) or values[key] <= 0 for key in required):
            raise SystemExit(f"Invalid churn measurement: {path}")
        values["connections_per_second"] = values["connections"] / values["elapsed_seconds"]
        samples.append(values)
    reports[kind] = {"samples": samples, "median": {
        key: statistics.median(sample[key] for sample in samples) for key in samples[0]
    }}

for key in ("connections", "app_bytes"):
    if len({sample[key] for report in reports.values() for sample in report["samples"]}) != 1:
        raise SystemExit(f"Compared churn workloads differ in {key}")

base, candidate = (reports[kind]["median"] for kind in ("baseline", "candidate"))
checks = {
    "cpu_per_connection_within_20_percent": candidate["cpu_us_per_connection"] <= base["cpu_us_per_connection"] * 1.20,
    "connections_per_second_at_least_85_percent": candidate["connections_per_second"] >= base["connections_per_second"] * .85,
    "p99_within_runner_tolerance": candidate["p99_ms"] <= base["p99_ms"] * 1.5 + 2,
}
print(json.dumps({
    "scope": "combined loopback peers and driver; 4096 byte-verified 512-byte replies with 16 concurrent workers; five alternating processes",
    "transport": os.environ.get("BP_RESOURCE_TRANSPORT", "wssmux"),
    "baseline_sha": os.environ.get("BP_RESOURCE_BASELINE_SHA"),
    "candidate_sha": os.environ.get("GITHUB_SHA"),
    "reports": reports,
    "checks": checks,
    "cpu_per_connection_change_percent": 100 * (candidate["cpu_us_per_connection"] / base["cpu_us_per_connection"] - 1),
    "connection_rate_change_percent": 100 * (candidate["connections_per_second"] / base["connections_per_second"] - 1),
}, indent=2))
if not all(checks.values()):
    raise SystemExit("Churn CPU/quality comparison failed; inspect the preserved paired samples")
