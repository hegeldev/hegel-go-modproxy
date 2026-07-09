package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

// copyFunc allows testing the cacher.
var copyFunc = io.Copy

type cacher struct {
	mu    sync.RWMutex
	files map[string]*os.File
	limit int
}

func newCacher() *cacher {
	return &cacher{files: map[string]*os.File{}, limit: 128}
}

func (c *cacher) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	var errs []error
	for _, f := range c.files {
		errs = append(errs, f.Close())
	}
	clear(c.files)
	return errors.Join(errs...)
}

func (c *cacher) Put(ctx context.Context, name string, content io.ReadSeeker) (err error) {
	closeOnError := func(c io.Closer) {
		if err != nil {
			_ = c.Close()
		}
	}

	h := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(h[:])

	f, err := os.CreateTemp("", "gomodcache")
	if err != nil {
		return err
	}
	defer closeOnError(f)

	// Immediately unlink the file so that closing it frees up disk space.
	if err := os.Remove(f.Name()); err != nil {
		return err
	}

retryWrite:
	_, err = copyFunc(f, content)
	if errors.Is(err, unix.ENOSPC) {
		if c.evictRandomFile() {
			offset, err := f.Seek(0, io.SeekCurrent)
			if err != nil {
				return err
			}

			if _, err := content.Seek(offset, io.SeekStart); err != nil {
				return err
			}

			goto retryWrite
		}

		return err
	} else if err != nil {
		return err
	}

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if old := c.files[hash]; old != nil {
		_ = old.Close()
	} else if len(c.files) >= c.limit {
		c.evictRandomFileLocked()
	}

	c.files[hash] = f
	return nil
}

func (c *cacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	h := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(h[:])

	c.mu.RLock()
	defer c.mu.RUnlock()

	f := c.files[hash]
	if f == nil {
		return nil, os.ErrNotExist
	}

	f, err := dupFile(f)
	if err != nil {
		return nil, err
	}

	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &readAtFile{
		f:        f,
		FileInfo: fi,
	}, nil
}

type readAtFile struct {
	f *os.File
	os.FileInfo
	offset int64
}

var _ io.ReadSeeker = (*readAtFile)(nil)

func (f *readAtFile) Read(p []byte) (int, error) {
	n, err := f.f.ReadAt(p, f.offset)
	f.offset += int64(n)
	return n, err
}

// Seek keeps cached responses compatible with http.ServeContent/Range requests.
// It only updates the wrapper offset; reads use ReadAt so duped descriptors do
// not share a mutable file offset.
func (f *readAtFile) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = f.offset + offset
	case io.SeekEnd:
		abs = f.Size() + offset
	default:
		return 0, os.ErrInvalid
	}
	if abs < 0 {
		return 0, os.ErrInvalid
	}
	f.offset = abs
	return abs, nil
}

func (f *readAtFile) Close() error {
	return f.f.Close()
}

func (c *cacher) evictRandomFile() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.evictRandomFileLocked()
}

func (c *cacher) evictRandomFileLocked() bool {
	for hash, f := range c.files {
		delete(c.files, hash)
		_ = f.Close()
		return true
	}

	return false
}

func dupFile(f *os.File) (*os.File, error) {
	raw, err := f.SyscallConn()
	if err != nil {
		return nil, err
	}

	var dup int
	var dupErr error
	err = raw.Control(func(fd uintptr) {
		dup, dupErr = unix.FcntlInt(fd, unix.F_DUPFD_CLOEXEC, 0)
	})
	if err != nil {
		return nil, err
	}
	if dupErr != nil {
		return nil, dupErr
	}

	return os.NewFile(uintptr(dup), f.Name()), nil
}
