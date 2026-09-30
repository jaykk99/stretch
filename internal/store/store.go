// Package store is the StretchStore engine: a thin-provisioned, compressed,
// deduplicated block volume backed by a capped physical directory.
//
// Layout inside the volume directory:
//
//	config.json  – logical/physical sizes, format version
//	chunks.dat   – append-only blob file: [u32 LE compLen][zstd bytes]...
//	manifest.log – append-only journal: [u32 LE len][json record][u32 LE crc32]
//
// Crash safety: every mutation appends chunk blobs first, then journal
// records, then fsyncs chunks.dat and manifest.log (in that order) before
// acknowledging the write. A torn tail record (short read or CRC mismatch)
// is truncated on open; anything after the last good record was never
// acknowledged, so dropping it is correct. Chunk blobs orphaned by a crash
// are simply never referenced — wasted space, never corruption.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/klauspost/compress/zstd"

	"stretchstore/internal/cdc"
)

// SlotSize is the logical granularity at which writes are re-chunked.
// A 1MB slot keeps partial-write re-chunking cheap while staying far below
// the 1000GB logical address space (1M slots max).
const SlotSize = 1 << 20

// ErrNoSpace is returned when a write would exceed the physical limit.
// The write is refused *before* anything is mutated: data already stored
// stays intact and readable.
var ErrNoSpace = errors.New("physical store full: write refused, existing data intact")

// Config is persisted as config.json at volume creation.
type Config struct {
	Version      int    `json:"version"`
	LogicalSize  uint64 `json:"logical_size"`
	PhysicalSize uint64 `json:"physical_size"`
}

type chunkRef struct {
	Hash string `json:"h"`
	Len  int    `json:"n"` // uncompressed length
}

// journal records
type recSlot struct {
	T string     `json:"t"` // "slot"
	S uint64     `json:"s"`
	C []chunkRef `json:"c"` // nil/empty = unmap
}
type recSize struct {
	T string `json:"t"` // "size"
	N uint64 `json:"n"`
}
type recHigh struct {
	T string `json:"t"` // "high"
	N uint64 `json:"n"`
}

type chunkLoc struct {
	off     int64
	compLen int
	rawLen  int
}

// Status is the honest accounting snapshot.
type Status struct {
	LogicalSize   uint64 // what we pretend to be (thin)
	FileSize      uint64 // current FUSE-visible file size (<= LogicalSize)
	LogicalMapped uint64 // bytes currently backed by real chunks
	HighWater     uint64 // highest byte ever written
	PhysicalUsed  uint64 // real bytes on disk (chunks + journal)
	PhysicalLimit uint64 // the hard cap; never exceeded
	UniqueChunks  uint64
	ChunkRefs     uint64
}

// Ratio returns logical-mapped : physical-used as a float (e.g. 104.5 means 104.5:1).
// Returns 0 when nothing is stored yet.
func (s Status) Ratio() float64 {
	if s.PhysicalUsed == 0 {
		return 0
	}
	return float64(s.LogicalMapped) / float64(s.PhysicalUsed)
}

type Store struct {
	dir string
	cfg Config

	mu        sync.Mutex
	chunksF   *os.File
	manifestF *os.File

	chunkIdx map[string]*chunkLoc // sha256hex -> location in chunks.dat
	refCount map[string]int
	slots    map[uint64][]chunkRef
	fileSize uint64
	high     uint64
	physUsed uint64

	enc *zstd.Encoder
	dec *zstd.Decoder
}

