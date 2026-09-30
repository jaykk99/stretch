package cdc

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"testing"
)

func TestDeterminism(t *testing.T) {
	data := make([]byte, 1<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	a := Chunk(data)
	b := Chunk(data)
	if len(a) != len(b) {
		t.Fatalf("non-deterministic chunk count: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if !bytes.Equal(a[i], b[i]) {
			t.Fatalf("chunk %d differs between runs", i)
		}
	}
}

func TestChunkSizesBounded(t *testing.T) {
	data := make([]byte, 4<<20)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	for i, c := range Chunk(data) {
		if len(c) > MaxSize {
			t.Fatalf("chunk %d exceeds MaxSize: %d", i, len(c))
		}
		if len(c) < MinSize && i < len(Chunk(data))-1 {
			t.Fatalf("non-terminal chunk %d below MinSize: %d", i, len(c))
		}
	}
}

func TestIdenticalContentIdenticalChunks(t *testing.T) {
	// Same bytes shifted by an insertion must still share most chunks:
	// content-defined boundaries resynchronize after the disruption.
	// (With only ~1 chunk of data the min-size gate dominates, so this
	// needs a few MB to be meaningful.)
	block := make([]byte, 2<<20)
	if _, err := rand.Read(block); err != nil {
		t.Fatal(err)
	}
	hashes := func(data []byte) map[string]bool {
		m := map[string]bool{}
		for _, c := range Chunk(data) {
			h := sha256.Sum256(c)
			m[hex.EncodeToString(h[:])] = true
		}
		return m
	}
	base := hashes(block)
	pad := bytes.Repeat([]byte{0xAA}, 1000)
	shifted := hashes(append(pad, block...))
	shared := 0
	for h := range base {
		if shifted[h] {
			shared++
		}
	}
	if frac := float64(shared) / float64(len(base)); frac < 0.5 {
		t.Fatalf("only %.0f%% of chunks shared after shift, want >= 50%%", 100*frac)
	}
}

func TestEmpty(t *testing.T) {
	if len(Chunk(nil)) != 0 || len(Chunk([]byte{})) != 0 {
		t.Fatal("empty input should yield no chunks")
	}
}

func TestCoverage(t *testing.T) {
	// Chunks must cover the input exactly once, in order, with no gaps/overlaps.
	data := make([]byte, 300*1024)
	for i := range data {
		data[i] = byte(i * 31)
	}
	chunks := Chunk(data)
	total := 0
	for _, c := range chunks {
		if !bytes.Equal(data[total:total+len(c)], c) {
			t.Fatal("chunk content does not match input at offset", total)
		}
		total += len(c)
	}
	if total != len(data) {
		t.Fatalf("coverage %d != input %d", total, len(data))
	}
}
