#!/usr/bin/env python3
"""Portable local GGUF launcher. Standard library only; llama.cpp does inference."""
import argparse
import ctypes
import hashlib
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import sys
import tempfile
import urllib.parse
import urllib.request

MIB = 1024 ** 2
GIB = 1024 ** 3

def available_ram():
    try:
        if sys.platform.startswith("linux") or os.environ.get("TERMUX_VERSION"):
            for line in Path("/proc/meminfo").read_text().splitlines():
                if line.startswith("MemAvailable:"):
                    ram = int(line.split()[1]) * 1024
                    # Respect cgroup v2 limits on Linux containers when visible.
                    try:
                        limit = Path("/sys/fs/cgroup/memory.max").read_text().strip()
                        used = int(Path("/sys/fs/cgroup/memory.current").read_text())
                        if limit != "max":
                            ram = min(ram, max(0, int(limit) - used))
                    except (OSError, ValueError):
                        pass
                    return ram
        if sys.platform == "win32":
            class Status(ctypes.Structure):
                _fields_ = [("length", ctypes.c_ulong), ("load", ctypes.c_ulong)] + [
                    (name, ctypes.c_ulonglong) for name in
                    ("total", "available", "page_total", "page_available",
                     "virtual_total", "virtual_available", "extended")]
            status = Status()
            status.length = ctypes.sizeof(status)
            if ctypes.windll.kernel32.GlobalMemoryStatusEx(ctypes.byref(status)):
                return status.available
        if sys.platform == "darwin":
            output = subprocess.check_output(["vm_stat"], text=True)
            page = int(re.search(r"page size of (\d+)", output).group(1))
            free = sum(int(re.search(r"^" + key + r":\s+(\d+)", output, re.M).group(1))
                       for key in ("Pages free", "Pages inactive"))
            return free * page
    except (OSError, ValueError, AttributeError, subprocess.SubprocessError):
        pass
    return None

