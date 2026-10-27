"""Compare the same verified workload in independent processes on one runner."""
import json
import pathlib
import re
import statistics
import sys

root = pathlib.Path(sys.argv[1])
mode = sys.argv[2] if len(sys.argv) > 2 else "resource"
delayed = mode.startswith("delayed")
prefix, count = (mode, 3) if delayed else ("resource", 5)
reports = {}
for kind in ("baseline", "candidate"):
    samples = []
    for path in sorted(root.glob(f"{prefix}-{kind}-*.txt")):
        if not re.fullmatch(rf"{prefix}-{kind}-\d+\.txt", path.name):
            continue
        text = path.read_text()
        if "--- PASS: TestWSMuxResourceProfile" not in text:
            raise SystemExit(f"Incomplete measurement: {path}")
        values = {}
        for tag in ("RESOURCE_IDLE", "RESOURCE_MIXED"):
            line = re.search(rf"{tag} (.+)", text)
            if line is None:
                raise SystemExit(f"Missing {tag}: {path}")
            values.update({k: float(v) for k, v in re.findall(r"(\w+)=(-?[\d.]+)", line[1])})
        samples.append(values)
    if len(samples) != count:
        raise SystemExit(f"Expected {count} independent {kind} samples")
    reports[kind] = {"samples": samples, "median": {
        key: statistics.median(s[key] for s in samples) for key in samples[0]
    }}

base, candidate = (reports[k]["median"] for k in ("baseline", "candidate"))
checks = {
    "cpu_per_gib_within_20_percent": candidate["cpu_seconds_per_gib"] <= base["cpu_seconds_per_gib"] * 1.20,
    "bulk_goodput_at_least_85_percent": candidate["echo_mbps"] >= base["echo_mbps"] * .85,
    "probe_p99_within_runner_tolerance": candidate["probe_p99_ms"] <= base["probe_p99_ms"] * 1.5 + 2,
}
if not delayed:
    checks["idle_heap_improves_at_least_15_percent"] = candidate["heap_delta_bytes"] <= base["heap_delta_bytes"] * .85
result = {"scope": "combined client/server process; 1000 idle streams plus eight verified bulk transfers and fresh-user probes on loopback",
          "baseline": "v1.16.0", "reports": reports, "checks": checks,
          "idle_heap_reduction_percent": 100 * (1 - candidate["heap_delta_bytes"] / base["heap_delta_bytes"]),
          "cpu_per_gib_change_percent": 100 * (candidate["cpu_seconds_per_gib"] / base["cpu_seconds_per_gib"] - 1)}
if delayed:
    result["scope"] = f"{mode}: combined client/server process; 16 idle streams, eight bulk workers and fresh-user probes; receiver-ingress netem: 40ms each direction, 0.1% loss, 200mbit, MTU1500, TSO/GSO/GRO off, seed42"
print(json.dumps(result, indent=2))
if not all(checks.values()):
    raise SystemExit("Resource/quality comparison failed; inspect the preserved samples")
