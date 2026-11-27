import hashlib
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch
import stretch_model as sm

class DeviceTests(unittest.TestCase):
    def test_low_ram_context(self):
        ctx, budget, threads = sm.profile(2*sm.GIB)
        self.assertEqual(ctx, 512)
        self.assertLess(budget, 2*sm.GIB)
        self.assertGreater(threads, 0)
    def test_no_ram_does_not_claim_fit(self):
        self.assertIsNone(sm.profile(None)[1])
    def test_fit_and_explicit_disk_mode(self):
        files = [(100, "model.Q4_K_M.gguf", "a"), (200, "model.Q8_0.gguf", "b")]
        self.assertEqual(sm.choose(files, 150)[2], "a")
        with self.assertRaises(ValueError):
            sm.choose(files, 50)
        self.assertEqual(sm.choose(files, 50, True)[2], "a")
    def test_file_integrity(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)/"model.gguf"
            p.write_bytes(b"GGUFtest")
            sm.verify_gguf(p, hashlib.sha256(p.read_bytes()).hexdigest())
            with self.assertRaises(ValueError):
                sm.verify_gguf(p, "0"*64)
            p.write_bytes(b"HTML")
            with self.assertRaises(ValueError):
                sm.verify_gguf(p)
    def test_launch_constructs_cpu_command_and_propagates_status(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)/"test.gguf"
            p.write_bytes(b"GGUFtest")
            with patch.object(sm, "available_ram", return_value=4*sm.GIB), \
                 patch.object(sm, "runtime", return_value="mock-cli"), \
                 patch.object(sm.subprocess, "call", return_value=7) as call:
                self.assertEqual(sm.main([str(p), "--cache", d, "--prompt", "hello"]), 7)
                cmd = call.call_args.args[0]
                self.assertEqual(cmd[:3], ["mock-cli", "-m", str(p.resolve())])
                self.assertEqual(cmd[cmd.index("-ngl")+1], "0")
    def test_plan_never_builds_or_launches(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)/"test.gguf"
            p.write_bytes(b"GGUFtest")
            with patch.object(sm, "runtime") as build:
                self.assertEqual(sm.main([str(p), "--plan"]), 0)
                build.assert_not_called()
    def test_refuse_oversize_before_build(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d)/"test.gguf"
            p.write_bytes(b"GGUFtest")
            with patch.object(sm, "available_ram", return_value=sm.MIB), \
                 patch.object(sm, "runtime") as build:
                self.assertEqual(sm.main([str(p), "--cache", d]), 1)
                build.assert_not_called()


    def test_lazy_disk_flags_and_older_runtime(self):
        f = {"--load-mode", "--lazy-mode", "--no-repack", "--no-warmup",
             "--batch-size", "--ubatch-size", "--cache-ram", "--cache-type-k"}
        flags = sm.low_memory_flags(f, 512, 1)
        self.assertEqual(flags[flags.index("--load-mode")+1], "mmap")
        self.assertEqual(flags[flags.index("--lazy-mode")+1], "on")
        self.assertEqual(flags[flags.index("--ubatch-size")+1], "8")
        self.assertEqual(flags[flags.index("--cache-ram")+1], "0")
        self.assertEqual(sm.low_memory_flags({"--mmap"}, 512), ["--mmap"])

    def test_disk_retry_only_on_memory_failure(self):
        import subprocess
        from unittest.mock import Mock
        cmd = ["cli", "-m", "example", "-c", "512"]
        def simulated(current, **kwargs):
            if current[current.index("-c")+1] == "512":
                kwargs["stderr"].write(b"cannot allocate memory")
                return subprocess.CompletedProcess(current, 1)
            return subprocess.CompletedProcess(current, 0)
        with patch.object(sm, "runtime_features", return_value={"--mmap"}), \
             patch.object(sm.subprocess, "run", side_effect=simulated) as run:
            self.assertEqual(sm.disk_inference(cmd, "cli", 512, "hello"), 0)
            self.assertEqual(run.call_count, 2)
        with patch.object(sm, "runtime_features", return_value={"--mmap"}), \
             patch.object(sm.subprocess, "run", return_value=subprocess.CompletedProcess(cmd, 2)) as run:
            self.assertEqual(sm.disk_inference(cmd, "cli", 512, "hello"), 2)
            self.assertEqual(run.call_count, 1)

if __name__ == "__main__":
    unittest.main()