// Create initializes a fresh volume directory. Nothing is preallocated:
// a brand-new 1000GB volume consumes ~0 physical bytes (thin provisioning).
func Create(dir string, logical, physical uint64) (*Store, error) {
	if logical == 0 || physical == 0 {
		return nil, fmt.Errorf("sizes must be > 0")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	cfgPath := filepath.Join(dir, "config.json")
	if _, err := os.Stat(cfgPath); err == nil {
		return nil, fmt.Errorf("volume already exists at %s", dir)
	}
	cfg := Config{Version: 1, LogicalSize: logical, PhysicalSize: physical}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(cfgPath, data, 0o644); err != nil {
		return nil, err
	}
	return Open(dir)
}

// Open loads an existing volume, replaying the journal and truncating any
// torn tail left by a crash.
func Open(dir string) (*Store, error) {
	cfgData, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil, fmt.Errorf("open volume: %w", err)
	}
	var cfg Config
	if err := json.Unmarshal(cfgData, &cfg); err != nil {
		return nil, fmt.Errorf("bad config.json: %w", err)
	}
	if cfg.Version != 1 {
		return nil, fmt.Errorf("unsupported volume version %d", cfg.Version)
	}
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return nil, err
	}
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return nil, err
	}
	st := &Store{
		dir:      dir,
		cfg:      cfg,
		chunkIdx: make(map[string]*chunkLoc),
		refCount: make(map[string]int),
		slots:    make(map[uint64][]chunkRef),
		fileSize: cfg.LogicalSize,
		enc:      enc,
		dec:      dec,
	}
	chunksPath := filepath.Join(dir, "chunks.dat")
	st.chunksF, err = os.OpenFile(chunksPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	manifestPath := filepath.Join(dir, "manifest.log")
	st.manifestF, err = os.OpenFile(manifestPath, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		st.chunksF.Close()
		return nil, err
	}
	if err := st.replayChunks(); err != nil {
		st.Close()
		return nil, fmt.Errorf("replay chunks.dat: %w", err)
	}
	if err := st.replayManifest(); err != nil {
		st.Close()
		return nil, fmt.Errorf("replay manifest.log: %w", err)
	}
	st.refreshPhysUsed()
	return st, nil
}

// replayChunks scans chunks.dat, rebuilding the chunk index and truncating
// a torn tail record.
func (st *Store) replayChunks() error {
	f := st.chunksF
	var off int64
	hdr := make([]byte, 4)
	for {
		if _, err := f.ReadAt(hdr, off); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			return err
		}
		compLen := binary.LittleEndian.Uint32(hdr)
		if compLen == 0 || compLen > 1<<28 {
			break // torn/garbage tail
		}
		comp := make([]byte, compLen)
		if _, err := f.ReadAt(comp, off+4); err != nil {
			break // torn tail
		}
		raw, err := st.dec.DecodeAll(comp, nil)
		if err != nil {
			break // corrupt tail; stop rather than guess
		}
		h := sha256.Sum256(raw)
		key := hex.EncodeToString(h[:])
		st.chunkIdx[key] = &chunkLoc{off: off + 4, compLen: int(compLen), rawLen: len(raw)}
		off += 4 + int64(compLen)
	}
	// Truncate any torn tail so the file is clean for appends.
	if err := f.Truncate(off); err != nil {
		return err
	}
	_, err := f.Seek(0, io.SeekEnd)
	return err
}

// replayManifest replays journal records, truncating a torn tail.
// Slot records are last-write-wins; refcounts are derived from the final map.
func (st *Store) replayManifest() error {
	f := st.manifestF
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	pos := 0
	good := 0
	for pos+8 <= len(data) {
		ln := int(binary.LittleEndian.Uint32(data[pos:]))
		if ln <= 0 || ln > 64<<20 || pos+4+ln+4 > len(data) {
			break // torn tail
		}
		body := data[pos+4 : pos+4+ln]
		crc := binary.LittleEndian.Uint32(data[pos+4+ln:])
		if crc32.ChecksumIEEE(body) != crc {
			break // torn tail
		}
		var probe struct {
			T string `json:"t"`
		}
		if err := json.Unmarshal(body, &probe); err != nil {
			break
		}
		switch probe.T {
		case "slot":
			var r recSlot
			if json.Unmarshal(body, &r) == nil {
				if len(r.C) == 0 {
					delete(st.slots, r.S)
				} else {
					st.slots[r.S] = r.C
				}
			}
		case "size":
			var r recSize
			if json.Unmarshal(body, &r) == nil {
				st.fileSize = r.N
			}
		case "high":
			var r recHigh
			if json.Unmarshal(body, &r) == nil && r.N > st.high {
				st.high = r.N
			}
		}
		pos += 4 + ln + 4
		good = pos
	}
	if good < len(data) {
		// Torn tail: cut it. Everything after the last good record was
		// never acknowledged to any writer.
		if err := f.Truncate(int64(good)); err != nil {
			return err
		}
	}
	for _, refs := range st.slots {
		for _, r := range refs {
			st.refCount[r.Hash]++
		}
	}
	_, err = f.Seek(0, io.SeekEnd)
	return err
}

