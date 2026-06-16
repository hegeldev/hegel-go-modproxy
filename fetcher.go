package main

import (
	"archive/zip"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/goproxy/goproxy"
)

type LFSFetcher struct {
	fetcher *goproxy.GoFetcher
	// Modules contains allowed Go module URLs mapped to git repositories.
	Modules map[string]string
}

func newLFSFetcher(modules map[string]string) *LFSFetcher {
	goprivate := strings.Join(slices.Collect(maps.Keys(modules)), ",")

	return &LFSFetcher{
		&goproxy.GoFetcher{
			Env: append(os.Environ(),
				"GOPRIVATE="+goprivate,
			),
		},
		modules,
	}
}

func (f *LFSFetcher) Query(ctx context.Context, path, query string) (string, time.Time, error) {
	path, ok := f.Modules[path]
	if !ok {
		return "", time.Time{}, errors.New("module is not allowed")
	}
	return f.fetcher.Query(ctx, path, query)
}

func (f *LFSFetcher) List(ctx context.Context, path string) ([]string, error) {
	path, ok := f.Modules[path]
	if !ok {
		return nil, errors.New("module is not allowed")
	}
	return f.fetcher.List(ctx, path)
}

func (f *LFSFetcher) Download(ctx context.Context, orig, version string) (info, mod, zipFile io.ReadSeekCloser, err error) {
	closeOnError := func(c io.Closer) {
		if c != nil && err != nil {
			_ = c.Close()
		}
	}

	path, ok := f.Modules[orig]
	if !ok {
		return nil, nil, nil, errors.New("module is not allowed")
	}

	info, mod, zipFile, err = f.fetcher.Download(ctx, path, version)
	if err != nil {
		return
	}
	defer closeOnError(info)
	defer closeOnError(mod)
	defer zipFile.Close()

	size, err := zipFile.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, nil, nil, err
	}

	if _, err := zipFile.Seek(0, io.SeekStart); err != nil {
		return nil, nil, nil, err
	}

	z, err := rewriteModule(seekReaderAt{zipFile}, size, path, orig)
	if err != nil {
		return nil, nil, nil, err
	}

	return info, mod, z, nil
}

type seekReaderAt struct {
	r io.ReadSeeker
}

func (sr seekReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if _, err := sr.r.Seek(off, io.SeekStart); err != nil {
		return 0, err
	}
	return sr.r.Read(p)
}

func rewriteModule(f io.ReaderAt, size int64, from, to string) (z *os.File, err error) {
	defer func() {
		if z != nil && err != nil {
			_ = z.Close()
			z = nil
		}
	}()

	z, err = os.CreateTemp("", "lfsproxy-module-zip")
	if err != nil {
		return nil, err
	}

	r, err := zip.NewReader(f, size)
	if err != nil {
		return nil, err
	}

	if err := os.Remove(z.Name()); err != nil {
		return nil, err
	}

	w := zip.NewWriter(z)
	for _, f := range r.File {
		if !strings.HasPrefix(f.Name, from) {
			return nil, fmt.Errorf("file %s doesn't have prefix %s", f.Name, from)
		}

		f.Name = to + f.Name[len(from):]
		if err := w.Copy(f); err != nil {
			return nil, err
		}
	}

	if err := w.Close(); err != nil {
		return nil, err
	}

	if _, err := z.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}

	return z, nil
}
