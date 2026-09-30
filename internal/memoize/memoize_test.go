package memoize

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func testCache(t *testing.T, exec Executor) *Cache {
	t.Helper()
	c, err := New(filepath.Join(t.TempDir(), "cache"), exec)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRepeatRunsOnce(t *testing.T) {
	var runs atomic.Int64
	exec := func(argv []string, stdin []byte) (Result, error) {
		runs.Add(1)
		time.Sleep(50 * time.Millisecond) // simulate expensive
		return Result{Stdout: []byte("digest:abc123"), ExitCode: 0}, nil
	}
	c := testCache(t, exec)
	var first []byte
	for i := 0; i < 50; i++ {
		res, hit, err := c.Run([]string{"burn", "--iters", "1000"}, []byte("input"))
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && hit {
			t.Fatal("first run should be a miss")
		}
		if i > 0 && !hit {
			t.Fatal("repeat run should be a hit")
		}
		if first == nil {
			first = res.Stdout
		} else if !bytes.Equal(first, res.Stdout) {
			t.Fatal("cached output not byte-identical")
		}
	}
	if runs.Load() != 1 {
		t.Fatalf("executed %d times, want 1", runs.Load())
	}
	st := c.Stats()
	if st.Hits != 49 || st.Misses != 1 {
		t.Fatalf("hits=%d misses=%d, want 49/1", st.Hits, st.Misses)
	}
}

func TestDifferentInputsDifferentJobs(t *testing.T) {
	var runs atomic.Int64
	c := testCache(t, func(argv []string, stdin []byte) (Result, error) {
		runs.Add(1)
		return Result{Stdout: append([]byte("out:"), stdin...), ExitCode: 0}, nil
	})
	r1, _, _ := c.Run([]string{"cmd"}, []byte("a"))
	r2, _, _ := c.Run([]string{"cmd"}, []byte("b"))
	if bytes.Equal(r1.Stdout, r2.Stdout) {
		t.Fatal("different inputs returned same output")
	}
	if runs.Load() != 2 {
		t.Fatalf("runs=%d, want 2", runs.Load())
	}
}

func TestStdinPartOfIdentity(t *testing.T) {
	var runs atomic.Int64
	c := testCache(t, func(argv []string, stdin []byte) (Result, error) {
		runs.Add(1)
		return Result{Stdout: stdin, ExitCode: 0}, nil
	})
	c.Run([]string{"cat"}, []byte("one"))
	c.Run([]string{"cat"}, []byte("two"))
	if runs.Load() != 2 {
		t.Fatal("stdin not part of job identity")
	}
}

func TestCorruptEntryReexecutes(t *testing.T) {
	var runs atomic.Int64
	dir := t.TempDir()
	c, _ := New(filepath.Join(dir, "c"), func(argv []string, stdin []byte) (Result, error) {
		runs.Add(1)
		return Result{Stdout: []byte("fresh"), ExitCode: 0}, nil
	})
	c.Run([]string{"x"}, nil)
	id := JobID([]string{"x"}, nil)
	// Corrupt the entry on disk.
	os.WriteFile(filepath.Join(dir, "c", "jobs", id+".json"), []byte("{corrupt"), 0o644)
	res, hit, err := c.Run([]string{"x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if hit {
		t.Fatal("corrupt entry should not hit")
	}
	if runs.Load() != 2 {
		t.Fatalf("runs=%d, want 2 (re-executed after corruption)", runs.Load())
	}
	if !bytes.Equal(res.Stdout, []byte("fresh")) {
		t.Fatal("wrong output after re-execution")
	}
}

func TestExitCodePreserved(t *testing.T) {
	c := testCache(t, func(argv []string, stdin []byte) (Result, error) {
		return Result{Stdout: []byte("nope"), ExitCode: 3}, nil
	})
	r1, _, _ := c.Run([]string{"failer"}, nil)
	r2, hit, _ := c.Run([]string{"failer"}, nil)
	if !hit || r2.ExitCode != 3 || !bytes.Equal(r1.Stdout, r2.Stdout) {
		t.Fatal("exit code / output not preserved across cache")
	}
}

func TestDefaultExecutorReal(t *testing.T) {
	// The real subprocess path: no mocks.
	res, err := DefaultExecutor([]string{"echo", "hello"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(res.Stdout), []byte("hello")) {
		t.Fatalf("unexpected stdout %q", res.Stdout)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %d", res.ExitCode)
	}
	if _, err := DefaultExecutor(nil, nil); err == nil {
		t.Fatal("expected error for empty command")
	}
}

func TestStatsPersist(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "c")
	c1, _ := New(dir, func(argv []string, stdin []byte) (Result, error) {
		return Result{Stdout: []byte("x"), ExitCode: 0}, nil
	})
	c1.Run([]string{"a"}, nil)
	c1.Run([]string{"a"}, nil)
	c2, _ := New(dir, nil)
	st := c2.Stats()
	if st.Hits != 1 || st.Misses != 1 {
		t.Fatalf("persisted stats hits=%d misses=%d", st.Hits, st.Misses)
	}
}
