package main

// The no-FUSE fallback: serve the volume as a virtual disk.img over
// HTTP/WebDAV on localhost. Works on every platform, including Windows
// builds and Android/Termux.

import (
	"context"
	"flag"
	"fmt"
	"os/signal"
	"syscall"

	"stretchstore/internal/httpserve"
	"stretchstore/internal/store"
)

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	dir := fs.String("dir", "", "volume directory")
	addr := fs.String("addr", "127.0.0.1:8080", "listen address (keep localhost unless you know why)")
	fs.Parse(args)
	if *dir == "" {
		return fmt.Errorf("--dir is required")
	}
	st, err := store.Open(*dir)
	if err != nil {
		return err
	}
	defer st.Close()
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return httpserve.Serve(ctx, st, *addr)
}
