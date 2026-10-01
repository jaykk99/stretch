// Command stretchcpu is the CLI for StretchCPU: content-addressed computation
// caching. Never compute the same thing twice.
//
//	stretchcpu run [--dir ~/.cache/stretchcpu] -- <cmd> [args...]  < stdin
//	stretchcpu stats [--dir ...]
//	stretchcpu clear [--dir ...]
//	stretchcpu test            # 1000x the same expensive job, honest report
//	stretchcpu burn --iters N   # deterministic CPU-burn used by the test
//
// Contract: only deterministic commands. Same argv+stdin => same bytes out.
// Nondeterministic or side-effecting commands must NOT be memoized.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"stretchstore/internal/cliutil"
	"stretchstore/internal/memoize"
)

func defaultDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "stretchcpu")
	}
	return filepath.Join(os.TempDir(), "stretchcpu")
}

// subcommands lists every top-level command, for help and completions.
var subcommands = []string{"run", "stats", "clear", "test", "burn", "help", "version", "completion"}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "run":
		err = cmdRun(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "clear":
		err = cmdClear(os.Args[2:])
	case "test":
		err = cmdTest(os.Args[2:])
	case "burn":
		err = cmdBurn(os.Args[2:])
	case "version":
		fmt.Println(cliutil.VersionLine("stretchcpu"))
	case "completion":
		err = cmdCompletion(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func cmdCompletion(args []string) error {
	shell := "bash"
	if len(args) > 0 {
		shell = args[0]
	}
	out, err := cliutil.Completion("stretchcpu", subcommands, shell)
	if err != nil {
		return err
	}
	fmt.Print(out)
	return nil
}

func usage() {
	fmt.Print(`stretchcpu — never compute the same thing twice.

Jobs are hashed by (command + stdin). First run executes for real; repeats
return cached bytes instantly. Deterministic commands ONLY.

Usage:
  stretchcpu run [--dir DIR] -- <cmd> [args...]   < stdin
  stretchcpu stats [--dir DIR]
  stretchcpu clear [--dir DIR]
  stretchcpu test
  stretchcpu version
  stretchcpu completion [bash|zsh|fish]   (shell completions on stdout)
`)
}

// burn is the deterministic expensive job: a fixed number of SHA-256
// iterations. Same --iters => same output bytes, always. Slow on purpose.
func cmdBurn(args []string) error {
	fs := flag.NewFlagSet("burn", flag.ExitOnError)
	iters := fs.Int("iters", 2000000, "hash iterations")
	fs.Parse(args)
	h := sha256.Sum256([]byte("stretchcpu-burn-seed"))
	for i := 0; i < *iters; i++ {
		h = sha256.Sum256(h[:])
	}
	fmt.Printf("burn(%d)=%s\n", *iters, hex.EncodeToString(h[:]))
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	fs.Parse(args)
	rest := fs.Args()
	if len(rest) > 0 && rest[0] == "--" {
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return fmt.Errorf("no command given: stretchcpu run -- <cmd> [args...]")
	}
	stdin, err := io.ReadAll(io.LimitReader(os.Stdin, memoize.MaxStdin+1))
	if err != nil {
		return err
	}
	c, err := memoize.New(*dir, nil)
	if err != nil {
		return err
	}
	res, hit, err := c.Run(rest, stdin)
	if err != nil {
		return err
	}
	os.Stdout.Write(res.Stdout)
	if len(res.Stderr) > 0 {
		os.Stderr.Write(res.Stderr)
	}
	kind := "MISS"
	if hit {
		kind = "HIT"
	}
	fmt.Fprintf(os.Stderr, "[stretchcpu %s] job %s exit=%d\n", kind, memoize.JobID(rest, stdin)[:12], res.ExitCode)
	if res.ExitCode != 0 {
		os.Exit(res.ExitCode)
	}
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	fs.Parse(args)
	c, err := memoize.New(*dir, nil)
	if err != nil {
		return err
	}
	st := c.Stats()
	total := st.Hits + st.Misses
	hitRate := 0.0
	if total > 0 {
		hitRate = 100 * float64(st.Hits) / float64(total)
	}
	fmt.Printf("jobs: %d (%d hits, %.1f%%)  misses/executions: %d\n", total, st.Hits, hitRate, st.Misses)
	fmt.Printf("bytes served from cache: %d  cache on disk: %d\n", st.BytesFromCache, st.CacheDirBytes)
	return nil
}

func cmdClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	fs.Parse(args)
	c, err := memoize.New(*dir, nil)
	if err != nil {
		return err
	}
	return c.Clear()
}

// cmdTest is the honest proof: the same expensive job submitted 1000 times
// through the real CLI (real subprocess spawns), 1 real execution.
func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	n := fs.Int("n", 1000, "submissions")
	iters := fs.Int("iters", 2000000, "burn iterations per execution")
	fs.Parse(args)

	dir, err := os.MkdirTemp("", "stretchcpu-test")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	job := []string{exe, "burn", "--iters", fmt.Sprint(*iters)}

	fmt.Printf("StretchCPU self-test — submitting the same expensive job %d times\n", *n)
	fmt.Printf("job: burn --iters %d (deterministic SHA-256 chain)\n\n", *iters)

	var first []byte
	var hits, misses int
	var execTook time.Duration
	t0 := time.Now()
	for i := 0; i < *n; i++ {
		cmd := exec.Command(exe, append([]string{"run", "--dir", dir, "--"}, job...)...)
		var so, se bytes.Buffer
		cmd.Stdout = &so
		cmd.Stderr = &se
		r0 := time.Now()
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("submission %d: %w\n%s", i, err, se.String())
		}
		if i == 0 {
			execTook = time.Since(r0)
		}
		if bytes.Contains(se.Bytes(), []byte("[stretchcpu HIT]")) {
			hits++
		} else {
			misses++
		}
		if first == nil {
			first = so.Bytes()
		} else if !bytes.Equal(first, so.Bytes()) {
			return fmt.Errorf("submission %d: output not byte-identical", i)
		}
		if i%250 == 249 {
			fmt.Printf("  ... %d/%d\n", i+1, *n)
		}
	}
	wall := time.Since(t0)
	naive := time.Duration(*n) * execTook

	fmt.Println("\n================ HONEST REPORT ================")
	fmt.Printf("  submissions:      %d\n", *n)
	fmt.Printf("  real executions:  %d\n", misses)
	fmt.Printf("  cache hits:       %d\n", hits)
	fmt.Printf("  outputs:          all %d byte-identical\n", len(first))
	fmt.Printf("  one execution:    %s\n", execTook.Round(time.Millisecond))
	fmt.Printf("  wall time:        %s\n", wall.Round(time.Millisecond))
	fmt.Printf("  naive (no cache): ~%s\n", naive.Round(time.Second))
	fmt.Printf("  effective speedup: %.0fx  (fewer executions, not faster cores)\n",
		float64(naive)/float64(wall))
	fmt.Println("\n  THE PHYSICS: nothing got faster. The core still runs at the same")
	fmt.Println("  speed — we just refused to redo work. 1000 identical jobs cost 1")
	fmt.Println("  execution + 999 cache lookups. This multiplies EFFECTIVE throughput")
	fmt.Println("  on repeated work and does nothing at all for unique work.")
	fmt.Println("  Deterministic commands only: memoizing `date`, random numbers, or")
	fmt.Println("  side effects (emails, charges) returns stale/wrong results.")
	if misses != 1 {
		return fmt.Errorf("expected exactly 1 real execution, got %d", misses)
	}
	return nil
}
