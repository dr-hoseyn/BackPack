"""Check that the deployment gate rejects misleading CPU wins."""
import importlib.util
import pathlib
import tempfile
import unittest

path = pathlib.Path(__file__).with_name("compare-wssmux-cpu-quality.py")
spec = importlib.util.spec_from_file_location("quality", path)
quality = importlib.util.module_from_spec(spec)
spec.loader.exec_module(quality)


class QualityGateTests(unittest.TestCase):
    def samples(self, root, cpu=9, p99=1, payload=536870912, goodput=599):
        for index in range(1, 7):
            for kind in ("baseline", "candidate"):
                candidate = kind == "candidate"
                (root / f"paced-{kind}-{index}.txt").write_text(
                    "RESOURCE_IDLE connections=1000 heap_delta_bytes=100000000\n"
                    f"RESOURCE_MIXED app_bytes={payload if candidate else 536870912} "
                    f"echo_mbps={goodput if candidate else 599} "
                    f"cpu_seconds_per_gib={cpu if candidate else 10} "
                    f"probe_p99_ms={p99 if candidate else 1}\n"
                    "RESOURCE_PEAK sampled_peak_rss_bytes=200000000\n"
                    "--- PASS: TestWSMuxResourceProfile (8.0s)\n")

    def test_verified_equal_load_with_repeatable_cpu_gain_passes(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.samples(root)
            self.assertTrue(quality.compare(root)["accepted"])

    def test_cpu_gain_does_not_excuse_tail_latency_or_lower_goodput(self):
        for change in ({"p99": 3}, {"goodput": 580}):
            with self.subTest(change=change), tempfile.TemporaryDirectory() as directory:
                root = pathlib.Path(directory)
                self.samples(root, cpu=8, **change)
                self.assertFalse(quality.compare(root)["accepted"])

    def test_less_work_and_partial_measurements_are_refused(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.samples(root, payload=268435456)
            with self.assertRaisesRegex(ValueError, "unequal app_bytes"):
                quality.compare(root)
            self.samples(root)
            (root / "paced-candidate-3.txt").write_text("RESOURCE_IDLE connections=1000\n")
            with self.assertRaisesRegex(ValueError, "incomplete"):
                quality.compare(root)

    def test_noise_sized_cpu_change_does_not_pass(self):
        with tempfile.TemporaryDirectory() as directory:
            root = pathlib.Path(directory)
            self.samples(root, cpu=9.9)
            self.assertFalse(quality.compare(root)["accepted"])


if __name__ == "__main__":
    unittest.main()
