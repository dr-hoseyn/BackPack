"""Keep the opt-in pool regression budgets distinct from historical gates."""
import json
import os
import pathlib
import subprocess
import sys
import tempfile
import unittest

SCRIPT = pathlib.Path(__file__).with_name("compare-wssmux-resources.py")


class ResourceComparisonTests(unittest.TestCase):
    def compare(self, *, strict=True, mode="resource", goodput=1000, cpu=10, heap=1000):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            count = 3 if mode.startswith("delayed") else 5
            for kind in ("baseline", "candidate"):
                for sample in range(1, count + 1):
                    c, g, h = (10, 1000, 1000) if kind == "baseline" else (cpu, goodput, heap)
                    (root / f"{mode}-{kind}-{sample}.txt").write_text(
                        f"RESOURCE_IDLE heap_delta_bytes={h}\n"
                        f"RESOURCE_MIXED cpu_seconds_per_gib={c} echo_mbps={g} probe_p99_ms=10\n"
                        "--- PASS: TestWSMuxResourceProfile\n"
                    )
            env = dict(os.environ, BP_WSSMUX_STRICT_POOL_COMPARISON="1" if strict else "")
            result = subprocess.run([sys.executable, str(SCRIPT), str(root), mode], env=env, capture_output=True, text=True)
            return result.returncode, json.loads(result.stdout)

    def test_equal_current_baseline_passes_without_claiming_heap_reduction(self):
        code, report = self.compare()
        self.assertEqual(code, 0)
        self.assertEqual(report["comparison_policy"], "strict_pool_regression")
        self.assertTrue(all(report["checks"].values()))

    def test_strict_budgets_reject_goodput_cpu_and_heap_regressions(self):
        for change in ({"goodput": 979}, {"cpu": 10.51}, {"heap": 1051}):
            with self.subTest(change=change):
                code, report = self.compare(**change)
                self.assertNotEqual(code, 0)
                self.assertFalse(all(report["checks"].values()))

    def test_original_historical_heap_requirement_is_preserved(self):
        code, report = self.compare(strict=False)
        self.assertNotEqual(code, 0)
        self.assertFalse(report["checks"]["idle_heap_improves_at_least_15_percent"])
        code, _ = self.compare(strict=False, heap=850)
        self.assertEqual(code, 0)

    def test_lossy_mode_also_uses_strict_regression_budgets(self):
        code, _ = self.compare(mode="delayed-auto1")
        self.assertEqual(code, 0)
        code, _ = self.compare(mode="delayed-pinned2", goodput=979)
        self.assertNotEqual(code, 0)


if __name__ == "__main__":
    unittest.main()
