//go:build linux || darwin || android

// Package fusefs presents a StretchStore volume as a FUSE filesystem.
//
// The mount contains a single file, "disk.img", whose apparent size is the
// full logical size (e.g. 1000GB). Reads and writes pass through the
// engine: chunk → zstd → dedup → physical store. `df` on the mount shows
// the thin-provisioned size; `stretchstore status` shows the truth.
package fusefs

import (
	"context"
	"errors"
	"syscall"

	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"

	"stretchstore/internal/store"
)

const fileName = "disk.img"

// Serve mounts the volume at mountpoint and blocks until unmounted
// (or ctx is cancelled). On Linux it needs /dev/fuse and the fusermount
// helper — stock on Chromebook Linux (Crostini) after `sudo apt install fuse3`.
func Serve(ctx context.Context, st *store.Store, mountpoint string) error {
	root := &rootNode{st: st}
	opts := &fs.Options{}
	opts.MountOptions.Options = []string{"fsname=stretchstore"}
	server, err := fs.Mount(mountpoint, root, opts)
	if err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		server.Unmount()
	}()
	server.WaitMount()
	server.Serve()
	return nil
}

type rootNode struct {
	fs.Inode
	st *store.Store
}

var _ = (fs.NodeLookuper)((*rootNode)(nil))

func (r *rootNode) Lookup(ctx context.Context, name string, out *fuse.EntryOut) (*fs.Inode, syscall.Errno) {
	if name != fileName {
		return nil, syscall.ENOENT
	}
	s := r.st.Status()
	out.Mode = 0o644
	out.Size = s.FileSize
	st := r.st
	ch := r.NewInode(ctx, &fileNode{st: st}, fs.StableAttr{Mode: syscall.S_IFREG})
	return ch, 0
}

func (r *rootNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o755 | syscall.S_IFDIR
	return 0
}

func (r *rootNode) Readdir(ctx context.Context) (fs.DirStream, syscall.Errno) {
	return fs.NewListDirStream([]fuse.DirEntry{
		{Name: fileName, Mode: syscall.S_IFREG, Ino: 2},
	}), 0
}

type fileNode struct {
	fs.Inode
	st *store.Store
}

func (f *fileNode) Getattr(ctx context.Context, fh fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	s := f.st.Status()
	out.Mode = 0o644 | syscall.S_IFREG
	out.Size = s.FileSize
	out.Blocks = s.PhysicalUsed / 512
	return 0
}

func (f *fileNode) Setattr(ctx context.Context, fh fs.FileHandle, in *fuse.SetAttrIn, out *fuse.AttrOut) syscall.Errno {
	if size, ok := in.GetSize(); ok {
		if err := f.st.Truncate(size); err != nil {
			return syscall.EINVAL
		}
	}
	return f.Getattr(ctx, fh, out)
}

func (f *fileNode) Open(ctx context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	return &fileHandle{st: f.st}, fuse.FOPEN_KEEP_CACHE, 0
}

type fileHandle struct {
	st *store.Store
}

func (h *fileHandle) Read(ctx context.Context, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	s := h.st.Status()
	if off >= int64(s.FileSize) {
		return fuse.ReadResultData(nil), 0
	}
	if off+int64(len(dest)) > int64(s.FileSize) {
		dest = dest[:int64(s.FileSize)-off]
	}
	if _, err := h.st.ReadAt(dest, off); err != nil {
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(dest), 0
}

func (h *fileHandle) Write(ctx context.Context, data []byte, off int64) (uint32, syscall.Errno) {
	n, err := h.st.WriteAt(data, off)
	if err != nil {
		if errors.Is(err, store.ErrNoSpace) {
			return 0, syscall.ENOSPC
		}
		return 0, syscall.EIO
	}
	return uint32(n), 0
}

func (h *fileHandle) Flush(ctx context.Context) syscall.Errno {
	return 0 // every WriteAt already fsyncs through the journal
}
