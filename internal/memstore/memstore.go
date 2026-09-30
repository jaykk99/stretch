// Package memstore is a compressed, deduplicated in-memory object store —
// the userspace cousin of zram + KSM.
//
// Segments are the allocation unit (think: a VM's RAM, a cache region).
// Each segment is split into 4KB pages. Every page is SHA-256 addressed:
// identical pages across all segments are stored exactly once, and each
// unique page is zstd-compressed. Zero pages cost nothing (unmapped holes).
//
// The physics, stated plainly: typical mixed data compresses ~2-4:1 (this
// is the range zram achieves in production). Zero-filled pages collapse to
// ~nothing. Random/incompressible pages are ~1:1 — sometimes a hair worse,
// because every 4KB page pays a few bytes of framing. The physical cap is a
// hard wall: writes that don't fit are refused, never silently dropped.
package memstore

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/klauspost/compress/zstd"
)

// PageSize matches the OS/KSM page size: dedup granularity is one page.
const PageSize = 4096

// pageOverhead is the honest per-unique-page bookkeeping estimate (map
// entries, hashes, slice headers) counted against the physical cap.
const pageOverhead = 64

// ErrNoSpace is returned when a write would exceed the physical cap.
var ErrNoSpace = errors.New("memory store full: write refused, existing data intact")

type page struct {
	comp []byte // zstd-compressed page bytes
	refs int
}

type segment struct {
	size  uint64
	pages []string // page index -> sha256 hex, "" = zero hole
}

// Stats is the honest accounting snapshot.
type Stats struct {
	Segments     int
	LogicalBytes uint64 // sum of segment sizes
	MappedBytes  uint64 // bytes backed by real pages
	PhysicalUsed uint64 // compressed bytes + bookkeeping
	PhysicalCap  uint64
	UniquePages  int
	PageRefs     int
}

// Ratio returns mapped : physical as a float (2.5 means 2.5:1).
func (s Stats) Ratio() float64 {
	if s.PhysicalUsed == 0 {
		return 0
	}
	return float64(s.MappedBytes) / float64(s.PhysicalUsed)
}

type Store struct {
	mu       sync.Mutex
	cap      uint64
	pages    map[string]*page
	segs     map[uint64]*segment
	nextID   uint64
	physUsed uint64
	enc      *zstd.Encoder
	dec      *zstd.Decoder
}

// New creates an empty store with a hard physical cap in bytes.
func New(capBytes uint64) (*Store, error) {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	return &Store{
		cap:   capBytes,
		pages: make(map[string]*page),
		segs:  make(map[uint64]*segment),
		enc:   enc,
		dec:   dec,
	}, nil
}

// Alloc creates a segment of size bytes. Segments start as pure holes (zeros)
// and consume no physical memory.
func (s *Store) Alloc(size uint64) uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nextID++
	id := s.nextID
	n := (size + PageSize - 1) / PageSize
	s.segs[id] = &segment{size: size, pages: make([]string, n)}
	return id
}

// Free releases a segment, dereferencing its pages.
func (s *Store) Free(id uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("no such segment %d", id)
	}
	for _, h := range seg.pages {
		if h == "" {
			continue
		}
		if p := s.pages[h]; p != nil {
			p.refs--
			if p.refs <= 0 {
				s.physUsed -= uint64(len(p.comp) + pageOverhead)
				delete(s.pages, h)
			}
		}
	}
	delete(s.segs, id)
	return nil
}