func (st *Store) refreshPhysUsed() {
	var total uint64
	for _, p := range []string{"chunks.dat", "manifest.log", "config.json"} {
		if fi, err := os.Stat(filepath.Join(st.dir, p)); err == nil {
			total += uint64(fi.Size())
		}
	}
	st.physUsed = total
}

// appendRecord writes one length-prefixed, CRC-protected journal record.
// Caller must fsync the manifest after the batch.
func (st *Store) appendRecord(v any) (int, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	var buf [8]byte
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(buf[4:8], crc32.ChecksumIEEE(body))
	n := 0
	for _, b := range [][]byte{buf[0:4], body, buf[4:8]} {
		m, err := st.manifestF.Write(b)
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// slotSpan returns the byte length of a slot (last slot may be short).
func (st *Store) slotSpan(slot uint64) int {
	start := slot * SlotSize
	if start >= st.cfg.LogicalSize {
		return 0
	}
	end := start + SlotSize
	if end > st.cfg.LogicalSize {
		end = st.cfg.LogicalSize
	}
	return int(end - start)
}

// readSlot reconstructs a slot's current bytes (zeros for unwritten regions).
func (st *Store) readSlot(slot uint64) []byte {
	span := st.slotSpan(slot)
	buf := make([]byte, span)
	refs, ok := st.slots[slot]
	if !ok {
		return buf
	}
	pos := 0
	for _, r := range refs {
		loc, ok := st.chunkIdx[r.Hash]
		// r.Len is the valid prefix length, always <= the stored raw length.
		if !ok || loc == nil || pos+r.Len > span {
			// Should never happen on a healthy store; treat as zeros
			// rather than failing the read.
			pos += r.Len
			continue
		}
		comp := make([]byte, loc.compLen)
		if _, err := st.chunksF.ReadAt(comp, loc.off); err != nil {
			pos += r.Len
			continue
		}
		raw, err := st.dec.DecodeAll(comp, nil)
		if err != nil || len(raw) < r.Len {
			pos += r.Len
			continue
		}
		copy(buf[pos:], raw[:r.Len])
		pos += r.Len
	}
	return buf
}

func isZeros(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

// WriteAt stores len(p) bytes at offset off. It is all-or-nothing per call:
// if the physical limit would be exceeded, nothing is written and ErrNoSpace
// is returned.
func (st *Store) WriteAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || uint64(off)+uint64(len(p)) > st.cfg.LogicalSize {
		return 0, fmt.Errorf("write out of range: off=%d len=%d logical=%d", off, len(p), st.cfg.LogicalSize)
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	uoff := uint64(off)
	end := uoff + uint64(len(p))
	firstSlot := uoff / SlotSize
	lastSlot := (end - 1) / SlotSize

	type slotPlan struct {
		slot   uint64
		chunks []chunkRef
	}
	var plans []slotPlan
	// chunkKey -> compressed bytes for blobs not yet in chunkIdx (batch-local dedup)
	staged := make(map[string][]byte)
	stagedRaw := make(map[string]int)

	pOff := uint64(0)
	for s := firstSlot; s <= lastSlot; s++ {
		sStart := s * SlotSize
		cur := st.readSlot(s)
		// splice the overlapping portion of p into cur
		wStart := uoff
		if wStart < sStart {
			wStart = sStart
		}
		wEnd := end
		if wEnd > sStart+uint64(len(cur)) {
			wEnd = sStart + uint64(len(cur))
		}
		copy(cur[wStart-sStart:], p[pOff:pOff+(wEnd-wStart)])
		pOff += wEnd - wStart

		if isZeros(cur) {
			plans = append(plans, slotPlan{slot: s, chunks: nil}) // unmap
			continue
		}
		var refs []chunkRef
		for _, c := range cdc.Chunk(cur) {
			h := sha256.Sum256(c)
			key := hex.EncodeToString(h[:])
			if _, ok := st.chunkIdx[key]; !ok {
				if _, ok := staged[key]; !ok {
					staged[key] = st.enc.EncodeAll(c, nil)
					stagedRaw[key] = len(c)
				}
			}
			refs = append(refs, chunkRef{Hash: key, Len: len(c)})
		}
		plans = append(plans, slotPlan{slot: s, chunks: refs})
	}

	// Physical budget check BEFORE mutating anything.
	var newBytes uint64
	for _, comp := range staged {
		newBytes += uint64(4 + len(comp))
	}
	// Estimate manifest growth: marshal each slot record once to measure.
	var manBytes uint64
	for _, pl := range plans {
		rec := recSlot{T: "slot", S: pl.slot, C: pl.chunks}
		body, _ := json.Marshal(rec)
		manBytes += uint64(4 + len(body) + 4)
	}
	manBytes += 12 // possible "high" record
	if st.physUsed+newBytes+manBytes > st.cfg.PhysicalSize {
		return 0, ErrNoSpace
	}

	// Commit: chunk blobs first, then journal, fsync both, then ack.
	for key, comp := range staged {
		var hdr [4]byte
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(comp)))
		if _, err := st.chunksF.Write(hdr[:]); err != nil {
			return 0, fmt.Errorf("chunk write: %w", err)
		}
		off, _ := st.chunksF.Seek(0, io.SeekCurrent)
		if _, err := st.chunksF.Write(comp); err != nil {
			return 0, fmt.Errorf("chunk write: %w", err)
		}
		st.chunkIdx[key] = &chunkLoc{off: off, compLen: len(comp), rawLen: stagedRaw[key]}
	}
	for _, pl := range plans {
		old := st.slots[pl.slot]
		rec := recSlot{T: "slot", S: pl.slot, C: pl.chunks}
		if _, err := st.appendRecord(rec); err != nil {
			return 0, fmt.Errorf("journal write: %w", err)
		}
		for _, r := range old {
			st.refCount[r.Hash]--
		}
		if len(pl.chunks) == 0 {
			delete(st.slots, pl.slot)
		} else {
			st.slots[pl.slot] = pl.chunks
			for _, r := range pl.chunks {
				st.refCount[r.Hash]++
			}
		}
	}
	if end > st.high {
		st.high = end
		st.appendRecord(recHigh{T: "high", N: st.high})
	}
	if err := st.chunksF.Sync(); err != nil {
		return 0, err
	}
	if err := st.manifestF.Sync(); err != nil {
		return 0, err
	}
	st.refreshPhysUsed()
	return len(p), nil
}

// ReadAt reads len(p) bytes at offset off. Unwritten regions read as zeros.
func (st *Store) ReadAt(p []byte, off int64) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if off < 0 || uint64(off)+uint64(len(p)) > st.fileSize {
		return 0, fmt.Errorf("read out of range")
	}
	st.mu.Lock()
	defer st.mu.Unlock()

	uoff := uint64(off)
	end := uoff + uint64(len(p))
	firstSlot := uoff / SlotSize
	lastSlot := (end - 1) / SlotSize
	pOff := 0
	for s := firstSlot; s <= lastSlot; s++ {
		sStart := s * SlotSize
		refs, ok := st.slots[s]
		rStart := uoff
		if rStart < sStart {
			rStart = sStart
		}
		rEnd := end
		if rEnd > sStart+SlotSize {
			rEnd = sStart + SlotSize
		}
		need := int(rEnd - rStart)
		if !ok {
			for i := 0; i < need; i++ {
				p[pOff+i] = 0
			}
			pOff += need
			continue
		}
		// walk chunks to cover [rStart, rEnd)
		pos := sStart
		dst := pOff
		for _, r := range refs {
			cStart, cEnd := pos, pos+uint64(r.Len)
			if cEnd > rStart && cStart < rEnd {
				oStart := rStart
				if oStart < cStart {
					oStart = cStart
				}
				oEnd := rEnd
				if oEnd > cEnd {
					oEnd = cEnd
				}
				loc, ok := st.chunkIdx[r.Hash]
				if !ok || loc == nil {
					return dst, fmt.Errorf("chunk missing: %s", r.Hash)
				}
				comp := make([]byte, loc.compLen)
				if _, err := st.chunksF.ReadAt(comp, loc.off); err != nil {
					return dst, fmt.Errorf("chunk read: %w", err)
				}
				raw, err := st.dec.DecodeAll(comp, nil)
				if err != nil || len(raw) < r.Len {
					return dst, fmt.Errorf("chunk decompress: %w", err)
				}
				copy(p[dst:dst+int(oEnd-oStart)], raw[oStart-cStart:oEnd-cStart])
				dst += int(oEnd - oStart)
			}
			pos = cEnd
		}
		pOff += need
	}
	return len(p), nil
}

