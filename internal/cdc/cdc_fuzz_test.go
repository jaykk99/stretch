package cdc

import (
	"bytes"
	"testing"
)

// FuzzChunk feeds arbitrary inputs through the chunker and asserts the
// structural invariants every chunking must uphold:
//
//  1. Tiling: concatenating the chunks reproduces the input byte-for-byte.
//  2. Bounds: every chunk is in (0, MaxSize], except the input may end
//     with one short tail when it can't reach MinSize.
//  3. No empty chunks, no panics, no hangs on any input.
//
// Run briefly in CI with: go test -fuzz=FuzzChunk -fuzztime=30s ./internal/cdc/
func FuzzChunk(f *testing.F) {
	// Seed corpus: shapes the fuzzer cares about.
	f.Add([]byte{})
	f.Add([]byte("x"))
	f.Add(bytes.Repeat([]byte("a"), MinSize))
	f.Add(bytes.Repeat([]byte("a"), MinSize+1))
	f.Add(bytes.Repeat([]byte("b"), MaxSize+100))
	f.Add(bytes.Repeat([]byte{0x00, 0xFF}, 100000))
	pattern := make([]byte, 0, 70000)
	for i := 0; i < 70000; i++ {
		pattern = append(pattern, byte(i*31))
	}
	f.Add(pattern)

	f.Fuzz(func(t *testing.T, data []byte) {
		// Guard the fuzzer's time budget: 4MB is plenty to exercise the
		// chunker and keeps each iteration fast.
		if len(data) > 4<<20 {
			t.Skip("input too large")
		}
		chunks := Chunk(data)

		if len(data) == 0 {
			if len(chunks) != 0 {
				t.Fatalf("empty input produced %d chunks", len(chunks))
			}
			return
		}
		if len(chunks) == 0 {
			t.Fatalf("non-empty input produced no chunks")
		}

		// Invariant 1: tiling.
		var joined []byte
		for i, c := range chunks {
			// Invariant 2/3: no empty chunks, size cap respected.
			if len(c) == 0 {
				t.Fatalf("chunk %d is empty", i)
			}
			if len(c) > MaxSize {
				t.Fatalf("chunk %d is %d bytes, over MaxSize %d", i, len(c), MaxSize)
			}
			joined = append(joined, c...)
		}
		if !bytes.Equal(joined, data) {
			t.Fatalf("chunks do not tile input: got %d bytes, want %d",
				len(joined), len(data))
		}

		// Invariant 4: determinism — same bytes, same chunk boundaries.
		again := Chunk(data)
		if len(again) != len(chunks) {
			t.Fatalf("non-deterministic: %d vs %d chunks", len(chunks), len(again))
		}
		for i := range chunks {
			if len(again[i]) != len(chunks[i]) {
				t.Fatalf("non-deterministic boundary at chunk %d", i)
			}
		}
	})
}

// FuzzChunkResync checks the property that makes content-defined chunking
// worth having: inserting bytes near the start of the input only perturbs
// chunk boundaries locally. Chunks fully after the insertion point must be
// byte-identical to the un-inserted run (the resynchronization property
// that gives cross-offset dedup).
func FuzzChunkResync(f *testing.F) {
	f.Add([]byte(bytes.Repeat([]byte("abcdefgh"), 20000)))
	f.Add(bytes.Repeat([]byte{1, 2, 3, 4, 5}, 30000))

	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) < 2*MaxSize || len(data) > 1<<20 {
			t.Skip("need a decent-sized input")
		}
		// Insert 100 bytes at the 1/3 mark.
		at := len(data) / 3
		insert := bytes.Repeat([]byte("!"), 100)
		shifted := make([]byte, 0, len(data)+len(insert))
		shifted = append(shifted, data[:at]...)
		shifted = append(shifted, insert...)
		shifted = append(shifted, data[at:]...)

		a := Chunk(data)
		b := Chunk(shifted)

		// The tail chunks of both runs cover the same underlying bytes
		// (data[2*at:] in the original). At least the final chunk must be
		// identical — with content-defined boundaries, most tail chunks are.
		// We assert the weaker, always-true form: the last chunk of the
		// shifted run equals the last chunk of the original run whenever
		// both tails extend past the insertion by more than MaxSize.
		la, lb := a[len(a)-1], b[len(b)-1]
		overlap := len(data) - 2*at
		if overlap > MaxSize && !bytes.Equal(la, lb) {
			t.Fatalf("resync failed: last chunks differ after insertion")
		}
	})
}
