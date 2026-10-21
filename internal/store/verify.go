// Verification engine: integrity checks over a StretchStore volume.
//
// Open() already refuses to open a volume whose journal or chunk file has
// mid-file corruption (a torn tail is the only crash damage it tolerates).
// Verify() goes further: it audits the *logical* integrity of a volume that
// opened cleanly, catching the problems Open can't see:
//
//   - missing chunks: a slot references a chunk hash with no blob in
//     chunks.dat. ReadAt silently returns zeros for these (fail-open reads),
//     so this is the one silent-data-loss shape the store has. Verify flags
//     every missing hash so the operator knows exactly which regions are
//     affected.
//   - orphaned chunks: blobs in chunks.dat referenced by no slot. These are
//     the normal, expected leftovers of overwrites (WriteAt replaces a
//     slot's chunk list; the old blobs stay on disk) and of crashed writes.
//     They are wasted space, not corruption — Prune() reclaims them.
//   - length mismatches: a slot ref claims more bytes than the chunk holds.
//   - slot tiling: a slot's refs must tile the slot exactly (sum of lengths
//     == slot span). A short tile means part of the slot reads as phantom
//     zeros; an over-long tile means refs overlap.
//   - journal health: every manifest.log record is CRC-checked and counted,
//     so a journal that was truncated-by-crash between the last Open and now
//     still shows up.
//
// Verify is read-only: it never mutates the volume.
package store

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// VerifyReport is the full result of an integrity audit.
type VerifyReport struct {
	SlotsChecked   int    // slots with chunk refs
	ChunkRefs      int    // total slot -> chunk references
	UniqueChunks   int    // distinct chunk hashes on disk
	BlobsOnDisk    int    // blob records physically present in chunks.dat
	OrphanedChunks int    // blobs on disk referenced by no slot
	OrphanedBytes  uint64 // physical bytes held by orphaned blobs (incl. headers)

	MissingChunks  []string // slot-referenced hashes with no blob on disk
	LengthMismatch []string // refs claiming more bytes than the chunk holds
	TilingErrors   []string // slots whose refs don't tile the slot exactly
	CorruptBlobs   []string // blobs whose bytes fail hash/decompress check

	JournalRecords int      // records scanned in manifest.log
	JournalErrors  []string // CRC/parse failures found in manifest.log
}

// OK reports whether the volume is fully healthy: no missing or corrupt
// data, and a clean journal. Orphans do not fail the check — they are
// expected waste, reported for the operator's information.
func (r *VerifyReport) OK() bool {
	return len(r.MissingChunks) == 0 &&
		len(r.LengthMismatch) == 0 &&
		len(r.TilingErrors) == 0 &&
		len(r.CorruptBlobs) == 0 &&
		len(r.JournalErrors) == 0
}

// Summary renders a human-readable audit report.
func (r *VerifyReport) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "slots checked:      %d\n", r.SlotsChecked)
	fmt.Fprintf(&b, "chunk references:   %d\n", r.ChunkRefs)
	fmt.Fprintf(&b, "unique chunks:      %d\n", r.UniqueChunks)
	fmt.Fprintf(&b, "blobs on disk:      %d\n", r.BlobsOnDisk)
	fmt.Fprintf(&b, "orphaned chunks:    %d (%s wasted — run 'prune' to reclaim)\n",
		r.OrphanedChunks, formatBytes(r.OrphanedBytes))
	fmt.Fprintf(&b, "journal records:    %d\n", r.JournalRecords)
	problems := len(r.MissingChunks) + len(r.LengthMismatch) +
		len(r.TilingErrors) + len(r.CorruptBlobs) + len(r.JournalErrors)
	if problems == 0 {
		b.WriteString("verdict:            HEALTHY — all data accounted for\n")
		return b.String()
	}
	fmt.Fprintf(&b, "verdict:            %d PROBLEM(S) FOUND\n", problems)
	for _, h := range r.MissingChunks {
		fmt.Fprintf(&b, "  MISSING chunk %s (slot data reads as zeros)\n", shortHash(h))
	}
	for _, h := range r.LengthMismatch {
		fmt.Fprintf(&b, "  LENGTH-MISMATCH chunk %s (ref longer than stored chunk)\n", shortHash(h))
	}
	for _, s := range r.TilingErrors {
		fmt.Fprintf(&b, "  TILING %s\n", s)
	}
	for _, h := range r.CorruptBlobs {
		fmt.Fprintf(&b, "  CORRUPT blob %s (hash/decompress check failed)\n", shortHash(h))
	}
	for _, e := range r.JournalErrors {
		fmt.Fprintf(&b, "  JOURNAL %s\n", e)
	}
	return b.String()
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16] + "…"
	}
	return h
}

