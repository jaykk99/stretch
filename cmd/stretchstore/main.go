// Command stretchstore is the CLI for StretchStore, the thin-provisioned,
// compressed, deduplicated virtual storage app.
//
//	stretchstore create --logical 1000GB --physical 1GB [--dir ./vol]
//	stretchstore mount   --dir ./vol --mp /mnt/stretch
//	stretchstore status  --dir ./vol
//	stretchstore test    [--dir ./vol] [--logical 1000GB] [--physical 1GB]
package main

import (
	"crypto/rand"
	"errors"
	"flag"
	"fmt"
	"math"
	mrand "math/rand"
	"os"
	"path/filepath"
	"syscall"

	"stretchstore/internal/size"
	"stretchstore/internal/store"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "create":
		err = cmdCreate(os.Args[2:])
	case "mount":
		err = cmdMount(os.Args[2:])
	case "status":
		err = cmdStatus(os.Args[2:])
	case "serve":
		err = cmdServe(os.Args[2:])
	case "test":
		err = cmdTest(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Print(`stretchstore — a 1000GB logical drive backed by 1GB of real storage.

Thin provisioning + zstd compression + content-defined dedup. The physics is
honest: huge ratios are real only for redundant data; unique random data is
~1:1 and the physical limit is a hard wall.

Usage:
  stretchstore create --logical 1000GB --physical 1GB [--dir ./vol]
  stretchstore mount  --dir ./vol --mp /mnt/stretch   (FUSE; Linux/macOS)
  stretchstore serve  --dir ./vol [--addr 127.0.0.1:8080]  (HTTP/WebDAV fallback, all OSes)
  stretchstore status --dir ./vol
  stretchstore test   [--dir ./vol] [--logical 1000GB] [--physical 1GB]

On machines without FUSE (Windows builds, Android/Termux, minimal
containers), use "serve": the volume appears as a virtual disk.img over
HTTP/WebDAV on localhost instead of a kernel mount.
`)
}

func cmdCreate(args []string) error {
	fs := flag.NewFlagSet("create", flag.ExitOnError)
	logical := fs.String("logical", "1000GB", "logical (thin) size, e.g. 1000GB")
	physical := fs.String("physical", "1GB", "physical backing limit, e.g. 1GB")
	dir := fs.String("dir", "", "volume directory")
	fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	l, err := size.Parse(*logical)
	if err != nil {
		return err
	}
	p, err := size.Parse(*physical)
	if err != nil {
		return err
	}
	st, err := store.Create(*dir, l, p)
	if err != nil {
		return err
	}
	st.Close()
	fmt.Printf("created volume at %s\n  logical:  %s (thin — costs nothing until written)\n  physical: %s (hard limit)\n", *dir, size.Format(l), size.Format(p))
	return nil
}

func cmdStatus(args []string) error {
	fs := flag.NewFlagSet("status", flag.ExitOnError)
	dir := fs.String("dir", "", "volume directory")
	fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	st, err := store.Open(*dir)
	if err != nil {
		return err
	}
	defer st.Close()
	printStatus(st.Status())
	return nil
}

func printStatus(s store.Status) {
	fmt.Printf("logical size:    %s\n", size.Format(s.LogicalSize))
	fmt.Printf("logical mapped:  %s\n", size.Format(s.LogicalMapped))
	fmt.Printf("physical used:   %s / %s (%.1f%%)\n", size.Format(s.PhysicalUsed), size.Format(s.PhysicalLimit),
		100*float64(s.PhysicalUsed)/float64(s.PhysicalLimit))
	fmt.Printf("unique chunks:   %d (%d references)\n", s.UniqueChunks, s.ChunkRefs)
	if s.PhysicalUsed > 0 {
		fmt.Printf("real ratio:      %.1f:1\n", s.Ratio())
	} else {
		fmt.Printf("real ratio:      n/a (nothing stored yet)\n")
	}
}

// ---------------------------------------------------------------------------
// The self-test: "makes and tests and finishes."
//
// Creates a fresh volume, writes four honest patterns (zeros, 100 identical
// copies, realistic mixed data, pure random as the adversarial case), reads
// everything back byte-for-byte, then fills the physical store to prove the
// limit is real and writes are refused honestly instead of pretending.
// ---------------------------------------------------------------------------

func cmdTest(args []string) error {
	fs := flag.NewFlagSet("test", flag.ExitOnError)
	dir := fs.String("dir", "", "volume directory (default: temp dir)")
	logical := fs.String("logical", "1000GB", "logical size")
	physical := fs.String("physical", "1GB", "physical limit")
	fs.Parse(args)
	l, err := size.Parse(*logical)
	if err != nil {
		return err
	}
	p, err := size.Parse(*physical)
	if err != nil {
		return err
	}
	volDir := *dir
	cleanup := false
	if volDir == "" {
		// NOTE: temp dir under cwd, not os.TempDir() — /tmp is often a tiny
		// tmpfs and the test needs room for the full 1GB physical cap.
		tmp, err := os.MkdirTemp(".", "stretchtest")
		if err != nil {
			return err
		}
		volDir = filepath.Join(tmp, "vol")
		cleanup = true
		defer os.RemoveAll(tmp)
	}
	st, err := store.Create(volDir, l, p)
	if err != nil {
		return err
	}
	defer st.Close()

	fmt.Printf("StretchStore self-test — %s logical / %s physical\n", size.Format(l), size.Format(p))
	fmt.Printf("volume: %s\n\n", volDir)

	type result struct {
		name     string
		logical  uint64
		physUsed uint64
	}
	var results []result
	physNow := func() uint64 { return st.Status().PhysicalUsed }

	// Pattern 1: 1GB of zeros. The best case: dedups to ONE chunk, compresses to ~nothing.
	fmt.Printf("[1/5] writing 1.0 GB of zeros ...")
	base := physNow()
	off := int64(0)
	zeroChunk := make([]byte, 16<<20)
	const zerosLen = 1 << 30
	for w := uint64(0); w < zerosLen; w += uint64(len(zeroChunk)) {
		if _, err := st.WriteAt(zeroChunk, off+int64(w)); err != nil {
			return fmt.Errorf("zeros write: %w", err)
		}
	}
	used := physNow() - base
	results = append(results, result{"1.0 GB of zeros", zerosLen, used})
	fmt.Printf(" physical +%s\n", size.Format(used))

	// Pattern 2: 100 copies of a 10MB file (1GB logical). Dedup should store it once.
	fmt.Printf("[2/5] writing 100 x 10MB identical files ...")
	base = physNow()
	rng := mrand.New(mrand.NewSource(42))
	const fox = "the quick brown fox jumps over the lazy dog 0123456789\n"
	file10 := make([]byte, 10<<20)
	for i := range file10 {
		// semi-compressible: repeating text-ish pattern with slight variation
		file10[i] = fox[(i*7)%len(fox)] + byte(rng.Intn(3))
	}
	off = int64(zerosLen)
	for i := 0; i < 100; i++ {
		if _, err := st.WriteAt(file10, off+int64(i)*int64(len(file10))); err != nil {
			return fmt.Errorf("repeat write %d: %w", i, err)
		}
	}
	used = physNow() - base
	results = append(results, result{"100 x 10MB identical", 100 * uint64(len(file10)), used})
	fmt.Printf(" physical +%s\n", size.Format(used))

	// Pattern 3: 200MB realistic mixed data — region types like real disk
	// content: text-heavy runs, structured binary, some repeats, some random.
	fmt.Printf("[3/5] writing 200 MB mixed realistic data ...")
	base = physNow()
	mixed := make([]byte, 200<<20)
	const lorem = "lorem ipsum dolor sit amet, consectetur adipiscing elit; "
	const region = 1 << 20
	for rg := 0; rg < len(mixed); rg += region {
		switch rg / region % 10 {
		case 0, 1, 2, 3: // text-heavy region
			for i := 0; i < region; i++ {
				mixed[rg+i] = lorem[(rg+i)%len(lorem)]
			}
		case 4, 5, 6: // structured binary: repeated 256-byte records
			for i := 0; i < region; i++ {
				mixed[rg+i] = byte((i%256)*5 + rg)
			}
		case 7, 8: // repeats of earlier content (dedupable)
			copy(mixed[rg:rg+region], mixed[rg%(4*region):][:region])
		default: // random region
			for i := 0; i < region; i++ {
				mixed[rg+i] = byte(rng.Intn(256))
			}
		}
	}
	off = int64(zerosLen) + 100*int64(len(file10))
	if _, err := st.WriteAt(mixed, off); err != nil {
		return fmt.Errorf("mixed write: %w", err)
	}
	used = physNow() - base
	results = append(results, result{"200 MB mixed realistic", uint64(len(mixed)), used})
	fmt.Printf(" physical +%s\n", size.Format(used))

	// Pattern 4: 100MB of pure random — the adversarial case. ~1:1, no miracles.
	fmt.Printf("[4/5] writing 100 MB pure random (adversarial) ...")
	base = physNow()
	advers := make([]byte, 100<<20)
	if _, err := rand.Read(advers); err != nil {
		return err
	}
	off += int64(len(mixed))
	if _, err := st.WriteAt(advers, off); err != nil {
		return fmt.Errorf("random write: %w", err)
	}
	used = physNow() - base
	results = append(results, result{"100 MB pure random", uint64(len(advers)), used})
	fmt.Printf(" physical +%s\n", size.Format(used))

	// Verify: read everything back byte-for-byte.
	fmt.Printf("[5/5] verifying all patterns byte-for-byte ...")
	if err := verifyZeros(st, 0, zerosLen); err != nil {
		return fmt.Errorf("zeros verify: %w", err)
	}
	for i := 0; i < 100; i++ {
		if err := verifyEqual(st, int64(zerosLen)+int64(i)*int64(len(file10)), file10); err != nil {
			return fmt.Errorf("repeat verify %d: %w", i, err)
		}
	}
	if err := verifyEqual(st, int64(zerosLen)+100*int64(len(file10)), mixed); err != nil {
		return fmt.Errorf("mixed verify: %w", err)
	}
	if err := verifyEqual(st, off, advers); err != nil {
		return fmt.Errorf("random verify: %w", err)
	}
	fmt.Printf(" OK\n")

	// Fill test: keep writing random until a physical wall. Our own 1GB cap
	// refuses honestly with ErrNoSpace; if the underlying disk fills first,
	// the OS error is equally real and equally terminal.
	fmt.Printf("      filling physical store to prove the limit is real ...")
	fillOff := off + int64(len(advers))
	fillChunk := make([]byte, 32<<20)
	var fillWrote uint64
	wallKind := "cap"
	for {
		if _, err := rand.Read(fillChunk); err != nil {
			return err
		}
		n, err := st.WriteAt(fillChunk, fillOff+int64(fillWrote))
		if errors.Is(err, store.ErrNoSpace) {
			break
		}
		if err != nil {
			if isDiskFull(err) {
				wallKind = "underlying disk"
				break
			}
			return fmt.Errorf("fill write: %w", err)
		}
		fillWrote += uint64(n)
	}
	// Previously written data must still be intact after the refusal.
	if err := verifyEqual(st, off, advers); err != nil {
		return fmt.Errorf("post-fill integrity: %w", err)
	}
	fmt.Printf(" refused at %s physical (%s wall, data intact)\n", size.Format(st.Status().PhysicalUsed), wallKind)

	// Report.
	fmt.Println("\n================ HONEST REPORT ================")
	var totLogical, totPhys uint64
	for _, r := range results {
		totLogical += r.logical
		totPhys += r.physUsed
		ratio := math.Inf(1)
		if r.physUsed > 0 {
			ratio = float64(r.logical) / float64(r.physUsed)
		}
		fmt.Printf("  %-24s logical %8s  physical %8s  ratio %10.1f:1\n",
			r.name, size.Format(r.logical), size.Format(r.physUsed), ratio)
	}
	fmt.Printf("  %-24s logical %8s  physical %8s  ratio %10.1f:1  (+fill %s random)\n",
		"TOTAL (patterns)", size.Format(totLogical), size.Format(totPhys),
		float64(totLogical)/float64(totPhys), size.Format(fillWrote))
	s := st.Status()
	fmt.Printf("\n  volume: %s logical, %s physical hard limit\n", size.Format(s.LogicalSize), size.Format(s.PhysicalLimit))
	fmt.Printf("  physical now: %s (%.1f%%) — unique chunks: %d\n",
		size.Format(s.PhysicalUsed), 100*float64(s.PhysicalUsed)/float64(s.PhysicalLimit), s.UniqueChunks)
	fmt.Println("\n  THE PHYSICS: 1000:1 is real ONLY for redundant data — zeros and")
	fmt.Println("  repeated files genuinely collapse 100:1 to 100000:1. Unique random")
	fmt.Println("  data (encrypted, video, already-compressed) is ~1:1: the 1GB")
	fmt.Println("  physical limit is REAL. This app shows true physical usage and")
	fmt.Println("  refuses writes with ENOSPC when full instead of pretending.")
	fmt.Println("  You cannot beat the pigeonhole principle: N bytes of physical")
	fmt.Println("  storage can hold at most N bytes of incompressible information.")
	if cleanup {
		fmt.Println("\n  (test volume was temporary and has been removed)")
	} else {
		fmt.Printf("\n  test volume kept at %s\n", volDir)
	}
	return nil
}

func verifyZeros(st *store.Store, off int64, n uint64) error {
	buf := make([]byte, 4<<20)
	for done := uint64(0); done < n; {
		want := uint64(len(buf))
		if done+want > n {
			want = n - done
		}
		b := buf[:want]
		for i := range b {
			b[i] = 0xAA // poison: ReadAt must overwrite everything
		}
		if _, err := st.ReadAt(b, off+int64(done)); err != nil {
			return err
		}
		for i, v := range b {
			if v != 0 {
				return fmt.Errorf("byte %d: got %d, want 0", done+uint64(i), v)
			}
		}
		done += want
	}
	return nil
}

func verifyEqual(st *store.Store, off int64, want []byte) error {
	buf := make([]byte, 4<<20)
	for done := 0; done < len(want); {
		n := len(buf)
		if done+n > len(want) {
			n = len(want) - done
		}
		b := buf[:n]
		if _, err := st.ReadAt(b, int64(off)+int64(done)); err != nil {
			return err
		}
		for i := 0; i < n; i++ {
			if b[i] != want[done+i] {
				return fmt.Errorf("byte %d: got %d, want %d", done+i, b[i], want[done+i])
			}
		}
		done += n
	}
	return nil
}

// isDiskFull reports whether err is the OS telling us the underlying disk
// is full — a real physical wall, just not ours.
func isDiskFull(err error) bool {
	for err != nil {
		if pe, ok := err.(*os.PathError); ok {
			err = pe.Err
			continue
		}
		if en, ok := err.(syscall.Errno); ok {
			return en == syscall.ENOSPC
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
