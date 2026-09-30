package store

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func testVol(t *testing.T, logical, physical uint64) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := Create(filepath.Join(dir, "vol"), logical, physical)
	if err != nil {
		t.Fatal(err)
	}
	return st, dir
}

func TestThinProvisionFreshVolume(t *testing.T) {
	st, _ := testVol(t, 1000<<30, 1<<30)
	defer st.Close()
	s := st.Status()
	if s.LogicalSize != 1000<<30 {
		t.Fatalf("logical size = %d, want 1000GB", s.LogicalSize)
	}
	if s.PhysicalUsed > 64<<10 {
		t.Fatalf("fresh 1000GB volume consumed %d physical bytes, want ~0", s.PhysicalUsed)
	}
	// Unwritten regions read as zeros without touching the disk.
	buf := make([]byte, 1<<20)
	if _, err := st.ReadAt(buf, 999<<30); err != nil {
		t.Fatal(err)
	}
	if !isZeros(buf) {
		t.Fatal("unwritten region did not read as zeros")
	}
	if got := st.Status().PhysicalUsed; got != s.PhysicalUsed {
		t.Fatalf("reading holes consumed physical space: %d -> %d", s.PhysicalUsed, got)
	}
}

func TestWriteReadRoundTrip(t *testing.T) {
	st, _ := testVol(t, 10<<30, 1<<30)
	defer st.Close()
	data := make([]byte, 3<<20)
	for i := range data {
		data[i] = byte(i * 13)
	}
	// straddle a slot boundary (SlotSize = 1MB)
	if _, err := st.WriteAt(data, SlotSize-1000); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, len(data))
	if _, err := st.ReadAt(out, SlotSize-1000); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, out) {
		t.Fatal("round-trip mismatch across slot boundary")
	}
}

func TestDedupStoresOnce(t *testing.T) {
	st, _ := testVol(t, 10<<30, 1<<30)
	defer st.Close()
	block := bytes.Repeat([]byte("the same ten megabytes, over and over. "), 300000) // ~12MB
	if _, err := st.WriteAt(block, 0); err != nil {
		t.Fatal(err)
	}
	after1 := st.Status()
	if _, err := st.WriteAt(block, 50<<20); err != nil {
		t.Fatal(err)
	}
	after2 := st.Status()
	if after2.UniqueChunks != after1.UniqueChunks {
		t.Fatalf("dedup failed: unique chunks %d -> %d for identical data",
			after1.UniqueChunks, after2.UniqueChunks)
	}
	// The chunk bytes must be stored exactly once. The journal legitimately
	// grows (it must record WHERE the second copy lives), but that overhead
	// must stay tiny relative to the duplicated data.
	growth := after2.PhysicalUsed - after1.PhysicalUsed
	if growth > uint64(len(block))/200 { // > 0.5% overhead is a bug
		t.Fatalf("second identical write consumed %d physical bytes for %d logical (%.2f%% overhead)",
			growth, len(block), 100*float64(growth)/float64(len(block)))
	}
	// Both copies read back identically.
	for _, off := range []int64{0, 50 << 20} {
		out := make([]byte, len(block))
		if _, err := st.ReadAt(out, off); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(block, out) {
			t.Fatalf("copy at %d mismatch", off)
		}
	}
}

func TestZerosCollapse(t *testing.T) {
	st, _ := testVol(t, 1000<<30, 1<<30)
	defer st.Close()
	zeros := make([]byte, 256<<20)
	if _, err := st.WriteAt(zeros, 0); err != nil {
		t.Fatal(err)
	}
	s := st.Status()
	if s.PhysicalUsed > 1<<20 {
		t.Fatalf("256MB of zeros consumed %d physical bytes, want < 1MB", s.PhysicalUsed)
	}
	out := make([]byte, len(zeros))
	if _, err := st.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	if !isZeros(out) {
		t.Fatal("zeros did not read back as zeros")
	}
}

func TestRandomDataHonestRatio(t *testing.T) {
	st, _ := testVol(t, 10<<30, 1<<30)
	defer st.Close()
	const n = 32 << 20
	data := make([]byte, n)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	before := st.Status().PhysicalUsed
	if _, err := st.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	used := st.Status().PhysicalUsed - before
	// Incompressible + unique: physical must be ~1:1 (allow 10% framing overhead).
	if used < n || used > n+n/10 {
		t.Fatalf("random data: %d logical consumed %d physical, want ~1:1", n, used)
	}
	out := make([]byte, n)
	if _, err := st.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, out) {
		t.Fatal("random data round-trip mismatch")
	}
}

