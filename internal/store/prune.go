// Garbage collection: reclaim physical space held by orphaned chunks.
//
// Every overwrite of a slot replaces that slot's chunk list, and every
// crashed write can leave staged blobs behind. In both cases the blobs stay
// in chunks.dat while no slot references them — Verify() reports these as
// orphans. They are pure waste: unreferenced data can never be read back.
//
// Prune() compacts chunks.dat down to exactly the referenced chunks:
//
//  1. Under the store lock, collect the referenced chunk hashes.
//  2. Stream every referenced blob into chunks.dat.new (referenced chunks
//     only, in old-file order so the rewrite is one sequential pass).
//  3. fsync the new file, close the old handle, atomically rename the new
//     file over chunks.dat, reopen it, and swap the in-memory index.
//
// Crash safety: the old chunks.dat is untouched until the atomic rename, so
// a crash at any point before the rename leaves the original file — and the
// unmodified manifest.log, which references chunks by hash, not offset —
// fully valid. After the rename, the new file is complete and fsynced. The
// manifest journal is never touched: it only ever names chunk hashes.
//
// Prune does not touch the logical data at all: every slot's refs keep
// pointing at the same hashes, which are all still present afterwards.
package store

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
)

// PruneReport describes what a garbage-collection pass reclaimed.
type PruneReport struct {
	BlobsKept     int    // referenced blobs carried into the new file
	ChunksRemoved int    // orphaned blobs dropped
	BytesBefore   uint64 // chunks.dat size before
	BytesAfter    uint64 // chunks.dat size after
	BytesFreed    uint64 // BytesBefore - BytesAfter
}

// Prune compacts chunks.dat to the referenced chunks and returns a report.
func (st *Store) Prune() (*PruneReport, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	chunksPath := filepath.Join(st.dir, "chunks.dat")
	tmpPath := filepath.Join(st.dir, "chunks.dat.new")

	before, err := fileSize(chunksPath)
	if err != nil {
		return nil, fmt.Errorf("prune: stat chunks.dat: %w", err)
	}

	// Referenced chunks, in old-file order: one sequential read pass.
	type item struct {
		hash string
		loc  *chunkLoc
	}
	oldCount := len(st.chunkIdx)
	var live []item
	for h, loc := range st.chunkIdx {
		if st.refCount[h] > 0 {
			live = append(live, item{h, loc})
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].loc.off < live[j].loc.off })

	tmp, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		if os.IsExist(err) {
			return nil, fmt.Errorf("prune: stale %s from a crashed prune — remove it and retry", tmpPath)
		}
		return nil, fmt.Errorf("prune: create temp file: %w", err)
	}
	// On any failure below, remove the temp file so the next Prune starts clean.
	failed := true
	defer func() {
		tmp.Close()
		if failed {
			os.Remove(tmpPath)
		}
	}()

	newIdx := make(map[string]*chunkLoc, len(live))
	var hdr [4]byte
	for _, it := range live {
		comp := make([]byte, it.loc.compLen)
		if _, err := st.chunksF.ReadAt(comp, it.loc.off); err != nil {
			return nil, fmt.Errorf("prune: read blob %s: %w", shortHash(it.hash), err)
		}
		binary.LittleEndian.PutUint32(hdr[:], uint32(len(comp)))
		if _, err := tmp.Write(hdr[:]); err != nil {
			return nil, fmt.Errorf("prune: write temp: %w", err)
		}
		bodyOff, _ := tmp.Seek(0, io.SeekCurrent)
		if _, err := tmp.Write(comp); err != nil {
			return nil, fmt.Errorf("prune: write temp: %w", err)
		}
		newIdx[it.hash] = &chunkLoc{off: bodyOff, compLen: len(comp), rawLen: it.loc.rawLen}
	}
	if err := tmp.Sync(); err != nil {
		return nil, fmt.Errorf("prune: sync temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return nil, fmt.Errorf("prune: close temp: %w", err)
	}

	// Atomic swap: the old file stays valid until this rename completes.
	st.chunksF.Close()
	if err := os.Rename(tmpPath, chunksPath); err != nil {
		// The old handle is closed but the old file is intact; reopen it so
		// the store stays usable, then report the failure.
		if fh, oerr := os.OpenFile(chunksPath, os.O_RDWR, 0o644); oerr == nil {
			st.chunksF = fh
		}
		return nil, fmt.Errorf("prune: rename: %w", err)
	}
	fh, err := os.OpenFile(chunksPath, os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("prune: reopen chunks.dat: %w", err)
	}
	if _, err := fh.Seek(0, io.SeekEnd); err != nil {
		fh.Close()
		return nil, fmt.Errorf("prune: seek: %w", err)
	}
	failed = false
	st.chunksF = fh
	st.chunkIdx = newIdx
	st.refreshPhysUsed()

	after, err := fileSize(chunksPath)
	if err != nil {
		return nil, fmt.Errorf("prune: stat new chunks.dat: %w", err)
	}
	return &PruneReport{
		BlobsKept:     len(live),
		ChunksRemoved: oldCount - len(newIdx),
		BytesBefore:   before,
		BytesAfter:    after,
		BytesFreed:    before - after,
	}, nil
}

func fileSize(path string) (uint64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return uint64(fi.Size()), nil
}
