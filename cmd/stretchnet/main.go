// Command stretchnet is the CLI for StretchNet: a deduplicing fetch cache.
// Download a URL once, serve it N times locally.
//
//	stretchnet fetch [--dir DIR] [--refresh] <url>   # body -> stdout, HIT/MISS -> stderr
//	stretchnet stats [--dir DIR]
//	stretchnet clear [--dir DIR]
//	stretchnet test                                  # 100x fetch, honest report
//
// v1 caches by URL with no revalidation: cached bytes are served as-is until
// --refresh. Stale data is the documented tradeoff.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"

	"stretchstore/internal/netcache"
	"stretchstore/internal/size"
)

func defaultDir() string {
	if d, err := os.UserCacheDir(); err == nil {
		return filepath.Join(d, "stretchnet")
	}
	return filepath.Join(os.TempDir(), "stretchnet")
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "fetch":
		err = cmdFetch(os.Args[2:])
	case "stats":
		err = cmdStats(os.Args[2:])
	case "clear":
		err = cmdClear(os.Args[2:])
	case "test":
		err = cmdTest(os.Args[2:])
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

func usage() {
	fmt.Print(`stretchnet — download once, serve N times.

Content-addressed (SHA-256) + zstd-compressed at rest. Identical bodies
across different URLs are stored once. No revalidation in v1: use --refresh.

Usage:
  stretchnet fetch [--dir DIR] [--refresh] <url>
  stretchnet stats [--dir DIR]
  stretchnet clear [--dir DIR]
  stretchnet test
`)
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	refresh := fs.Bool("refresh", false, "force network re-download")
	fs.Parse(args)
	if fs.NArg() != 1 {
		return fmt.Errorf("one URL required")
	}
	c, err := netcache.New(*dir, nil)
	if err != nil {
		return err
	}
	body, fromCache, err := c.Fetch(fs.Arg(0), *refresh)
	if err != nil {
		return err
	}
	os.Stdout.Write(body)
	kind := "MISS"
	if fromCache {
		kind = "HIT"
	}
	fmt.Fprintf(os.Stderr, "[stretchnet %s] %s (%s)\n", kind, fs.Arg(0), size.Format(uint64(len(body))))
	return nil
}

func cmdStats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	fs.Parse(args)
	c, err := netcache.New(*dir, nil)
	if err != nil {
		return err
	}
	st := c.Stats()
	fmt.Printf("urls: %d  unique bodies: %d (%s on disk)\n", st.URLs, st.StoredBodies, size.Format(st.StoredBytes))
	fmt.Printf("network fetches: %d (%s)  cache hits: %d\n", st.NetworkFetches, size.Format(st.BytesFetched), st.CacheHits)
	fmt.Printf("bandwidth saved: %s\n", size.Format(st.SavedBytes()))
	return nil
}

func cmdClear(args []string) error {
	fs := flag.NewFlagSet("clear", flag.ExitOnError)
	dir := fs.String("dir", defaultDir(), "cache dir")
	fs.Parse(args)
	c, err := netcache.New(*dir, nil)
	if err != nil {
		return err
	}
	return c.Clear()
}

// cmdTest is the honest proof: a local file served 100 times, 1 transfer.
// Uses a localhost test server so the proof is deterministic and offline.
func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	n := fs.Int("n", 100, "fetches")
	fs.Parse(args)

	// Deterministic 5MB file: compressible text-ish with variation.
	var buf bytes.Buffer
	seed := []byte("stretchnet-test-file; the quick brown fox; ")
	for buf.Len() < 5<<20 {
		buf.Write(seed)
	}
	file := buf.Bytes()[:5<<20]

	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(file)
	}))
	defer srv.Close()

	dir, err := os.MkdirTemp("", "stretchnet-test")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	c, err := netcache.New(dir, srv.Client())
	if err != nil {
		return err
	}
	url := srv.URL + "/bigfile.dat"

	fmt.Printf("StretchNet self-test — fetching the same %s file %d times\n\n", size.Format(uint64(len(file))), *n)
	var first []byte
	for i := 0; i < *n; i++ {
		got, _, err := c.Fetch(url, false)
		if err != nil {
			return err
		}
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			return fmt.Errorf("fetch %d: bytes differ", i)
		}
	}
	st := c.Stats()

	fmt.Println("================ HONEST REPORT ================")
	fmt.Printf("  fetches:          %d\n", *n)
	fmt.Printf("  network transfers: %d (%s)\n", st.NetworkFetches, size.Format(st.BytesFetched))
	fmt.Printf("  cache hits:       %d\n", st.CacheHits)
	fmt.Printf("  bytes:            all %d byte-identical\n", len(first))
	fmt.Printf("  bandwidth saved:  %s\n", size.Format(st.SavedBytes()))
	fmt.Printf("  stored on disk:   %s compressed (%.1f:1)\n",
		size.Format(st.StoredBytes), float64(len(file))/float64(max(st.StoredBytes, 1)))
	fmt.Println("\n  THE PHYSICS: the first fetch still costs a full download — nothing")
	fmt.Println("  here makes the network faster. What it does is refuse to pay twice:")
	fmt.Println("  100 fetches cost 1 transfer + 99 local reads. Identical bodies at")
	fmt.Println("  different URLs share one stored copy. v1 never revalidates: a")
	fmt.Println("  cached URL is served as-is until you --refresh. Don't cache things")
	fmt.Println("  that change every request.")
	if hits.Load() != 1 {
		return fmt.Errorf("server hit %d times, want exactly 1", hits.Load())
	}
	return nil
}