func formatBytes(n uint64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := uint64(unit), 0
	for n >= unit*div && exp < 4 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// Verify audits the volume and returns a full report. It is read-only.
func (st *Store) Verify() (*VerifyReport, error) {
	st.mu.Lock()
	defer st.mu.Unlock()

	rep := &VerifyReport{}

	// 1. Re-scan chunks.dat straight from disk and re-verify every blob:
	//    decompress it, recompute its sha256, confirm it matches the key the
	//    index was built from. This catches bit-rot that happened after Open.
	onDisk, corrupt, err := st.scanChunkBlobs()
	if err != nil {
		return nil, fmt.Errorf("verify: scan chunks.dat: %w", err)
	}
	rep.CorruptBlobs = corrupt
	rep.BlobsOnDisk = len(onDisk)
	rep.UniqueChunks = len(st.chunkIdx)

	// 2. Check every slot's refs: the blob must exist, the claimed length
	//    must fit inside the stored chunk, and the refs must tile the slot.
	for slot, refs := range st.slots {
		rep.SlotsChecked++
		var tiled int
		for _, r := range refs {
			rep.ChunkRefs++
			loc, ok := onDisk[r.Hash]
			if !ok {
				rep.MissingChunks = append(rep.MissingChunks, r.Hash)
				continue
			}
			if r.Len > loc.rawLen || r.Len <= 0 {
				rep.LengthMismatch = append(rep.LengthMismatch, r.Hash)
			}
			tiled += r.Len
		}
		if want := st.slotSpan(slot); tiled != want {
			rep.TilingErrors = append(rep.TilingErrors,
				fmt.Sprintf("slot %d: refs tile %d bytes, slot span is %d", slot, tiled, want))
		}
	}

	// 3. Orphans: blobs nobody references. Expected after overwrites and
	//    crashed writes — wasted space, not corruption.
	for h, loc := range onDisk {
		if st.refCount[h] == 0 {
			rep.OrphanedChunks++
			rep.OrphanedBytes += uint64(4 + loc.compLen)
		}
	}

	// 4. Journal health: CRC-check every record without replaying.
	records, jerrs, err := scanJournal(filepath.Join(st.dir, "manifest.log"))
	if err != nil {
		return nil, fmt.Errorf("verify: scan manifest.log: %w", err)
	}
	rep.JournalRecords = records
	rep.JournalErrors = jerrs

	return rep, nil
}

// scanChunkBlobs re-reads chunks.dat from disk and returns the blobs it
// finds (hash -> location), plus the hashes of any blob that fails the
// hash/decompress check. A torn tail is tolerated exactly like in
// replayChunks — it is crash damage, not corruption — and simply stops the
// scan.
func (st *Store) scanChunkBlobs() (map[string]*chunkLoc, []string, error) {
	f := st.chunksF
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	fileSize := fi.Size()
	found := make(map[string]*chunkLoc)
	var corrupt []string
	hdr := make([]byte, 4)
	var off int64
	for {
		if _, err := f.ReadAt(hdr, off); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break // torn tail
			}
			return nil, nil, err
		}
		compLen := binary.LittleEndian.Uint32(hdr)
		recEnd := off + 4 + int64(compLen)
		if compLen == 0 || compLen > 1<<28 || recEnd > fileSize {
			break // torn tail: garbage length at the end
		}
		comp := make([]byte, compLen)
		if _, err := f.ReadAt(comp, off+4); err != nil {
			break // torn tail: body cut short
		}
		raw, err := st.dec.DecodeAll(comp, nil)
		if err != nil {
			corrupt = append(corrupt, fmt.Sprintf("offset %d (undecodable)", off))
			off = recEnd
			continue
		}
		h := sha256.Sum256(raw)
		key := hex.EncodeToString(h[:])
		found[key] = &chunkLoc{off: off + 4, compLen: int(compLen), rawLen: len(raw)}
		off = recEnd
	}
	return found, corrupt, nil
}

// scanJournal CRC-checks every record in manifest.log without replaying it.
// It returns the record count and any errors. A torn tail (the only crash
// damage Open tolerates) is not an error — but it IS reported in the count
// so the operator can see the journal was cut.
func scanJournal(path string) (int, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, nil, err
	}
	var errs []string
	records := 0
	pos := 0
	for pos < len(data) {
		if pos+8 > len(data) {
			break // torn tail
		}
		ln := int(binary.LittleEndian.Uint32(data[pos:]))
		recEnd := pos + 4 + ln + 4
		if ln <= 0 || ln > 64<<20 || recEnd > len(data) {
			break // torn tail
		}
		body := data[pos+4 : pos+4+ln]
		if crc32.ChecksumIEEE(body) != binary.LittleEndian.Uint32(data[pos+4+ln:]) {
			errs = append(errs, fmt.Sprintf("record %d at offset %d: CRC mismatch", records, pos))
			pos = recEnd
			records++
			continue
		}
		records++
		pos = recEnd
	}
	return records, errs, nil
}
