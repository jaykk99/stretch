//go:build windows

package main

// Windows builds ship without FUSE (go-fuse has no Windows backend).
// The volume is still fully usable via `serve`.

import (
	"flag"
	"fmt"
)

func cmdMount(args []string) error {
	fs := flag.NewFlagSet("mount", flag.ExitOnError)
	dir := fs.String("dir", "", "volume directory")
	fs.Parse(args)
	_ = dir
	return fmt.Errorf("FUSE mount is not available in Windows builds — " +
		"use: stretchstore serve --dir <vol>  (local HTTP/WebDAV file browser with the virtual disk.img)")
}
