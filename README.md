# Stretch — 1000x-ing every spec, honestly

Four small apps that squeeze more out of the hardware you already have.
Keyless, offline, no services, no accounts — static binaries that run anywhere.

| App | What it stretches | The honest deal |
|---|---|---|
| **stretchstore** | Storage | 1000GB thin volume on 1GB real. Zeros/repeats collapse; random is 1:1 |
| **stretchmem** | RAM | Userspace zram+KSM: per-page zstd + cross-segment dedup, hard cap |
| **stretchcpu** | Compute | Content-addressed memoization: 1000 identical jobs → 1 execution |
| **stretchnet** | Bandwidth | Download once, serve N times locally; dedupes identical bodies |

**The physics, up front:** none of these create resources. They refuse to
store, compute, or transfer the same bytes twice. N bytes of physical storage
hold at most N bytes of incompressible information (pigeonhole principle).
Encrypted, video, already-compressed, or random data is ~1:1 everywhere —
and every app below proves it with an adversarial test instead of hiding it.

## Install (one line)

```bash
curl -fsSL https://raw.githubusercontent.com/jaykk99/stretch/main/install.sh | sh
```

`install.sh` detects your OS/arch, downloads the matching prebuilt tarball
from this repo's GitHub releases, verifies its SHA-256 checksum against
`SHA256SUMS.txt`, installs the four binaries to `~/.local/bin` (or
`$PREFIX/bin` on Termux), and prints the per-OS notes. Mirroring the release
files elsewhere works too — point the script at the mirror:

```bash
curl -fsSL $STRETCH_RELEASE_BASE/install.sh | STRETCH_RELEASE_BASE=$STRETCH_RELEASE_BASE sh
```

No release download wanted? Build from source (needs the Go toolchain, no
network at runtime, ever):

```bash
git clone https://github.com/jaykk99/stretch.git && cd stretch
./install.sh --from-source        # builds the four binaries, installs to ~/.local/bin (or $PREFIX/bin on Termux)
# or manually:
go build -o stretchstore ./cmd/stretchstore
go build -o stretchmem   ./cmd/stretchmem
go build -o stretchcpu   ./cmd/stretchcpu
go build -o stretchnet   ./cmd/stretchnet
```

### Per-OS behavior

| OS / device | FUSE `mount` | Fallback | Notes |
|---|---|---|---|
| Linux x86_64 / arm64 | ✅ if `/dev/fuse` exists | `serve` | `sudo apt install fuse3` on Debian/Ubuntu/Chromebook |
| macOS (Intel + Apple Silicon) | ✅ with macFUSE | `serve` | without macFUSE, `mount` explains and points at `serve` |
| Windows x86_64 | ❌ (no FUSE backend) | `serve` | virtual `disk.img` over HTTP/WebDAV on localhost |
| Android arm64 (Termux) | ❌ without root | `serve` | open `http://127.0.0.1:8080` on the device |

`stretchmem`, `stretchcpu`, and `stretchnet` work identically on every OS —
they never needed FUSE. Only `stretchstore mount` is FUSE-dependent, and it
degrades gracefully: it never crashes, it tells you exactly what to run instead.

### Chromebook (Crostini Linux)

```bash
# in the Linux terminal:
sudo apt update && sudo apt install -y fuse3   # for kernel mounts (optional)
curl -fsSL https://raw.githubusercontent.com/jaykk99/stretch/main/install.sh | sh
~/.local/bin/stretchstore create --logical 1000GB --physical 1GB --dir ~/vol
~/.local/bin/stretchstore mount --dir ~/vol --mp ~/mnt/stretch   # needs fuse3
# ...or without fuse3:
~/.local/bin/stretchstore serve --dir ~/vol   # browse at http://127.0.0.1:8080
```

### Android / ARM64 (Termux)

Notes for Android/ARM64:

