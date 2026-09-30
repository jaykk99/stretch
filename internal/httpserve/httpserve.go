// Package httpserve is the no-FUSE fallback for StretchStore: the volume
// appears as a virtual disk.img over plain HTTP on localhost, plus enough
// WebDAV (OPTIONS/PROPFIND) for file managers to browse it.
//
// This is what Windows builds, Android/Termux, and FUSE-less containers
// use instead of `mount`. Reads honor HTTP Ranges; writes go through PUT
// with ?offset=. A full physical store answers 507 Insufficient Storage —
// the same honest refusal as ENOSPC on a kernel mount.
//
// Binds localhost only by default. Exposing it to a network is your call.
package httpserve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"stretchstore/internal/size"
	"stretchstore/internal/store"
)

// diskReader adapts the store to io.ReadSeeker for http.ServeContent.
type diskReader struct {
	st  *store.Store
	off int64
}

func (r *diskReader) Read(p []byte) (int, error) {
	n, err := r.st.ReadAt(p, r.off)
	r.off += int64(n)
	if err != nil {
		return n, err
	}
	if n == 0 {
		return 0, io.EOF
	}
	return n, nil
}

func (r *diskReader) Seek(offset int64, whence int) (int64, error) {
	sz := int64(r.st.Status().LogicalSize)
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = r.off + offset
	case io.SeekEnd:
		abs = sz + offset
	default:
		return 0, fmt.Errorf("bad whence")
	}
	if abs < 0 || abs > sz {
		return 0, fmt.Errorf("seek out of range")
	}
	r.off = abs
	return abs, nil
}

// writerAt adapts the store to io.Writer for streaming PUT bodies.
type offsetWriter struct {
	st  *store.Store
	off int64
}

func (w *offsetWriter) Write(p []byte) (int, error) {
	n, err := w.st.WriteAt(p, w.off)
	w.off += int64(n)
	return n, err
}

type Server struct {
	st *store.Store
}

func davHeaders(w http.ResponseWriter) {
	w.Header().Set("DAV", "1")
	w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, PROPFIND")
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	st := s.st.Status()
	fmt.Fprintf(w, `<!doctype html><html><head><meta charset=utf-8><title>StretchStore</title>
<style>body{font-family:monospace;max-width:70ch;margin:4em auto;padding:0 1em}</style>
</head><body>
<h1>StretchStore — no-FUSE mode</h1>
<p>Logical volume: <b>%s</b> &nbsp; Physical: <b>%s</b> / %s (%.1f%%)</p>
<p>Unique chunks: %d &nbsp; Mapped: %s</p>
<p><a href="/disk.img">disk.img</a> — the virtual disk (HTTP Range reads, PUT writes)</p>
<p><a href="/api/status">api/status</a> — JSON</p>
<p>This is the fallback file browser for machines without FUSE
(Windows builds, Android/Termux, minimal containers). Reads are served
straight from the deduplicated store; holes read as zeros. When the
physical cap fills, writes fail with <b>507 Insufficient Storage</b> —
the same honest refusal as ENOSPC on a FUSE mount.</p>
</body></html>`,
		size.Format(st.LogicalSize), size.Format(st.PhysicalUsed), size.Format(st.PhysicalLimit),
		100*float64(st.PhysicalUsed)/float64(st.PhysicalLimit),
		st.UniqueChunks, size.Format(st.LogicalMapped))
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s.st.Status())
}

func (s *Server) handleDisk(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET", "HEAD":
		davHeaders(w)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "disk.img", time.Now(), &diskReader{st: s.st})
	case "PUT":
		s.handlePut(w, r)
	case "OPTIONS":
		davHeaders(w)
		w.WriteHeader(http.StatusOK)
	case "PROPFIND":
		s.handlePropfind(w, r)
	default:
		w.Header().Set("Allow", "OPTIONS, GET, HEAD, PUT, PROPFIND")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handlePut(w http.ResponseWriter, r *http.Request) {
	davHeaders(w)
	off := int64(0)
	if q := r.URL.Query().Get("offset"); q != "" {
		v, err := strconv.ParseInt(q, 10, 64)
		if err != nil || v < 0 {
			http.Error(w, "bad offset", http.StatusBadRequest)
			return
		}
		off = v
	}
	if r.ContentLength > int64(s.st.Status().LogicalSize)-off {
		http.Error(w, "write exceeds logical volume", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	wr := &offsetWriter{st: s.st, off: off}
	n, err := io.CopyBuffer(wr, r.Body, make([]byte, 1<<20))
	if err != nil {
		if errors.Is(err, store.ErrNoSpace) {
			http.Error(w, "physical store full: write refused", http.StatusInsufficientStorage)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "wrote %d bytes at offset %d\n", n, off)
}

func (s *Server) handlePropfind(w http.ResponseWriter, r *http.Request) {
	davHeaders(w)
	depth := r.Header.Get("Depth")
	st := s.st.Status()
	sz := st.LogicalSize
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.WriteHeader(207) // Multi-Status
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="utf-8"?><D:multistatus xmlns:D="DAV:">`)
	prop := func(href, name string, size uint64, isDir bool) {
		typ := ""
		if isDir {
			typ = `<D:resourcetype><D:collection/></D:resourcetype>`
		} else {
			typ = `<D:resourcetype/>`
		}
		fmt.Fprintf(&b, `<D:response><D:href>%s</D:href><D:propstat><D:prop>`+
			`<D:displayname>%s</D:displayname>%s<D:getcontentlength>%d</D:getcontentlength>`+
			`</D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`,
			href, name, typ, size)
	}
	prop("/", "", 0, true)
	if depth != "0" {
		prop("/disk.img", "disk.img", sz, false)
	}
	b.WriteString(`</D:multistatus>`)
	w.Write([]byte(b.String()))
}

func (s *Server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/disk.img", s.handleDisk)
	return mux
}

// Serve runs the fallback file server until ctx ends. addr is typically
// 127.0.0.1:8080 — localhost only.
func Serve(ctx context.Context, st *store.Store, addr string) error {
	s := &Server{st: st}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	fmt.Printf("serving volume on http://%s  (disk.img via HTTP/WebDAV, Ctrl-C to stop)\n", ln.Addr())
	srv := &http.Server{Handler: s.routes()}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	err = srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
