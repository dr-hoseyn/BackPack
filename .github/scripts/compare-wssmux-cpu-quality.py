"""Reject CPU candidates unless equal-load measurements also preserve quality.

Inputs are independent, alternating runs of the exact same e2e binary workload.
The baseline is the same source built with -pgo=off, not an older wire format.
"""
import json
import math
import pathlib
import random
import re
import statistics
import sys


def measurement(path):
    text = path.read_text()
    if "--- PASS: TestWSMuxResourceProfile" not in text:
        raise ValueError(f"incomplete or failed resource run: {path.name}")
    values = {}
    for tag in ("RESOURCE_IDLE", "RESOURCE_MIXED", "RESOURCE_PEAK"):
        match = re.search(rf"{tag} (.+)", text)
        if match is None:
            raise ValueError(f"missing {tag}: {path.name}")
        values.update({k: float(v) for k, v in re.findall(r"(\w+)=(-?[\d.]+)", match[1])})
    if not all(math.isfinite(v) and v >= 0 for v in values.values()):
        raise ValueError(f"invalid resource sample: {path.name}")
    required = {"connections", "app_bytes", "cpu_seconds_per_gib", "echo_mbps", "probe_p99_ms", "sampled_peak_rss_bytes"}
    if not required.issubset(values):
        raise ValueError(f"missing resource fields: {path.name}")
    if values.get("cpu_seconds_per_gib", 0) <= 0 or values.get("app_bytes", 0) <= 0:
        raise ValueError(f"empty resource sample: {path.name}")
    return values


def compare(root, prefix="paced", count=6):
    if count < (3 if prefix.startswith("delayed") else 6):
        raise ValueError("insufficient independent paired runs")
    runs = {kind: [measurement(root / f"{prefix}-{kind}-{i}.txt")
                   for i in range(1, count + 1)] for kind in ("baseline", "candidate")}
    for left, right in zip(runs["baseline"], runs["candidate"]):
        for key in ("connections", "app_bytes"):
            if left[key] != right[key]:
                raise ValueError(f"unequal {key} between paired workloads")
    medians = {kind: {k: statistics.median(row[k] for row in rows) for k in rows[0]}
               for kind, rows in runs.items()}
    base, candidate = medians["baseline"], medians["candidate"]
    ratios = [b["cpu_seconds_per_gib"] / a["cpu_seconds_per_gib"]
              for a, b in zip(runs["baseline"], runs["candidate"])]
    rng = random.Random(20261030)
    boot = sorted(statistics.median(rng.choices(ratios, k=len(ratios))) for _ in range(10000))
    upper = boot[9749]
    delayed = prefix.startswith("delayed")
    # Tolerances describe measurement resolution/noise, not an advertised gain.
    # A candidate is never accepted merely for delivering fewer bytes.
    checks = {
        "equal_verified_workload": True,
        "goodput_preserved": candidate["echo_mbps"] >= base["echo_mbps"] * .995,
        "tail_latency_preserved": candidate["probe_p99_ms"] <= base["probe_p99_ms"] +
                                  max(10 if delayed else .25, base["probe_p99_ms"] * .05),
        "sampled_rss_preserved": candidate["sampled_peak_rss_bytes"] <= base["sampled_peak_rss_bytes"] * 1.02 + 8 * 1024**2,
    }
    if not delayed:
        checks.update({
            "paced_goodput_at_least_594_mbps": candidate["echo_mbps"] >= 594,
            "cpu_median_improves_at_least_3_percent": statistics.median(ratios) <= .97,
            "paired_bootstrap_upper_below_one": upper < 1,
        })
    return {"scope": "same source and workload; -pgo=off versus explicit production CPU profile; combined client/server/echo",
            "prefix": prefix, "runs": runs, "medians": medians, "cpu_paired_ratios": ratios,
            "cpu_paired_median_change_percent": 100 * (statistics.median(ratios) - 1),
            "cpu_bootstrap_median_ratio_upper_97_5_percent": upper,
            "checks": checks, "accepted": all(checks.values())}


if __name__ == "__main__":
    result = compare(pathlib.Path(sys.argv[1]), sys.argv[2] if len(sys.argv) > 2 else "paced",
                     int(sys.argv[3]) if len(sys.argv) > 3 else 6)
    print(json.dumps(result, indent=2))
    if not result["accepted"]:
        raise SystemExit("Candidate rejected: CPU savings or quality are not established; do not deploy")
