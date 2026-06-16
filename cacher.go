package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
)

type cacher struct {
	dir string
}

func newCacher() (*cacher, error) {
	tmp, err := os.MkdirTemp("", "lfsproxy")
	if err != nil {
		return nil, err
	}
	return &cacher{tmp}, nil
}

func (c *cacher) Close() error {
	return os.RemoveAll(c.dir)
}

func (c *cacher) Put(ctx context.Context, name string, content io.ReadSeeker) (err error) {
	h := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(h[:])
	file := filepath.Join(c.dir, hash)

	f, err := os.CreateTemp(c.dir, "cache-tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())

	if _, err := io.Copy(f, content); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	if err := os.Chmod(f.Name(), 0o644); err != nil {
		return err
	}

	// Publish the fully-written temp file into its final location with an
	// atomic rename, so a concurrent Get never observes a half-written entry.
	// rename(2) replaces any existing file in place; that's safe here because
	// entries are keyed by a hash of the name, so two writers for the same key
	// always produce identical content.
	return os.Rename(f.Name(), file)
}

func (c *cacher) Get(ctx context.Context, name string) (io.ReadCloser, error) {
	h := sha256.Sum256([]byte(name))
	hash := hex.EncodeToString(h[:])
	file := filepath.Join(c.dir, hash)

	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}

	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &struct {
		*os.File
		os.FileInfo
	}{f, fi}, nil
}
