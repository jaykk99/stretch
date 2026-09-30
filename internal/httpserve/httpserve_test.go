package httpserve

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"stretchstore/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Create(t.TempDir(), 100<<20, 64<<20) // 100MB logical, 64MB physical
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testHandler(t *testing.T, st *store.Store) http.Handler {
	t.Helper()
	return (&Server{st: st}).routes()
}

func TestRootAndStatus(t *testing.T) {
	st := testStore(t)
	h := testHandler(t, st)
	r := httptest.NewRequest("GET", "/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "disk.img") {
		t.Fatalf("root: code=%d body=%q", w.Code, w.Body.String()[:100])
	}
	r = httptest.NewRequest("GET", "/api/status", nil)
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "LogicalSize") {
		t.Fatalf("status: code=%d", w.Code)
	}
}

func TestPutGetRoundTrip(t *testing.T) {
	st := testStore(t)
	h := testHandler(t, st)
	data := bytes.Repeat([]byte("hello webdav world; "), 10000)

	r := httptest.NewRequest("PUT", "/disk.img?offset=1048576", bytes.NewReader(data))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("PUT: code=%d body=%s", w.Code, w.Body.String())
	}

	r = httptest.NewRequest("GET", "/disk.img", nil)
	r.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", 1048576, 1048576+len(data)-1))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 206 {
		t.Fatalf("range GET: code=%d", w.Code)
	}
	if !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("range bytes mismatch")
	}
}

func TestPutRefused507(t *testing.T) {
	st := testStore(t)
	h := testHandler(t, st)
	// Fill the physical store with fresh random (incompressible), coarse then
	// fine, so the next PUT honestly has nowhere to go.
	off := int64(0)
	for _, sz := range []int{8 << 20, 1 << 20, 256 << 10} {
		for {
			fill := make([]byte, sz)
			if _, err := io.ReadFull(rand.Reader, fill); err != nil {
				t.Fatal(err)
			}
			if _, err := st.WriteAt(fill, off); err != nil {
				if !errors.Is(err, store.ErrNoSpace) {
					t.Fatalf("fill: %v", err)
				}
				break
			}
			off += int64(sz)
		}
	}
	more := make([]byte, 1<<20)
	if _, err := io.ReadFull(rand.Reader, more); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("PUT", "/disk.img?offset=0", bytes.NewReader(more))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusInsufficientStorage {
		t.Fatalf("expected 507, got %d", w.Code)
	}
}

func TestPropfind(t *testing.T) {
	st := testStore(t)
	h := testHandler(t, st)
	r := httptest.NewRequest("PROPFIND", "/disk.img", nil)
	r.Header.Set("Depth", "1")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 207 {
		t.Fatalf("PROPFIND: code=%d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "disk.img") {
		t.Fatal("PROPFIND missing disk.img")
	}
}

func TestLiveServe(t *testing.T) {
	st := testStore(t)
	msg := []byte("live server bytes")
	if _, err := st.WriteAt(msg, 0); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, st, addr)
	time.Sleep(300 * time.Millisecond)

	resp, err := http.Get("http://" + addr + "/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	got, _ := io.ReadAll(io.LimitReader(resp.Body, int64(len(msg))))
	if !bytes.Equal(got, msg) {
		t.Fatalf("live GET mismatch: %q", got)
	}
	if resp.Header.Get("Accept-Ranges") != "bytes" {
		t.Fatal("missing Accept-Ranges")
	}
}