def profile(ram, context=None):
    # Reserve for runtime/KV cache/OS. Architecture-dependent; not a proof of fit.
    ctx = context or (512 if ram is None or ram < 3*GIB else
                      1024 if ram < 6*GIB else 2048 if ram < 12*GIB else 4096)
    reserve = max(768*MIB, ctx * MIB // 2)
    budget = max(0, int(ram * 0.85) - reserve) if ram is not None else None
    threads = max(1, min(8, (os.cpu_count() or 2)//2))
    return ctx, budget, threads

def hf_candidates(repo):
    if not re.fullmatch(r"[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+", repo):
        raise ValueError("Use a local GGUF, HTTPS URL, or Hugging Face owner/repo.")
    if any(x in (".", "..") for x in repo.split("/")):
        raise ValueError("Invalid repository name.")
    url = "https://huggingface.co/api/models/" + repo + "/tree/main?recursive=true"
    with urllib.request.urlopen(url, timeout=30) as response:
        items = json.loads(response.read(8*MIB + 1))
    result = []
    for item in items:
        name = item.get("path", "")
        if (item.get("type") != "file" or not name.lower().endswith(".gguf")
                or re.search(r"-\d{5}-of-\d{5}\.gguf$", name, re.I)
                or "mmproj" in name.lower()):
            continue
        size = item.get("size", 0)
        if isinstance(size, int) and size > 0:
            result.append((size, name, "https://huggingface.co/" + repo +
                           "/resolve/main/" + urllib.parse.quote(name, safe="/")))
    return result

def choose(candidates, budget, disk_mode=False):
    if not candidates:
        raise ValueError("No single-file GGUF found; provide a compatible GGUF URL or file.")
    # Prefer common >=4-bit variants when they fit; otherwise pick largest fitting.
    fitting = [x for x in candidates if budget is not None and x[0] <= budget]
    if fitting:
        preferred = [x for x in fitting if re.search(r"(?:Q[4568](?:_|\.|-)|F16|BF16)", x[1], re.I)]
        return max(preferred or fitting, key=lambda x: x[0])
    if disk_mode:
        return min(candidates, key=lambda x: x[0])
    raise ValueError("No model variant fits the estimated weight budget. Choose a smaller model or explicitly try --disk-mode.")

def verify_gguf(path, sha=None):
    with path.open("rb") as f:
        if f.read(4) != b"GGUF":
            raise ValueError("Not a GGUF file: " + str(path))
    if sha:
        h = hashlib.sha256()
        with path.open("rb") as f:
            for block in iter(lambda: f.read(MIB), b""):
                h.update(block)
        if h.hexdigest().lower() != sha.lower():
            raise ValueError("SHA-256 mismatch: " + str(path))

def download(url, root, budget, disk_mode, sha=None, expected=None):
    if urllib.parse.urlsplit(url).scheme != "https":
        raise ValueError("Only HTTPS model downloads are accepted.")
    key = hashlib.sha256(url.encode()).hexdigest()[:24]
    target = root / (key + ".gguf")
    if target.is_file():
        verify_gguf(target, sha)
        if expected is not None and target.stat().st_size != expected:
            raise ValueError("Cached model size differs; remove that cache entry to redownload.")
        return target
    with urllib.request.urlopen(url, timeout=60) as response:
        if urllib.parse.urlsplit(response.geturl()).scheme != "https":
            raise ValueError("Refusing insecure redirect.")
        size = int(response.headers.get("Content-Length") or 0) or expected
        if not disk_mode and (budget is None or size is None or size > budget):
            raise ValueError("Download size is unknown or above budget; choose a smaller model or --disk-mode.")
        free = shutil.disk_usage(root).free - 128*MIB
        limit = free if disk_mode else min(free, budget)
        if size and size > limit:
            raise ValueError("Not enough disk space or memory budget for model.")
        fd, name = tempfile.mkstemp(prefix=key, suffix=".part", dir=root)
        temp = Path(name)
        try:
            count = 0
            with os.fdopen(fd, "wb") as f:
                while True:
                    block = response.read(MIB)
                    if not block:
                        break
                    count += len(block)
                    if count > limit:
                        raise ValueError("Download exceeded disk or memory budget.")
                    f.write(block)
            if size is not None and count != size:
                raise ValueError("Incomplete model download.")
            if expected is not None and count != expected:
                raise ValueError("Model size changed; retry with a pinned URL.")
            verify_gguf(temp, sha)
            os.replace(temp, target)
        finally:
            temp.unlink(missing_ok=True)
    return target

def find_binary(name, root, explicit=None):
    if explicit:
        p = Path(explicit).expanduser().resolve()
        if not p.is_file():
            raise ValueError("Runtime binary not found: " + str(p))
        return str(p)
    override = os.environ.get("LLAMA_CPP_BIN" if name == "llama-cli" else "LLAMA_QUANTIZE_BIN")
    if override:
        return find_binary(name, root, override)
    found = shutil.which(name)
    if found:
        return found
    for folder in ("bin", "bin/Release", "bin/RelWithDebInfo"):
        p = root / "llama.cpp" / "build" / folder / (name + (".exe" if os.name == "nt" else ""))
        if p.is_file():
            return str(p)
    return None

def runtime(root, explicit, no_build):
    binary = find_binary("llama-cli", root, explicit)
    if binary:
        return binary
    if no_build:
        raise ValueError("llama-cli not installed; omit --no-build or supply --runtime.")
    if not all(shutil.which(x) for x in ("git", "cmake")):
        raise ValueError("Install git, CMake and a C++ compiler first (Termux: pkg install git cmake clang make).")
    checkout = root / "llama.cpp"
    if not checkout.exists():
        subprocess.run(["git", "clone", "--depth", "1", "https://github.com/ggml-org/llama.cpp", str(checkout)], check=True)
    if not (checkout / ".git").exists():
        raise ValueError("Existing runtime source is not a Git checkout.")
    subprocess.run(["cmake", "-S", str(checkout), "-B", str(checkout/"build"),
                    "-DCMAKE_BUILD_TYPE=Release", "-DBUILD_SHARED_LIBS=OFF",
                    "-DGGML_NATIVE=ON", "-DLLAMA_CURL=OFF"], check=True)
    subprocess.run(["cmake", "--build", str(checkout/"build"), "--config", "Release",
                    "--target", "llama-cli", "llama-quantize", "--parallel", "1"], check=True)
    binary = find_binary("llama-cli", root)
    if not binary:
        raise ValueError("Build finished but llama-cli was not found.")
    return binary

def quantize(model, root, level):
    binary = find_binary("llama-quantize", root)
    if not binary:
        raise ValueError("llama-quantize not installed.")
    fd, name = tempfile.mkstemp(prefix="quant-", suffix=".gguf", dir=root)
    os.close(fd)
    out = Path(name)
    try:
        subprocess.run([binary, str(model), str(out), level], check=True)
        verify_gguf(out)
        if out.stat().st_size >= model.stat().st_size:
            raise ValueError("Quantization did not reduce size; original preserved.")
        target = out.with_name(out.stem + "-" + level + ".gguf")
        os.replace(out, target)
        return target
    finally:
        out.unlink(missing_ok=True)


def runtime_features(cli):
    """Discover flags instead of assuming a particular llama.cpp version."""
    result = subprocess.run([cli, "--help"], stdout=subprocess.PIPE,
                            stderr=subprocess.STDOUT, text=True, timeout=20)
    return set(re.findall(r"(?<![\w-])--[a-z][a-z0-9-]*", result.stdout or ""))

def low_memory_flags(features, ctx, attempt=0):
    flags = []
    if "--load-mode" in features:
        flags += ["--load-mode", "mmap"]
    elif "--mmap" in features:
        flags += ["--mmap"]
    if "--lazy-mode" in features:
        flags += ["--lazy-mode", "on"]
    for flag in ("--no-repack", "--no-warmup"):
        if flag in features:
            flags.append(flag)
    if "--cache-ram" in features:
        flags += ["--cache-ram", "0"]
    if "--ctx-checkpoints" in features:
        flags += ["--ctx-checkpoints", "0"]
    batch = min(ctx, 128 if attempt == 0 else 32)
    if "--batch-size" in features:
        flags += ["--batch-size", str(batch)]
    if "--ubatch-size" in features:
        flags += ["--ubatch-size", str(min(batch, 32 if attempt == 0 else 8))]
    # K-cache quantization is optional; leave V unchanged for broad compatibility.
    if attempt and "--cache-type-k" in features:
        flags += ["--cache-type-k", "q8_0"]
    return flags

def disk_inference(cmd, cli, ctx, prompt, retries=True):
    features = runtime_features(cli)
    if not ({"--load-mode", "--mmap"} & features):
        raise ValueError("Runtime does not advertise mmap support; install a recent llama.cpp.")
    # Environment variables can otherwise override memory mapping or lock pages.
    env = {k:v for k,v in os.environ.items() if not k.startswith("LLAMA_ARG_")}
    for attempt, context in enumerate([ctx, max(128, ctx//2)] if retries and prompt is not None and ctx > 128 else [ctx]):
        current = list(cmd)
        current[current.index("-c")+1] = str(context)
        current += low_memory_flags(features, context, attempt)
        print(json.dumps({"disk_attempt": attempt+1, "context": context,
                          "lazy_tensors": "--lazy-mode" in features,
                          "runtime_flags": low_memory_flags(features, context, attempt)}),
              file=sys.stderr, flush=True)
        if prompt is None:
            return subprocess.call(current, env=env)
        # Preserve output as it occurs and spool stderr to disk, not RAM.
        with tempfile.TemporaryFile() as diagnostic:
            result = subprocess.run(current, env=env, stderr=diagnostic)
            diagnostic.seek(0, os.SEEK_END)
            diagnostic.seek(max(0, diagnostic.tell()-65536))
            tail = diagnostic.read().decode("utf-8", errors="replace")
            print(tail, file=sys.stderr, end="")
        status = result.returncode
        if status == 0:
            return 0
        oom = status in (-9, 137) or bool(re.search(
            r"out of memory|cannot allocate memory|bad_alloc|failed to allocate|not enough memory",
            tail, re.I))
        if not oom or attempt or context <= 128 or not retries:
            return status
        print("Possible memory exhaustion; retrying once with smaller context/batches. Partial output may repeat.", file=sys.stderr)
    return status


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("model", help="local GGUF, HTTPS GGUF URL, or HF owner/repo")
    parser.add_argument("--cache", type=Path, default=Path(os.environ.get("STRETCH_MODEL_CACHE", str(Path.home()/".cache"/"stretch-models"))))
    parser.add_argument("--runtime")
    parser.add_argument("--no-build", action="store_true")
    parser.add_argument("--disk-mode", action="store_true", help="allow oversize mmap experiment; may be slow or fail")
    parser.add_argument("--context", type=int)
    parser.add_argument("--no-retry", action="store_true", help="disable memory-pressure retry")
    parser.add_argument("--ram-mib", type=int, help="manual upper bound on available RAM")
    parser.add_argument("--sha256")
    parser.add_argument("--quantize", choices=["Q8_0","Q5_K_M","Q4_K_M","Q3_K_M","Q2_K"])
    parser.add_argument("--prompt")
    parser.add_argument("--tokens", type=int, default=128)
    parser.add_argument("--plan", action="store_true")
    args = parser.parse_args(argv)
    if (args.context is not None and args.context < 128) or args.tokens < 1 or (args.ram_mib is not None and args.ram_mib < 1):
        parser.error("context must be >=128; tokens and RAM must be positive")
    if args.sha256 and not re.fullmatch(r"[0-9a-fA-F]{64}", args.sha256):
        parser.error("SHA-256 must be 64 hexadecimal digits")
    try:
        ram = available_ram()
        if args.ram_mib is not None:
            manual = args.ram_mib*MIB
            ram = min(ram, manual) if ram is not None else manual
        ctx, budget, threads = profile(ram, args.context)
        root = args.cache.expanduser().resolve()
        local = Path(args.model).expanduser()
        expected = None
        url = None
        if local.is_file():
            local = local.resolve()
            verify_gguf(local, args.sha256)
            expected = local.stat().st_size
        elif args.model.startswith("https://"):
            url = args.model
        elif args.model.endswith(".gguf"):
            raise ValueError("Local GGUF not found.")
        else:
            expected, _, url = choose(hf_candidates(args.model), budget, args.disk_mode)
        print(json.dumps({"platform": platform.platform(), "arch": platform.machine(),
                          "available_ram_mib": None if ram is None else ram//MIB,
                          "estimated_weight_budget_mib": None if budget is None else budget//MIB,
                          "context": ctx, "threads": threads, "model": url or str(local),
                          "model_bytes": expected, "disk_mode": args.disk_mode}), flush=True)
        if args.plan:
            return 0
        root.mkdir(parents=True, exist_ok=True)
        if url:
            local = download(url, root, budget, args.disk_mode or bool(args.quantize), args.sha256, expected)
        if not args.quantize and not args.disk_mode and (budget is None or local.stat().st_size > budget):
            raise ValueError("Model exceeds estimated budget; use smaller GGUF, --quantize, or explicit --disk-mode.")
        cli = runtime(root, args.runtime, args.no_build)
        if args.quantize:
            local = quantize(local, root, args.quantize)
            print("Quantized model:", local, flush=True)
        if not args.disk_mode and (budget is None or local.stat().st_size > budget):
            raise ValueError("Quantized model still exceeds estimated budget; use a smaller model.")
        cmd = [cli, "-m", str(local), "-c", str(ctx), "-t", str(threads),
               "-ngl", "0", "-n", str(args.tokens)]
        # llama.cpp enables mmap by default; no mlock or artificial swap changes.
        if args.prompt is not None:
            cmd += ["-p", args.prompt, "--no-conversation"]
        if args.disk_mode:
            return disk_inference(cmd, cli, ctx, args.prompt, not args.no_retry)
        return subprocess.call(cmd)
    except (ValueError, OSError, subprocess.SubprocessError) as error:
        print("stretch-model:", error, file=sys.stderr)
        return 1

if __name__ == "__main__":
    sys.exit(main())
