package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/mod/module"
	modzip "golang.org/x/mod/zip"
)

// materialize copies a (possibly unlinked) zip file to a real path so that
// path-based helpers like modzip.CheckZip can open it.
func materialize(t *testing.T, f *os.File) string {
	t.Helper()

	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("seek: %v", err)
	}
	dst := filepath.Join(t.TempDir(), "module.zip")
	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create temp zip: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(out.Name()) })
	if _, err := io.Copy(out, f); err != nil {
		out.Close()
		t.Fatalf("copy zip: %v", err)
	}
	if err := out.Close(); err != nil {
		t.Fatalf("close temp zip: %v", err)
	}
	return dst
}

func TestRewriteModule(t *testing.T) {
	const (
		from    = "github.com/hegeldev/hegel-go"
		to      = "hegel.dev/go/hegel"
		version = "v1.0.0"
	)

	var buf bytes.Buffer
	src := module.Version{Path: from, Version: version}
	if err := modzip.CreateFromDir(&buf, src, filepath.Join("testdata", "module")); err != nil {
		t.Fatalf("create module zip: %v", err)
	}
	raw := buf.Bytes()

	z, err := rewriteModule(bytes.NewReader(raw), int64(len(raw)), from, to)
	if err != nil {
		t.Fatalf("rewriteModule: %v", err)
	}
	defer z.Close()

	// The rewritten archive must be a valid module zip for the vanity path.
	dst := module.Version{Path: to, Version: version}
	cf, err := modzip.CheckZip(dst, materialize(t, z))
	if err != nil {
		t.Fatalf("CheckZip: %v", err)
	}
	if err := cf.Err(); err != nil {
		t.Fatalf("rewritten zip is not a valid module zip: %v", err)
	}
}

func TestRewriteModuleRejectsUnexpectedPrefix(t *testing.T) {
	const (
		from    = "github.com/hegeldev/hegel-go"
		to      = "hegel.dev/go/hegel"
		version = "v1.0.0"
	)

	// Build a zip whose entries carry a different module prefix.
	var buf bytes.Buffer
	m := module.Version{Path: "github.com/someone/else", Version: version}
	if err := modzip.CreateFromDir(&buf, m, filepath.Join("testdata", "module")); err != nil {
		t.Fatalf("create module zip: %v", err)
	}
	raw := buf.Bytes()

	z, err := rewriteModule(bytes.NewReader(raw), int64(len(raw)), from, to)
	if err == nil {
		z.Close()
		t.Fatal("expected error for entries lacking the module prefix, got nil")
	}
}
