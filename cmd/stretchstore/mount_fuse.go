//go:build linux || darwin || android

package main

// FUSE mount path: compiled in on Linux, macOS, and Android (Termux).
// At runtime we still verify /dev/fuse exists and degrade to `serve`
// with a clear message when it doesn't.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"stretchstore/internal/fusefs"
	"stretchstore/internal/size"
	"stretchstore/internal/store"
)

func cmdMount(args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	dir := fs.String("dir", "", "volume directory")
	mp := fs.String("mp", "", "mountpoint")
	fs.Parse(args)
	if *dir == "" || *mp == "" {
		return fmt.Errorf("--dir and --mp are required")
	}
	if _, err := os.Stat("/dev/fuse"); err != nil {
		if runtime.GOOS == "android" {
			return errors.New("no /dev/fuse on this device — Termux has no FUSE without root\n" +
				"fallback: stretchstore serve --dir " + *dir + "  (then open http://127.0.0.1:8080 on the device)")
		}
		return errors.New("no /dev/fuse on this machine — FUSE mount unavailable\n" +
			"hint: on Chromebook Linux run: sudo apt install fuse3\n" +
			"fallback: stretchstore serve --dir " + *dir + "  (HTTP/WebDAV file browser on localhost)")
	}
	st, err := store.Open(*dir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	s := st.Status()
	fmt.Printf("mounted %s at %s — disk.img appears as %s (real backing: %s used of %s)\n",
		*dir, *mp, size.Format(s.LogicalSize), size.Format(s.PhysicalUsed), size.Format(s.PhysicalLimit))
	fmt.Println("Ctrl-C to unmount.")
	if err := fusefs.Serve(ctx, st, *mp); err != nil {
		return fmt.Errorf("FUSE mount failed (%v) — fallback: stretchstore serve --dir %s", err, *dir)
	}
	return nil
}