// Truncate resizes the FUSE-visible file. Shrinking unmaps and dereferences
// chunks beyond the new size; growing just extends the zero hole.
func (st *Store) Truncate(newSize uint64) error {
	if newSize > st.cfg.LogicalSize {
		return fmt.Errorf("truncate beyond logical size")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if newSize == st.fileSize {
		return nil
	}
	if newSize < st.fileSize {
		firstDead := newSize / SlotSize
		// partial slot: keep the head by trimming the last chunk reference.
		// (Refs may use a prefix of a stored chunk: r.Len <= rawLen always.)
		if rem := newSize % SlotSize; rem != 0 {
			if refs, ok := st.slots[firstDead]; ok {
				var newRefs []chunkRef
				var covered uint64
				for _, r := range refs {
					if covered >= rem {
						break
					}
					if covered+uint64(r.Len) <= rem {
						newRefs = append(newRefs, r)
						covered += uint64(r.Len)
					} else {
						newRefs = append(newRefs, chunkRef{Hash: r.Hash, Len: int(rem - covered)})
						covered = rem
					}
				}
				st.appendRecord(recSlot{T: "slot", S: firstDead, C: newRefs})
				for _, r := range refs {
					st.refCount[r.Hash]--
				}
				if len(newRefs) == 0 {
					delete(st.slots, firstDead)
				} else {
					st.slots[firstDead] = newRefs
					for _, r := range newRefs {
						st.refCount[r.Hash]++
					}
				}
			}
			firstDead++
		}
		for s := firstDead; ; s++ {
			if s*SlotSize >= st.cfg.LogicalSize {
				break
			}
			if refs, ok := st.slots[s]; ok {
				st.appendRecord(recSlot{T: "slot", S: s})
				for _, r := range refs {
					st.refCount[r.Hash]--
				}
				delete(st.slots, s)
			}
			if (s+1)*SlotSize >= st.fileSize {
				break
			}
		}
	}
	st.fileSize = newSize
	st.appendRecord(recSize{T: "size", N: newSize})
	if err := st.manifestF.Sync(); err != nil {
		return err
	}
	st.refreshPhysUsed()
	return nil
}

// Status returns the honest accounting snapshot.
func (st *Store) Status() Status {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.refreshPhysUsed()
	var mapped uint64
	for s, refs := range st.slots {
		span := uint64(st.slotSpan(s))
		var n uint64
		for _, r := range refs {
			n += uint64(r.Len)
		}
		if n > span {
			n = span
		}
		mapped += n
	}
	return Status{
		LogicalSize:   st.cfg.LogicalSize,
		FileSize:      st.fileSize,
		LogicalMapped: mapped,
		HighWater:     st.high,
		PhysicalUsed:  st.physUsed,
		PhysicalLimit: st.cfg.PhysicalSize,
		UniqueChunks:  uint64(len(st.chunkIdx)),
		ChunkRefs:     uint64(sumRefs(st.refCount)),
	}
}

func sumRefs(m map[string]int) int {
	t := 0
	for _, v := range m {
		t += v
	}
	return t
}

// Close flushes and closes the volume.
func (st *Store) Close() error {
	st.mu.Lock()
	defer st.mu.Unlock()
	var e1, e2 error
	if st.chunksF != nil {
		st.chunksF.Sync()
		e1 = st.chunksF.Close()
	}
	if st.manifestF != nil {
		st.manifestF.Sync()
		e2 = st.manifestF.Close()
	}
	if e1 != nil {
		return e1
	}
	return e2
}
