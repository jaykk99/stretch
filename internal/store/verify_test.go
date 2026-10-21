package store

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"hash/crc32"
	"os"
	"path/filepath"
	"testing"
)

func testVolume(t *testing.T, logical, physical uint64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Create(filepath.Join(dir, "vol"), logical, physical)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st, filepath.Join(dir, "vol")
}

func mustWrite(t *testing.T, st *Store, p []byte, off int64) {
	t.Helper()
	if _, err := st.WriteAt(p, off); err != nil {
		t.Fatalf("WriteAt(off=%d, len=%d): %v", off, len(p), err)
	}
}

func mustRead(t *testing.T, st *Store, off int64, n int) []byte {
	t.Helper()
	buf := make([]byte, n)
	if _, err := st.ReadAt(buf, off); err != nil {
		t.Fatalf("ReadAt(off=%d, n=%d): %v", off, n, err)
	}
	return buf
}

func TestVerifyHealthyVolume(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 64<<20)

	zeros := make([]byte, 4<<20)
	mustWrite(t, st, zeros, 0)

	data := make([]byte, 2<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, data, 4<<20)

	rep, err := st.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("expected healthy volume, got:\n%s", rep.Summary())
	}
	if rep.SlotsChecked == 0 || rep.ChunkRefs == 0 {
		t.Fatalf("expected slots and refs to be checked, got %+v", rep)
	}
	if rep.JournalRecords == 0 {
		t.Fatalf("expected journal records, got 0")
	}
}

func TestVerifyFindsOrphansAfterOverwrite(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 64<<20)

	// Write random data, then overwrite the same region with different
	// random data. The first generation's chunks become orphans.
	a := make([]byte, 2<<20)
	b := make([]byte, 2<<20)
	if _, err := rand.Read(a); err != nil {
		t.Fatal(err)
	}
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, a, 0)
	before := st.Status().PhysicalUsed
	mustWrite(t, st, b, 0)
	if st.Status().PhysicalUsed <= before {
		t.Fatalf("expected physical growth after overwrite of random data")
	}

	rep, err := st.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("orphans are not a failure, got:\n%s", rep.Summary())
	}
	if rep.OrphanedChunks == 0 {
		t.Fatalf("expected orphaned chunks after overwrite, got none")
	}
	if rep.OrphanedBytes == 0 {
		t.Fatalf("expected orphaned bytes > 0")
	}
	// New data must still read back exactly.
	if got := mustRead(t, st, 0, len(b)); !bytes.Equal(got, b) {
		t.Fatalf("data corrupted by overwrite")
	}
}

func TestVerifyDetectsMissingChunk(t *testing.T) {
	st, dir := testVolume(t, 1<<30, 64<<20)

	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, data, 0)

	// Simulate blob loss: drop one referenced chunk from the on-disk index
	// bookkeeping (the slot refs still point at it). Verify must flag it.
	var victim string
	for slot, refs := range st.slots {
		_ = slot
		if len(refs) > 0 {
			victim = refs[0].Hash
			break
		}
	}
	if victim == "" {
		t.Fatalf("no chunk refs to victimize")
	}
	// Remove the blob from the physical file so the disk scan misses it too.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	chunksPath := filepath.Join(dir, "chunks.dat")
	raw, err := os.ReadFile(chunksPath)
	if err != nil {
		t.Fatal(err)
	}
	// Drop the FIRST blob record (header + body) from the file.
	compLen := binary.LittleEndian.Uint32(raw[:4])
	raw = raw[4+compLen:]
	if err := os.WriteFile(chunksPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after blob drop: %v", err)
	}
	defer st2.Close()

	rep, err := st2.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if rep.OK() {
		t.Fatalf("expected MISSING chunk report, got healthy:\n%s", rep.Summary())
	}
	if len(rep.MissingChunks) == 0 {
		t.Fatalf("expected missing chunks listed, got:\n%s", rep.Summary())
	}
}

func TestVerifyDetectsJournalCorruption(t *testing.T) {
	st, dir := testVolume(t, 1<<30, 64<<20)

	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, data, 0)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Flip one byte inside the BODY of the first record (CRC will fail).
	// Open would refuse this volume — so exercise the journal scanner
	// directly, which is what Verify uses on an already-open store.
	manPath := filepath.Join(dir, "manifest.log")
	raw, err := os.ReadFile(manPath)
	if err != nil {
		t.Fatal(err)
	}
	ln := int(binary.LittleEndian.Uint32(raw[:4]))
	if ln <= 8 || len(raw) < 4+ln+4 {
		t.Fatalf("manifest too short to corrupt a record body")
	}
	raw[4+ln/2] ^= 0xFF
	if err := os.WriteFile(manPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	records, errs, err := scanJournal(manPath)
	if err != nil {
		t.Fatalf("scanJournal: %v", err)
	}
	if records == 0 {
		t.Fatalf("expected some records scanned")
	}
	if len(errs) == 0 {
		t.Fatalf("expected journal CRC error, got none")
	}
}

func TestVerifyEmptyVolume(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 64<<20)
	rep, err := st.Verify()
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !rep.OK() {
		t.Fatalf("empty volume should verify healthy:\n%s", rep.Summary())
	}
}

func TestScanJournalDetectsBadCRC(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.log")

	body := []byte(`{"t":"size","n":123}`)
	var rec []byte
	var hdr [8]byte
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(len(body)))
	binary.LittleEndian.PutUint32(hdr[4:8], crc32.ChecksumIEEE(body)^0xDEAD) // bad CRC
	rec = append(rec, hdr[0:4]...)
	rec = append(rec, body...)
	rec = append(rec, hdr[4:8]...)
	if err := os.WriteFile(path, rec, 0o644); err != nil {
		t.Fatal(err)
	}

	records, errs, err := scanJournal(path)
	if err != nil {
		t.Fatalf("scanJournal: %v", err)
	}
	if records != 1 {
		t.Fatalf("expected 1 record scanned, got %d", records)
	}
	if len(errs) != 1 {
		t.Fatalf("expected 1 journal error, got %v", errs)
	}
}

func TestVerifySummaryMentionsOrphans(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 64<<20)
	a := make([]byte, 1<<20)
	if _, err := rand.Read(a); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, a, 0)
	mustWrite(t, st, make([]byte, 1<<20), 0) // overwrite -> orphans

	rep, err := st.Verify()
	if err != nil {
		t.Fatal(err)
	}
	s := rep.Summary()
	if !bytes.Contains([]byte(s), []byte("orphaned chunks")) {
		t.Fatalf("summary should mention orphans:\n%s", s)
	}
	if !rep.OK() {
		t.Fatalf("summary of healthy-with-orphans volume should still be OK:\n%s", s)
	}
}