func TestPhysicalLimitHonestlyRefused(t *testing.T) {
	st, _ := testVol(t, 10<<30, 64<<20) // tiny 64MB physical
	defer st.Close()
	precious := []byte("precious data that must survive")
	if _, err := st.WriteAt(precious, 0); err != nil {
		t.Fatal(err)
	}
	// Fill with random until refusal.
	chunk := make([]byte, 4<<20)
	off := int64(1 << 20)
	refused := false
	for i := 0; i < 100; i++ {
		if _, err := rand.Read(chunk); err != nil {
			t.Fatal(err)
		}
		_, err := st.WriteAt(chunk, off)
		if errors.Is(err, ErrNoSpace) {
			refused = true
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		off += int64(len(chunk))
	}
	if !refused {
		t.Fatal("expected ENOSPC refusal, writes kept succeeding past the physical limit")
	}
	s := st.Status()
	if s.PhysicalUsed > s.PhysicalLimit {
		t.Fatalf("physical limit exceeded: used %d > limit %d", s.PhysicalUsed, s.PhysicalLimit)
	}
	// Data written before the refusal is intact.
	out := make([]byte, len(precious))
	if _, err := st.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(precious, out) {
		t.Fatal("pre-existing data corrupted after ENOSPC refusal")
	}
}

func TestCrashRecoveryTornJournal(t *testing.T) {
	dir := t.TempDir()
	volDir := filepath.Join(dir, "vol")
	st, err := Create(volDir, 10<<30, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("data written before the crash")
	if _, err := st.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash mid-journal-write: append a torn record (length prefix
	// claims 1MB, only 10 bytes follow, no CRC).
	f, err := os.OpenFile(filepath.Join(volDir, "manifest.log"), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte{0x00, 0x00, 0x10, 0x00}) // length = 1MB (little-endian)
	f.Write([]byte("0123456789"))
	f.Close()
	// Also append garbage to chunks.dat tail.
	cf, _ := os.OpenFile(filepath.Join(volDir, "chunks.dat"), os.O_WRONLY|os.O_APPEND, 0o644)
	cf.Write([]byte{0xFF, 0xFF, 0xFF, 0xFF, 0x01, 0x02})
	cf.Close()

	st2, err := Open(volDir)
	if err != nil {
		t.Fatalf("reopen after torn tail: %v", err)
	}
	defer st2.Close()
	out := make([]byte, len(data))
	if _, err := st2.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, out) {
		t.Fatal("data lost after crash recovery")
	}
	// Store still works after recovery.
	if _, err := st2.WriteAt([]byte("after crash"), 1<<20); err != nil {
		t.Fatalf("write after recovery: %v", err)
	}
}

func TestTruncate(t *testing.T) {
	st, _ := testVol(t, 10<<30, 1<<30)
	defer st.Close()
	data := bytes.Repeat([]byte("shrink me "), 200000)
	if _, err := st.WriteAt(data, 0); err != nil {
		t.Fatal(err)
	}
	if err := st.Truncate(100); err != nil {
		t.Fatal(err)
	}
	s := st.Status()
	if s.FileSize != 100 {
		t.Fatalf("file size = %d, want 100", s.FileSize)
	}
	out := make([]byte, 100)
	if _, err := st.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data[:100], out) {
		t.Fatal("truncated head mismatch")
	}
	// Reopen: truncation must persist via the journal.
	dir := st.dir
	st.Close()
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if st2.Status().FileSize != 100 {
		t.Fatal("truncate did not survive reopen")
	}
}

func TestPartialOverwriteKeepsRest(t *testing.T) {
	st, _ := testVol(t, 10<<30, 1<<30)
	defer st.Close()
	base := bytes.Repeat([]byte("A"), 2<<20)
	if _, err := st.WriteAt(base, 0); err != nil {
		t.Fatal(err)
	}
	patch := bytes.Repeat([]byte("B"), 100)
	if _, err := st.WriteAt(patch, 1<<20-50); err != nil { // straddles slot boundary
		t.Fatal(err)
	}
	out := make([]byte, 2<<20)
	if _, err := st.ReadAt(out, 0); err != nil {
		t.Fatal(err)
	}
	for i, v := range out {
		want := byte('A')
		if i >= (1<<20-50) && i < (1<<20+50) {
			want = 'B'
		}
		if v != want {
			t.Fatalf("byte %d = %c, want %c", i, v, want)
		}
	}
}
