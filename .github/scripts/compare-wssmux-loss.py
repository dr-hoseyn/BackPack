"""A focused diagnostic; the required resource workflow retains its own gates."""
import json
import os
import pathlib
import re
import statistics
import sys

root = pathlib.Path(sys.argv[1])
reports = {}
for kind in ("baseline", "previous", "candidate"):
    samples = []
    for sample in range(1, 6):
        path = root / f"loss-{kind}-{sample}.txt"
        text = path.read_text()
        if "--- PASS: TestWSMuxResourceProfile" not in text:
            raise SystemExit(f"Incomplete sample: {path}")
        values = {}
        for tag in ("RESOURCE_IDLE", "RESOURCE_MIXED"):
            line = re.search(rf"{tag} (.+)", text)
            if line is None:
                raise SystemExit(f"Missing {tag}: {path}")
            values.update({k: float(v) for k, v in re.findall(r"(\w+)=(-?[\d.]+)", line[1])})
        samples.append(values)
    reports[kind] = {
        "sha": (root / f"loss-{kind}-sha.txt").read_text().strip(),
        "samples": samples,
        "median": {key: statistics.median(s[key] for s in samples) for key in samples[0]},
    }

base, previous, candidate = (reports[k]["median"] for k in ("baseline", "previous", "candidate"))
checks = {
    # A stricter diagnostic target than the required suite's 15% tolerance.
    "goodput_at_least_98_percent_of_buffered_baseline": candidate["echo_mbps"] >= base["echo_mbps"] * .98,
    "cpu_per_gib_within_20_percent": candidate["cpu_seconds_per_gib"] <= base["cpu_seconds_per_gib"] * 1.20,
    "probe_p99_within_existing_runner_tolerance": candidate["probe_p99_ms"] <= base["probe_p99_ms"] * 1.5 + 2,
    "idle_heap_reduction_at_least_20_percent": candidate["heap_delta_bytes"] <= base["heap_delta_bytes"] * .80,
}
print(json.dumps({
    "scope": f"{os.environ.get('MUX_MODE')}: same-runner 80ms RTT, 0.1% loss, 200 Mbit/s; five paired seeds 42-46; eight workers, eight 4 MiB round trips each; independent network namespaces",
    "reports": reports,
    "checks": checks,
    "goodput_change_vs_buffered_percent": 100 * (candidate["echo_mbps"] / base["echo_mbps"] - 1),
    "goodput_change_vs_previous_percent": 100 * (candidate["echo_mbps"] / previous["echo_mbps"] - 1),
}, indent=2))
if not all(checks.values()):
    raise SystemExit("Loss diagnostic target missed; inspect all samples before drawing conclusions")
