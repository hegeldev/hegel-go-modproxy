package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
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
	return &LFSFetcher{
		&goproxy.GoFetcher{
			Env: append(os.Environ(),
				// Fetch upstream repos directly via git so the git-lfs smudge
				// filter runs (a public proxy would serve unsmudged pointers).
				"GOPROXY=direct",
				// Never consult the checksum database for upstream fetches.
				// Doing so would publish the private upstream module into the
				// public transparency log. We only serve allow-listed, vanity-
				// mapped modules; the client still records its own go.sum
				// entries for the vanity path, which is what we want.
				"GOSUMDB=off",
			),
		},
		modules,
	}
}

// lookup resolves a vanity module path to its upstream git repository. Modules
// outside the allow-list return an error wrapping fs.ErrNotExist, which goproxy
// renders as a 404 so the go command falls through to the next proxy in its
// GOPROXY list (e.g. the public proxy for transitive dependencies). A bare
// error would render as a fatal 500 instead.
func (f *LFSFetcher) lookup(path string) (string, error) {
	repo, ok := f.Modules[path]
	if !ok {
		return "", fmt.Errorf("module %q is not served by this proxy: %w", path, fs.ErrNotExist)
	}
	return repo, nil
}

func (f *LFSFetcher) Query(ctx context.Context, path, query string) (string, time.Time, error) {
	path, err := f.lookup(path)
	if err != nil {
		return "", time.Time{}, err
	}
	return f.fetcher.Query(ctx, path, query)
}

func (f *LFSFetcher) List(ctx context.Context, path string) ([]string, error) {
	path, err := f.lookup(path)
	if err != nil {
		return nil, err
	}
	return f.fetcher.List(ctx, path)
}

func (f *LFSFetcher) Download(ctx context.Context, orig, version string) (info, mod, zipFile io.ReadSeekCloser, err error) {
	closeOnError := func(c io.Closer) {
		if c != nil && err != nil {
			_ = c.Close()
		}
	}

	path, err := f.lookup(orig)
	if err != nil {
		return nil, nil, nil, err
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
	// io.ReaderAt requires a short read to return a non-nil error; a bare Read
	// may return fewer bytes than len(p) with err == nil, which archive/zip
	// does not tolerate. io.ReadFull fills p fully or reports io.EOF /
	// io.ErrUnexpectedEOF.
	return io.ReadFull(sr.r, p)
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