func isZeros(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// pageBytes reconstructs a segment page's current content.
func (s *Store) pageBytes(seg *segment, idx int) []byte {
	buf := make([]byte, PageSize)
	h := seg.pages[idx]
	if h == "" {
		return buf
	}
	p := s.pages[h]
	if p == nil {
		return buf
	}
	raw, err := s.dec.DecodeAll(p.comp, nil)
	if err != nil || len(raw) != PageSize {
		return buf
	}
	copy(buf, raw)
	return buf
}

// Write stores len(p) bytes at offset off in segment id. All-or-nothing per
// call with respect to the cap: if it doesn't fit, nothing is mutated and
// ErrNoSpace is returned.
func (s *Store) Write(id uint64, p []byte, off uint64) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("no such segment %d", id)
	}
	if off+uint64(len(p)) > seg.size {
		return fmt.Errorf("write out of segment bounds")
	}

	type plan struct {
		idx  int
		hash string
		comp []byte // non-nil when this is a brand-new page
	}
	var plans []plan
	staged := make(map[string][]byte) // hash -> compressed (batch-local dedup)
	var need uint64

	pOff := 0
	firstPage := int(off / PageSize)
	lastPage := int((off + uint64(len(p)) - 1) / PageSize)
	for idx := firstPage; idx <= lastPage; idx++ {
		pgStart := uint64(idx) * PageSize
		cur := s.pageBytes(seg, idx)
		wStart := off
		if wStart < pgStart {
			wStart = pgStart
		}
		wEnd := off + uint64(len(p))
		if wEnd > pgStart+PageSize {
			wEnd = pgStart + PageSize
		}
		copy(cur[wStart-pgStart:], p[pOff:pOff+int(wEnd-wStart)])
		pOff += int(wEnd - wStart)

		if isZeros(cur) {
			plans = append(plans, plan{idx: idx})
			continue
		}
		h := sha256.Sum256(cur)
		key := hex.EncodeToString(h[:])
		pl := plan{idx: idx, hash: key}
		if _, ok := s.pages[key]; !ok {
			if _, ok := staged[key]; !ok {
				comp := s.enc.EncodeAll(cur, nil)
				staged[key] = comp
				need += uint64(len(comp) + pageOverhead)
			}
		}
		plans = append(plans, pl)
	}
	if s.physUsed+need > s.cap {
		return ErrNoSpace
	}
	for key, comp := range staged {
		s.pages[key] = &page{comp: comp}
		s.physUsed += uint64(len(comp) + pageOverhead)
	}
	for _, pl := range plans {
		old := seg.pages[pl.idx]
		if old == pl.hash {
			continue
		}
		if old != "" {
			if pg := s.pages[old]; pg != nil {
				pg.refs--
				if pg.refs <= 0 {
					s.physUsed -= uint64(len(pg.comp) + pageOverhead)
					delete(s.pages, old)
				}
			}
		}
		seg.pages[pl.idx] = pl.hash
		if pl.hash != "" {
			s.pages[pl.hash].refs++
		}
	}
	return nil
}

// Read reads len(p) bytes at offset off from segment id. Holes read as zeros.
func (s *Store) Read(id uint64, p []byte, off uint64) error {
	if len(p) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	seg, ok := s.segs[id]
	if !ok {
		return fmt.Errorf("no such segment %d", id)
	}
	if off+uint64(len(p)) > seg.size {
		return fmt.Errorf("read out of segment bounds")
	}
	pOff := 0
	firstPage := int(off / PageSize)
	lastPage := int((off + uint64(len(p)) - 1) / PageSize)
	for idx := firstPage; idx <= lastPage; idx++ {
		pgStart := uint64(idx) * PageSize
		rStart := off
		if rStart < pgStart {
			rStart = pgStart
		}
		rEnd := off + uint64(len(p))
		if rEnd > pgStart+PageSize {
			rEnd = pgStart + PageSize
		}
		n := int(rEnd - rStart)
		if h := seg.pages[idx]; h != "" {
			if pg := s.pages[h]; pg != nil {
				if raw, err := s.dec.DecodeAll(pg.comp, nil); err == nil && len(raw) == PageSize {
					copy(p[pOff:pOff+n], raw[rStart-pgStart:rStart-pgStart+uint64(n)])
					pOff += n
					continue
				}
			}
		}
		for i := 0; i < n; i++ {
			p[pOff+i] = 0
		}
		pOff += n
	}
	return nil
}

// Stats returns the honest accounting snapshot.
func (s *Store) Stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := Stats{
		Segments:     len(s.segs),
		PhysicalUsed: s.physUsed,
		PhysicalCap:  s.cap,
		UniquePages:  len(s.pages),
	}
	for _, seg := range s.segs {
		st.LogicalBytes += seg.size
		for _, h := range seg.pages {
			if h != "" {
				st.MappedBytes += PageSize
			}
		}
	}
	for _, pg := range s.pages {
		st.PageRefs += pg.refs
	}
	return st
}