- Termux: no FUSE without root; use the fallback:
  `stretchstore serve --dir ./vol`   (then open http://127.0.0.1:8080 on the device)
- `stretchmem`, `stretchcpu`, `stretchnet` work identically on every OS (no FUSE involved).
- Self-tests: `stretchstore test` | `stretchmem test` | `stretchcpu test` | `stretchnet test`

```bash
pkg install -y curl
curl -fsSL https://raw.githubusercontent.com/jaykk99/stretch/main/install.sh | sh
# installer detects Termux and picks the android/arm64 build, installs to $PREFIX/bin
stretchstore create --logical 100GB --physical 512MB --dir ~/vol
stretchstore serve --dir ./vol
# open http://127.0.0.1:8080 in the device browser
```

---

## stretchstore — thin storage

A 1000GB logical drive backed by 1GB of real storage. Thin provisioning +
zstd compression + content-defined chunking (FastCDC-style rolling Gear hash,
16–256KB) + SHA-256 dedup. Crash-safe append-only journal with CRCs.

```bash
stretchstore create --logical 1000GB --physical 1GB --dir ./vol
stretchstore status --dir ./vol     # the truth: logical vs physical
stretchstore mount  --dir ./vol --mp /mnt/stretch     # FUSE (Linux/macOS)
stretchstore serve  --dir ./vol [--addr 127.0.0.1:8080]  # no-FUSE fallback
stretchstore test                   # autonomous self-test, honest report
stretchstore verify --dir ./vol     # integrity audit (chunk refs, journal CRCs)
stretchstore prune  --dir ./vol     # garbage-collect orphaned chunks
stretchstore version                # print version
stretchstore completion [bash|zsh|fish]  # shell completions
```

**Verify & prune.** Overwriting a slot orphans its old chunks — Verify reports
them (plus any missing/corrupt chunks, slot-tiling errors, and journal CRC
failures; exit code 1 on data problems), and Prune compacts `chunks.dat` down
to the referenced chunks and reclaims the space, crash-safely (the old file
is only replaced by an atomic rename once the new one is fsynced).

**No-FUSE mode** (`serve`): the volume appears as a virtual `disk.img` over
HTTP on localhost — Range reads, `PUT /disk.img?offset=N` writes, a minimal
`PROPFIND` for file-manager browsing, `/api/status` JSON, and a small HTML
browser at `/`. A full physical store answers **507 Insufficient Storage** —
the same honest refusal as `ENOSPC` on a kernel mount.

**Measured** (`stretchstore test`, 1000GB thin / 1GB cap — real run):

| Data | Logical | Physical | Ratio |
|---|---|---|---|
| 1GB of zeros | 1.00 GB | 39.04 KB | 26,858:1 |
| 100 × 10MB identical | 1000 MB | 5.27 MB | 189.9:1 |
| 200MB mixed realistic | 200 MB | 20.10 MB | 10.0:1 |
| 100MB pure random (adversarial) | 100 MB | 100.13 MB | **1.0:1** |
| fill to the wall | +896 MB random | refused at 1022.73 MB | cap wall, data intact |

## stretchmem — compressed RAM

Userspace zram + KSM: allocate segments, write pages, read them back.
4KB pages, each zstd-compressed and SHA-256 deduplicated across all segments.
Zero pages cost nothing. Hard physical cap — writes that don't fit are refused.

```bash
stretchmem test [--cap 512MB]   # autonomous self-test, honest report
stretchmem version
stretchmem completion [bash|zsh|fish]
```

**Measured** (`stretchmem test`, 512MB cap — real run):

| Data | Logical | Physical | Ratio |
|---|---|---|---|
| 64MB zeros | 64 MB | 0 B | infinite |
| 100 × 1MB identical | 100 MB | 399 KB | 256.6:1 |
| 32MB mixed (text/binary/random pages) | 32 MB | 6.53 MB | 4.9:1 |
| 16MB pure random (adversarial) | 16 MB | 16.30 MB | **1.0:1** (framing costs a hair) |

## stretchcpu — never compute twice

Content-addressed memoization. A job is hashed by (command argv + stdin
bytes); the first run executes for real, repeats return cached bytes
instantly. This multiplies *effective* throughput on repeated work — it does
not make cores faster and does nothing for unique work.

```bash
stretchcpu run --dir ~/.cache/stretchcpu -- <cmd> [args...] < stdin
stretchcpu stats | stretchcpu clear
stretchcpu test        # 1000x the same expensive job, honest report
stretchcpu version
stretchcpu completion [bash|zsh|fish]
```

**Measured** (`stretchcpu test` — real run): 1000 submissions of a
deterministic 2M-iteration SHA-256 job → **1 real execution, 999 cache hits**,
all outputs byte-identical, 112x effective speedup (336ms × 1000 naive vs 3s wall).

**Contract:** deterministic commands ONLY. Memoizing `date`, random numbers,
or side effects (emails, charges) returns stale/wrong results. That's the
documented deal, not a bug.

## stretchnet — download once

A deduplicating fetch cache. Bodies are SHA-256 content-addressed and
zstd-compressed at rest; identical bodies at different URLs share one copy.
Bandwidth accounting tracks every byte saved.

```bash
stretchnet fetch [--refresh] <url>   # body -> stdout, HIT/MISS -> stderr
stretchnet stats | stretchnet clear
stretchnet test                      # 100x fetch, honest report
stretchnet version
stretchnet completion [bash|zsh|fish]
```

**Measured** (`stretchnet test` — real run, local server): 100 fetches of a
5MB file → **1 network transfer, 99 cache hits**, 495MB bandwidth saved, all
bytes identical.

**Limit:** v1 caches by URL with no revalidation — cached bytes are served
as-is until `--refresh`. Don't cache things that change every request.

---

## Building releases

```bash
./scripts/make-release.sh 0.2.0   # -> dist/*.tar.gz + SHA256SUMS.txt
```

Cross-compile matrix (all `CGO_ENABLED=0`, static):

| Target | Binaries | FUSE |
|---|---|---|
| linux/amd64 | ✅ 4/4 | ✅ |
| linux/arm64 | ✅ 4/4 | ✅ |
| darwin/amd64 | ✅ 4/4 | ✅ (macFUSE) |
| darwin/arm64 | ✅ 4/4 | ✅ (macFUSE) |
| windows/amd64 | ✅ 4/4 | ❌ → `serve` |
| android/arm64 | ✅ 4/4 | ❌ → `serve` |

The device smoke test is the matrix itself plus `go vet`: if all six targets
compile and vet is clean, every device above can run the suite.

Fuzz the chunker and benchmark the store's hot paths:

```bash
go test -fuzz='^FuzzChunk$' -fuzztime=60s ./internal/cdc/      # tiling, determinism
go test -fuzz='^FuzzChunkResync$' -fuzztime=60s ./internal/cdc/ # resync after inserts
go test -run=NONE -bench=. ./internal/store/                   # write/read/verify/prune
```

## Design notes (stretchstore)

- **Crash safety:** `chunks.dat` and `manifest.log` are append-only; blobs,
  then journal records, then fsyncs — in that order. A torn tail is truncated
  on open; unacknowledged writes are dropped, which is always safe.
  Corruption in the *middle* of either file is not a crash artifact, so
  `Open` refuses with an error instead of silently discarding acknowledged
  data.
- **Write atomicity vs. the cap:** the physical budget is checked *before*
  any mutation, measured exactly (chunk blobs + every journal byte). A write
  either fits entirely or is refused (`ENOSPC`); usage can never creep past
  the cap.
- **Granularity:** 1MB logical slots, independently re-chunked per write
  (avg 64KB chunks).
- **No compaction (yet):** dereferenced chunks aren't reclaimed — overwriting
  frees logical space but the physical file doesn't shrink. `status` is honest
  about it.

## Honest limits (all four)

- Unique incompressible data is ~1:1 everywhere. Period.
- stretchcpu only helps repeated deterministic work; stretchnet only helps
  repeated fetches of stable URLs.
- stretchstore: one disk dies, data dies — no redundancy, no encryption.
- The FUSE layer and the `serve` fallback are volume frontends (one big
  `disk.img`), not full filesystems.


## Adaptive local model runtime (Stretch + Model Squeezer)

The shared Python 3.9+ launcher uses llama.cpp for CPU inference. It does not
require an API key or a hosted server. Run from either repository checkout:

    python3 stretch_model.py owner/model-GGUF --plan
    python3 stretch_model.py owner/model-GGUF --prompt "Hello"
    python3 stretch_model.py /path/to/model.gguf --prompt "Hello"

On Windows use `python` instead of `python3`. For a public direct HTTPS GGUF URL,
pass the URL as the first argument. Use --sha256 with a trusted file digest
for integrity; pinned URLs are recommended.

The launcher detects available RAM, sets a conservative weight budget, selects a
single-file GGUF from a repository containing variants of the same model,
downloads in bounded chunks, reuses the local cache, and sets context and
thread counts. It uses an installed llama-cli or builds llama.cpp from source
on first run (git, CMake, and C++ compiler required). No GPU is required.
For Android/Termux install prerequisites:

    pkg install python git cmake clang make

For Debian/Ubuntu/Chromebook Linux:

    sudo apt install python3 git cmake build-essential

For Windows, install Python, Git, CMake and Visual Studio C++ Build Tools,
or an existing llama-cli from llama.cpp; for macOS install Python, Git, CMake
and Xcode command-line tools. Pass --runtime /path/to/llama-cli to reuse a
native runtime; --no-build refuses to download runtime source.

Both repositories use the same STRETCH_MODEL_CACHE directory (default
~/.cache/stretch-models). Set --cache to a real USB/SSD filesystem path to
share storage between runs. The ordinary filesystem permits memory mapping;
Stretch's virtual disk HTTP endpoint is NOT a drop-in mmap filesystem.
The launcher does not inject Stretch's userspace memory store into llama.cpp.

For local unquantized GGUF, add --quantize Q4_K_M to create a smaller copy
without altering the original. Quantization itself needs RAM and temporary
disk; prefer a prequantized file on weak devices. Lossy quantization changes
weights and can degrade accuracy. Repeated quantization cannot shrink forever.
LoRA adapters and safetensors require architecture-compatible GGUF conversion
or merging first; arbitrary model architectures are not supported.

--disk-mode explicitly permits a larger-than-budget mmap experiment; it is
not a guarantee of usable speed or successful loading. --context and --ram-mib
allow stricter settings. Model file size is not total memory use; the KV cache
and architecture matter. First-time builds/downloads need internet; local
GGUF plus installed runtime works offline. iOS, browser-only devices, and
microcontrollers are not supported by this command-line launcher.

Tests: python -m unittest test_stretch_model -v. CI also attempts a real
small-model generation on Linux. Cross-platform test coverage does not
substitute for a physical Android/Chromebook inference test.


### Low-memory execution and layer streaming

GGUF on a constrained device:

    python stretch_model.py /path/to/model.gguf --disk-mode --context 512 --prompt "Hello"

The launcher probes the installed llama.cpp's actual flags. With modern
versions it enables mmap and lazy tensor reads (only tensors supported by
the runtime), disables weight repacking and warmup, bounds inference batch
sizes, and disables additional prompt caching where available. Older runtimes
fall back to advertised mmap flags. A one-shot prompt retries once at reduced
context after a possible memory allocation failure; --no-retry disables that.
Interactive sessions are not replayed. This is OS page-cache management, not
a custom GGUF layer scheduler or a guaranteed RAM cap.

Optional true module-level disk offload for local safetensors models, on
systems with compatible PyTorch wheels (desktop Linux/macOS/Windows):

    python -m pip install torch transformers accelerate safetensors
    python stretch_stream.py /path/to/local-model --offload-dir /path/to/ssd --ram-mib 2048 --plan
    python stretch_stream.py /path/to/local-model --offload-dir /path/to/ssd --ram-mib 2048 --prompt "Hello"

This builds an empty model to infer a CPU/disk device map, loads safe tensor
checkpoints, and uses Accelerate's execution hooks to bring disk-offloaded
weights into memory on demand. Temporary offload files are cleaned after
normal completion. KV caching is disabled to reduce persistent memory.
A CPU layer and working buffers must still fit; no total RSS guarantee.
--require-offload refuses runs where all weights fit in RAM. Disk offload can
be very slow, especially without an SSD. No remote custom model code is
executed. The optional backend is not yet supported on Termux/iOS. It uses
ordinary safetensors, not GGUF or arbitrary model formats.

Run the real synthetic Transformer smoke test with:

    python smoke_stretch_stream.py
