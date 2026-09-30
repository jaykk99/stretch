package netcache

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func testServer(t *testing.T, hits *atomic.Int64, body []byte) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Write(body)
	}))
}

func TestFetchOnceServeMany(t *testing.T) {
	var hits atomic.Int64
	body := bytes.Repeat([]byte("cached file contents. "), 100000) // ~2.3MB
	srv := testServer(t, &hits, body)
	defer srv.Close()

	c, err := New(filepath.Join(t.TempDir(), "cache"), srv.Client())
	if err != nil {
		t.Fatal(err)
	}
	var first []byte
	for i := 0; i < 20; i++ {
		got, fromCache, err := c.Fetch(srv.URL+"/file", false)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && fromCache {
			t.Fatal("first fetch should hit network")
		}
		if i > 0 && !fromCache {
			t.Fatal("repeat fetch should be cached")
		}
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Fatal("cached bytes differ")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("server hit %d times, want 1", hits.Load())
	}
	if !bytes.Equal(first, body) {
		t.Fatal("fetched bytes != server bytes")
	}
	st := c.Stats()
	if st.NetworkFetches != 1 || st.CacheHits != 19 {
		t.Fatalf("fetches=%d hits=%d, want 1/19", st.NetworkFetches, st.CacheHits)
	}
	if st.SavedBytes() != uint64(19*len(body)) {
		t.Fatalf("saved=%d, want %d", st.SavedBytes(), 19*len(body))
	}
}

func TestIdenticalBodiesDeduped(t *testing.T) {
	var hits atomic.Int64
	body := bytes.Repeat([]byte("same bytes, two urls. "), 50000)
	srv := testServer(t, &hits, body)
	defer srv.Close()

	c, _ := New(filepath.Join(t.TempDir(), "cache"), srv.Client())
	if _, _, err := c.Fetch(srv.URL+"/a", false); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Fetch(srv.URL+"/b", false); err != nil {
		t.Fatal(err)
	}
	st := c.Stats()
	if st.StoredBodies != 1 {
		t.Fatalf("identical bodies stored %d times, want 1", st.StoredBodies)
	}
	if st.URLs != 2 {
		t.Fatalf("urls=%d, want 2", st.URLs)
	}
}

func TestRefreshRefetches(t *testing.T) {
	var hits atomic.Int64
	srv := testServer(t, &hits, []byte("v1"))
	defer srv.Close()
	c, _ := New(filepath.Join(t.TempDir(), "cache"), srv.Client())
	c.Fetch(srv.URL+"/f", false)
	c.Fetch(srv.URL+"/f", false)
	if hits.Load() != 1 {
		t.Fatalf("hits=%d, want 1", hits.Load())
	}
	if _, fromCache, err := c.Fetch(srv.URL+"/f", true); err != nil || fromCache {
		t.Fatalf("refresh should hit network: fromCache=%v err=%v", fromCache, err)
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d after refresh, want 2", hits.Load())
	}
}

func TestCorruptBodyRefetches(t *testing.T) {
	var hits atomic.Int64
	body := []byte("real body")
	srv := testServer(t, &hits, body)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "cache")
	c, _ := New(dir, srv.Client())
	got, _, err := c.Fetch(srv.URL+"/f", false)
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("fetch: %v", err)
	}
	// Corrupt the stored body on disk.
	ents, _ := filepath.Glob(filepath.Join(dir, "bodies", "*.zst"))
	if len(ents) != 1 {
		t.Fatalf("expected 1 body file, got %d", len(ents))
	}
	f, err := os.OpenFile(ents[0], os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteAt([]byte{0xFF}, 10)
	f.Close()
	got2, fromCache, err := c.Fetch(srv.URL+"/f", false)
	if err != nil {
		t.Fatal(err)
	}
	if fromCache {
		t.Fatal("corrupt body served from cache")
	}
	if !bytes.Equal(got2, body) {
		t.Fatal("refetched body mismatch")
	}
	if hits.Load() != 2 {
		t.Fatalf("hits=%d, want 2 (refetch after corruption)", hits.Load())
	}
}

func TestHTTPErrorNotCached(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	c, _ := New(filepath.Join(t.TempDir(), "cache"), srv.Client())
	if _, _, err := c.Fetch(srv.URL+"/nope", false); err == nil {
		t.Fatal("expected error for 404")
	}
	st := c.Stats()
	if st.URLs != 0 || st.NetworkFetches != 0 {
		t.Fatal("failed fetch was cached/counted")
	}
}
