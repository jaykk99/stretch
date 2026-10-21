package store

import (
	"bytes"
	"crypto/rand"
	"os"
	"path/filepath"
	"testing"
)

func TestPruneReclaimsOrphanedSpace(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 256<<20)

	// Three generations of random data over the same region: generations 1
	// and 2 become orphans.
	for i := 0; i < 3; i++ {
		data := make([]byte, 4<<20)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		mustWrite(t, st, data, 0)
	}
	before := st.Status().PhysicalUsed

	rep, err := st.Prune()
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if rep.ChunksRemoved == 0 {
		t.Fatalf("expected orphaned chunks to be removed, got %+v", rep)
	}
	if rep.BytesFreed == 0 {
		t.Fatalf("expected bytes freed, got %+v", rep)
	}
	after := st.Status().PhysicalUsed
	if after >= before {
		t.Fatalf("physical used did not shrink: before=%d after=%d", before, after)
	}
	// Logical data untouched: the third generation reads back.
	vrep, err := st.Verify()
	if err != nil {
		t.Fatalf("Verify after prune: %v", err)
	}
	if !vrep.OK() {
		t.Fatalf("volume unhealthy after prune:\n%s", vrep.Summary())
	}
	if vrep.OrphanedChunks != 0 {
		t.Fatalf("expected zero orphans after prune, got %d", vrep.OrphanedChunks)
	}
}

func TestPrunePreservesAllData(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 256<<20)

	// Mixed content across several slots, with dedup-friendly repeats.
	chunk := make([]byte, 1<<20)
	if _, err := rand.Read(chunk); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, chunk, 0)
	mustWrite(t, st, chunk, 1<<20) // identical: dedups to the same chunks
	other := make([]byte, 1<<20)
	if _, err := rand.Read(other); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, other, 10<<20)

	if _, err := st.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	if got := mustRead(t, st, 0, len(chunk)); !bytes.Equal(got, chunk) {
		t.Fatalf("slot 0 data changed by prune")
	}
	if got := mustRead(t, st, 1<<20, len(chunk)); !bytes.Equal(got, chunk) {
		t.Fatalf("deduped slot 1 data changed by prune")
	}
	if got := mustRead(t, st, 10<<20, len(other)); !bytes.Equal(got, other) {
		t.Fatalf("slot 10 data changed by prune")
	}
}

func TestPruneOnFreshVolume(t *testing.T) {
	st, _ := testVolume(t, 1<<30, 64<<20)
	rep, err := st.Prune()
	if err != nil {
		t.Fatalf("Prune on empty volume: %v", err)
	}
	if rep.ChunksRemoved != 0 || rep.BytesFreed != 0 {
		t.Fatalf("empty prune should free nothing, got %+v", rep)
	}
}

func TestPruneThenWriteAgain(t *testing.T) {
	st, dir := testVolume(t, 1<<30, 256<<20)

	data := make([]byte, 2<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, data, 0)
	mustWrite(t, st, make([]byte, 2<<20), 0) // orphan the first write

	if _, err := st.Prune(); err != nil {
		t.Fatalf("Prune: %v", err)
	}

	// The volume must stay fully writable after the file swap.
	more := make([]byte, 2<<20)
	if _, err := rand.Read(more); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, more, 4<<20)
	if got := mustRead(t, st, 4<<20, len(more)); !bytes.Equal(got, more) {
		t.Fatalf("post-prune write lost data")
	}
	// And the pruned data (zeros) still reads back.
	if got := mustRead(t, st, 0, 1<<20); !bytes.Equal(got, make([]byte, 1<<20)) {
		t.Fatalf("pre-prune data changed")
	}

	// Close + reopen: the swapped chunks.dat must replay cleanly.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatalf("reopen after prune: %v", err)
	}
	defer st2.Close()
	if got := mustRead(t, st2, 4<<20, len(more)); !bytes.Equal(got, more) {
		t.Fatalf("data lost across reopen after prune")
	}
	vrep, err := st2.Verify()
	if err != nil {
		t.Fatal(err)
	}
	if !vrep.OK() {
		t.Fatalf("reopened volume unhealthy:\n%s", vrep.Summary())
	}
}

func TestPruneStaleTempFileRefuses(t *testing.T) {
	st, dir := testVolume(t, 1<<30, 64<<20)
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, st, data, 0)

	// Simulate a crashed prune's leftover temp file.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(dir, "chunks.dat.new")
	if err := os.WriteFile(stale, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	st2, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.Prune(); err == nil {
		t.Fatalf("expected Prune to refuse with a stale temp file present")
	}
}
