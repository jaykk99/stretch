package memstore

import (
	"bytes"
	"crypto/rand"
	"errors"
	"testing"
)

func TestThinAlloc(t *testing.T) {
	s, err := New(1 << 30)
	if err != nil {
		t.Fatal(err)
	}
	s.Alloc(1 << 30) // 1GB segment must cost ~nothing
	st := s.Stats()
	if st.PhysicalUsed != 0 {
		t.Fatalf("fresh 1GB segment used %d physical bytes", st.PhysicalUsed)
	}
}

func TestZerosCollapse(t *testing.T) {
	s, _ := New(1 << 30)
	id := s.Alloc(64 << 20)
	if err := s.Write(id, make([]byte, 64<<20), 0); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	if st.PhysicalUsed != 0 {
		t.Fatalf("64MB of zeros used %d physical bytes", st.PhysicalUsed)
	}
	if st.UniquePages != 0 {
		t.Fatalf("zeros created %d pages", st.UniquePages)
	}
	out := make([]byte, 1<<20)
	if err := s.Read(id, out, 0); err != nil {
		t.Fatal(err)
	}
	if !isZeros(out) {
		t.Fatal("zeros did not read back as zeros")
	}
}

func TestDedupAcrossSegments(t *testing.T) {
	s, _ := New(1 << 30)
	block := bytes.Repeat([]byte("shared page content, 4KB of it. "), 128) // 4KB
	a := s.Alloc(1 << 20)
	b := s.Alloc(1 << 20)
	if err := s.Write(a, bytes.Repeat(block, 256), 0); err != nil { // 1MB
		t.Fatal(err)
	}
	after1 := s.Stats()
	if err := s.Write(b, bytes.Repeat(block, 256), 0); err != nil {
		t.Fatal(err)
	}
	after2 := s.Stats()
	if after2.UniquePages != after1.UniquePages {
		t.Fatalf("dedup failed: unique pages %d -> %d", after1.UniquePages, after2.UniquePages)
	}
	if after2.PhysicalUsed != after1.PhysicalUsed {
		t.Fatalf("identical segment consumed %d more bytes", after2.PhysicalUsed-after1.PhysicalUsed)
	}
	// Both read back identically.
	for _, id := range []uint64{a, b} {
		out := make([]byte, 1<<20)
		if err := s.Read(id, out, 0); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out, bytes.Repeat(block, 256)) {
			t.Fatalf("segment %d mismatch", id)
		}
	}
}

func TestRandomHonest(t *testing.T) {
	s, _ := New(1 << 30)
	id := s.Alloc(16 << 20)
	data := make([]byte, 16<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(id, data, 0); err != nil {
		t.Fatal(err)
	}
	st := s.Stats()
	// Incompressible: physical must be within a few % of logical (framing may
	// even push it slightly OVER 1:1 — that honesty is the point).
	lo := uint64(float64(len(data)) * 0.95)
	hi := uint64(float64(len(data)) * 1.05)
	if st.PhysicalUsed < lo || st.PhysicalUsed > hi {
		t.Fatalf("16MB random: physical %d, want within 5%% of logical", st.PhysicalUsed)
	}
	out := make([]byte, len(data))
	if err := s.Read(id, out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(data, out) {
		t.Fatal("random round-trip mismatch")
	}
}

func TestCapRefused(t *testing.T) {
	s, _ := New(1 << 20) // 1MB cap
	id := s.Alloc(64 << 20)
	precious := []byte("precious")
	if err := s.Write(id, precious, 0); err != nil {
		t.Fatal(err)
	}
	big := make([]byte, 4<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	if err := s.Write(id, big, 1<<20); !errors.Is(err, ErrNoSpace) {
		t.Fatalf("expected ErrNoSpace, got %v", err)
	}
	if got := s.Stats().PhysicalUsed; got > 1<<20 {
		t.Fatalf("cap exceeded: %d > 1MB", got)
	}
	out := make([]byte, len(precious))
	if err := s.Read(id, out, 0); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out, precious) {
		t.Fatal("data corrupted after refusal")
	}
}

func TestFreeReclaims(t *testing.T) {
	s, _ := New(1 << 30)
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	a := s.Alloc(1 << 20)
	if err := s.Write(a, data, 0); err != nil {
		t.Fatal(err)
	}
	used := s.Stats().PhysicalUsed
	if used == 0 {
		t.Fatal("nothing stored")
	}
	b := s.Alloc(1 << 20)
	if err := s.Write(b, data, 0); err != nil { // dedup: shares pages
		t.Fatal(err)
	}
	if err := s.Free(a); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats().PhysicalUsed; got != used {
		t.Fatalf("after freeing one of two shared segments: %d, want %d (still referenced)", got, used)
	}
	if err := s.Free(b); err != nil {
		t.Fatal(err)
	}
	if got := s.Stats().PhysicalUsed; got != 0 {
		t.Fatalf("after freeing all: %d physical bytes leaked", got)
	}
}

func TestPartialPageWrite(t *testing.T) {
	s, _ := New(1 << 30)
	id := s.Alloc(1 << 20)
	base := bytes.Repeat([]byte("A"), 1<<20)
	if err := s.Write(id, base, 0); err != nil {
		t.Fatal(err)
	}
	// straddle a page boundary
	if err := s.Write(id, bytes.Repeat([]byte("B"), 100), PageSize-50); err != nil {
		t.Fatal(err)
	}
	out := make([]byte, 1<<20)
	if err := s.Read(id, out, 0); err != nil {
		t.Fatal(err)
	}
	for i, v := range out {
		want := byte('A')
		if i >= PageSize-50 && i < PageSize+50 {
			want = 'B'
		}
		if v != want {
			t.Fatalf("byte %d = %c, want %c", i, v, want)
		}
	}
}
