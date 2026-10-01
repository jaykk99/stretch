// Package memoize is content-addressed computation caching: never compute
// the same thing twice.
//
// A job is identified by SHA-256(command argv + stdin bytes). The first
// run executes for real and stores the compressed result; repeats return
// the cached bytes instantly with byte-identical output.
//
// The honest framing: this multiplies EFFECTIVE throughput on repeated
// work (1000 identical jobs → 1 execution). It does not make cores faster,
// it does not parallelize anything, and it only works for DETERMINISTIC
// commands. A command that reads the clock, a random number, or the
// network will return stale cached output — that is the documented contract,
// not a bug. Side-effecting commands (send email, charge card) must never
// be memoized; the cache cannot know what a command does.
//
// There is no long-running daemon process: the "daemon" is the persistent
// cache directory. Every CLI invocation is a client of it.
package memoize

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"
)

// MaxStdin is the largest stdin captured into a job identity.
const MaxStdin = 256 << 20

// Result is a completed job's captured output.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
	Took     time.Duration
}

// Executor runs a job for real. DefaultExecutor shells out via os/exec.
type Executor func(argv []string, stdin []byte) (Result, error)

// DefaultExecutor runs argv[0] with argv[1:] as a real subprocess.
func DefaultExecutor(argv []string, stdin []byte) (Result, error) {
	if len(argv) == 0 {
		return Result{}, errors.New("empty command")
	}
	t0 := time.Now()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Stdin = bytes.NewReader(stdin)
	var so, se bytes.Buffer
	cmd.Stdout = &so
	cmd.Stderr = &se
	err := cmd.Run()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return Result{}, err
		}
	}
	return Result{Stdout: so.Bytes(), Stderr: se.Bytes(), ExitCode: code, Took: time.Since(t0)}, nil
}

// JobID hashes argv (length-prefixed) and stdin into the job identity.
func JobID(argv []string, stdin []byte) string {
	h := sha256.New()
	for _, a := range argv {
		fmt.Fprintf(h, "%d:%s;", len(a), a)
	}
	h.Write([]byte{0})
	h.Write(stdin)
	return hex.EncodeToString(h.Sum(nil))
}

type entry struct {
	Argv     []string `json:"argv"`
	StdinSHA string   `json:"stdin_sha"`
	Stdout   string   `json:"stdout"` // base64(zstd)
	Stderr   string   `json:"stderr"`
	ExitCode int      `json:"exit"`
	TookNs   int64    `json:"took_ns"`
}

// Stats persists across runs in stats.json.
type Stats struct {
	Hits           uint64 `json:"hits"`
	Misses         uint64 `json:"misses"`
	BytesFromCache uint64 `json:"bytes_from_cache"`
	CacheDirBytes  uint64 `json:"cache_dir_bytes"`
}

type Cache struct {
	mu    sync.Mutex // guards jobs on disk, stats, and the stats file
	dir   string
	jobs  string
	exec  Executor
	enc   *zstd.Encoder
	dec   *zstd.Decoder
	stats Stats
}

// New opens (or creates) the cache directory.
func New(dir string, exec Executor) (*Cache, error) {
	if exec == nil {
		exec = DefaultExecutor
	}
	jobs := filepath.Join(dir, "jobs")
	if err := os.MkdirAll(jobs, 0o755); err != nil {
		return nil, err
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	c := &Cache{dir: dir, jobs: jobs, exec: exec, enc: enc, dec: dec}
	if data, err := os.ReadFile(filepath.Join(dir, "stats.json")); err == nil {
		json.Unmarshal(data, &c.stats)
	}
	return c, nil
}

func (c *Cache) saveStats() {
	data, _ := json.Marshal(c.stats)
	os.WriteFile(filepath.Join(c.dir, "stats.json"), data, 0o644)
}

func (c *Cache) path(id string) string { return filepath.Join(c.jobs, id+".json") }

func (c *Cache) load(id string) (Result, bool) {
	data, err := os.ReadFile(c.path(id))
	if err != nil {
		return Result{}, false
	}
	var e entry
	if json.Unmarshal(data, &e) != nil {
		return Result{}, false // corrupt entry: treat as miss, overwrite below
	}
	so, err1 := base64.StdEncoding.DecodeString(e.Stdout)
	se, err2 := base64.StdEncoding.DecodeString(e.Stderr)
	if err1 != nil || err2 != nil {
		return Result{}, false
	}
	out, err1 := c.dec.DecodeAll(so, nil)
	errb, err2 := c.dec.DecodeAll(se, nil)
	if err1 != nil || err2 != nil {
		return Result{}, false
	}
	return Result{Stdout: out, Stderr: errb, ExitCode: e.ExitCode, Took: time.Duration(e.TookNs)}, true
}

// Run executes the job or returns the cached result. hit reports which.
// It is safe for concurrent use: the whole check-execute-store sequence is
// serialized, so two goroutines racing the same job produce one execution
// and one cache hit, never a torn entry or a stats race.
func (c *Cache) Run(argv []string, stdin []byte) (res Result, hit bool, err error) {
	if len(stdin) > MaxStdin {
		return Result{}, false, fmt.Errorf("stdin exceeds %d bytes", MaxStdin)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	id := JobID(argv, stdin)
	if res, ok := c.load(id); ok {
		c.stats.Hits++
		c.stats.BytesFromCache += uint64(len(res.Stdout))
		c.saveStats()
		return res, true, nil
	}
	res, err = c.exec(argv, stdin)
	if err != nil {
		return Result{}, false, err
	}
	sh := sha256.Sum256(stdin)
	e := entry{
		Argv:     argv,
		StdinSHA: hex.EncodeToString(sh[:]),
		Stdout:   base64.StdEncoding.EncodeToString(c.enc.EncodeAll(res.Stdout, nil)),
		Stderr:   base64.StdEncoding.EncodeToString(c.enc.EncodeAll(res.Stderr, nil)),
		ExitCode: res.ExitCode,
		TookNs:   int64(res.Took),
	}
	data, _ := json.Marshal(e)
	tmp := c.path(id) + ".tmp"
	if werr := os.WriteFile(tmp, data, 0o644); werr == nil {
		os.Rename(tmp, c.path(id)) // atomic commit; corrupt tmp never observed
	}
	c.stats.Misses++
	c.saveStats()
	return res, false, nil
}

// Stats returns cumulative hit/miss accounting plus current cache size.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.stats
	var total uint64
	filepath.Walk(c.jobs, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += uint64(info.Size())
		}
		return nil
	})
	st.CacheDirBytes = total
	return st
}

// Clear wipes all cached results (stats are kept).
func (c *Cache) Clear() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	ents, err := os.ReadDir(c.jobs)
	if err != nil {
		return err
	}
	for _, e := range ents {
		os.Remove(filepath.Join(c.jobs, e.Name()))
	}
	return nil
}
