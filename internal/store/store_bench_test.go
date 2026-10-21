package store

import (
	"crypto/rand"
	"testing"
)

// Benchmarks for the store's hot paths. Run with:
//   go test -bench=. -benchtime=10x ./internal/store/

func BenchmarkWriteAtRandom(b *testing.B) {
	st, _ := testVolume(b, 1<<30, 512<<20)
	buf := make([]byte, 1<<20)
	if _, err := rand.Read(buf); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Distinct offsets defeat dedup: measures the adversarial path.
		if _, err := st.WriteAt(buf, int64(i)<<20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteAtDedup(b *testing.B) {
	st, _ := testVolume(b, 1<<30, 512<<20)
	buf := make([]byte, 1<<20)
	if _, err := rand.Read(buf); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Same bytes every time: measures the dedup fast path
		// (chunk hashes already indexed, only the journal grows).
		if _, err := st.WriteAt(buf, int64(i)<<20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadAt(b *testing.B) {
	st, _ := testVolume(b, 1<<30, 512<<20)
	buf := make([]byte, 4<<20)
	if _, err := rand.Read(buf); err != nil {
		b.Fatal(err)
	}
	mustWrite(b, st, buf, 0)
	out := make([]byte, 1<<20)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.ReadAt(out, 0); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerify(b *testing.B) {
	st, _ := testVolume(b, 1<<30, 512<<20)
	buf := make([]byte, 4<<20)
	if _, err := rand.Read(buf); err != nil {
		b.Fatal(err)
	}
	mustWrite(b, st, buf, 0)
	mustWrite(b, st, buf, 4<<20) // overwrite -> orphans, the realistic case
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Verify(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPrune(b *testing.B) {
	st, _ := testVolume(b, 1<<30, 512<<20)
	buf := make([]byte, 2<<20)
	for i := 0; i < 3; i++ {
		if _, err := rand.Read(buf); err != nil {
			b.Fatal(err)
		}
		mustWrite(b, st, buf, 0) // two orphaned generations
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		// Re-orphan: each iteration needs fresh garbage to collect.
		// (Prune itself is measured; setup is not.)
		nb := make([]byte, 2<<20)
		if _, err := rand.Read(nb); err != nil {
			b.Fatal(err)
		}
		mustWrite(b, st, nb, 0)
		b.StartTimer()
		if _, err := st.Prune(); err != nil {
			b.Fatal(err)
		}
	}
}
