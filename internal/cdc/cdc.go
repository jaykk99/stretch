// Package cdc implements a FastCDC-style content-defined chunker.
//
// Instead of cutting data into fixed-size blocks, chunk boundaries are
// decided by the *content itself* (a rolling gear hash). Identical byte
// sequences therefore produce identical chunks even when they sit at
// different offsets — which is what makes cross-file dedup work.
//
// The gear table is generated from a fixed seed, so chunking is fully
// deterministic: same bytes in → same chunks out, every run.
package cdc

const (
	MinSize = 16 * 1024
	AvgSize = 64 * 1024
	MaxSize = 256 * 1024
)

// avgBits is log2(AvgSize): a boundary fires with probability 1/2^16,
// giving an expected chunk size of ~64KB between the min/max clamps.
//
// window is the rolling-hash window in bytes. Boundaries depend only on
// the last `window` bytes (not on everything since the chunk start), so
// chunking *resynchronizes* after insertions/deletions: identical content
// at different offsets yields identical chunks. This is what makes
// cross-file, cross-offset dedup actually work.
const avgBits = 16
const window = 48

var gear [256]uint64

func init() {
	// splitmix64 with a fixed seed: deterministic table, no startup randomness.
	x := uint64(0x9E3779B97F4A7C15)
	for i := range gear {
		x += 0x9E3779B97F4A7C15
		z := x
		z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
		z = (z ^ (z >> 27)) * 0x94D049BB133111EB
		gear[i] = z ^ (z >> 31)
	}
}

// Chunk splits data into content-defined chunks. Each returned slice
// aliases data (no copying). An empty input yields no chunks.
func Chunk(data []byte) [][]byte {
	var out [][]byte
	n := len(data)
	start := 0
	for start < n {
		// The last chunk just runs to the end if it can't reach MinSize.
		if n-start <= MinSize {
			out = append(out, data[start:n])
			break
		}
		hash := uint64(0)
		end := n
		limit := start + MaxSize
		if limit < n {
			end = limit
		}
		cut := end // default: forced cut at max/end
		const mask = uint64((1 << avgBits) - 1)
		for i := start; i < end; i++ {
			// Gear rolling hash over a fixed window: the byte leaving the
			// window is subtracted out (mod 2^64 arithmetic), so the hash
			// — and therefore every boundary decision — depends only on
			// the last `window` bytes.
			hash = (hash << 1) + gear[data[i]]
			if i-start >= window {
				hash -= gear[data[i-window]] << window
			}
			if i-start+1 >= MinSize && (hash&mask) == 0 {
				cut = i + 1
				break
			}
		}
		out = append(out, data[start:cut])
		start = cut
	}
	return out
}
