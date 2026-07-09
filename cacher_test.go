package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"testing"

	"golang.org/x/sys/unix"
)

func TestCacherPutGet(t *testing.T) {
	c := newCacher()
	t.Cleanup(func() { _ = c.Close() })

	const name = "example.com/module/@v/v1.0.0.zip"
	content := []byte("module zip content")

	if err := c.Put(context.Background(), name, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, err := c.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer got.Close()

	if size := got.(interface{ Size() int64 }).Size(); size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", size, len(content))
	}

	body, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	if !bytes.Equal(body, content) {
		t.Fatalf("content = %q, want %q", body, content)
	}
}

func TestCacherGetMissing(t *testing.T) {
	c := newCacher()

	got, err := c.Get(context.Background(), "missing")
	if err == nil {
		got.Close()
		t.Fatal("Get returned nil error for missing entry")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get error = %v, want os.ErrNotExist", err)
	}
}

func TestCacherGetReadersHaveIndependentOffsets(t *testing.T) {
	c := newCacher()
	t.Cleanup(func() { _ = c.Close() })

	const name = "example.com/module/@v/v1.0.0.zip"
	content := []byte("0123456789")

	if err := c.Put(context.Background(), name, bytes.NewReader(content)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	first, err := c.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get first: %v", err)
	}
	defer first.Close()

	second, err := c.Get(context.Background(), name)
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	defer second.Close()

	prefix := make([]byte, 4)
	if _, err := io.ReadFull(first, prefix); err != nil {
		t.Fatalf("ReadFull first: %v", err)
	}
	if !bytes.Equal(prefix, content[:4]) {
		t.Fatalf("first prefix = %q, want %q", prefix, content[:4])
	}

	secondBody, err := io.ReadAll(second)
	if err != nil {
		t.Fatalf("ReadAll second: %v", err)
	}
	if !bytes.Equal(secondBody, content) {
		t.Fatalf("second content = %q, want %q", secondBody, content)
	}

	firstRest, err := io.ReadAll(first)
	if err != nil {
		t.Fatalf("ReadAll first rest: %v", err)
	}
	if !bytes.Equal(firstRest, content[4:]) {
		t.Fatalf("first rest = %q, want %q", firstRest, content[4:])
	}
}

func TestCacherPutEvictsWhenLimitReached(t *testing.T) {
	c := newCacher()
	c.limit = 1
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Put(context.Background(), "first", bytes.NewReader([]byte("first content"))); err != nil {
		t.Fatalf("Put first: %v", err)
	}

	secondContent := []byte("second content")
	if err := c.Put(context.Background(), "second", bytes.NewReader(secondContent)); err != nil {
		t.Fatalf("Put second: %v", err)
	}

	if first, err := c.Get(context.Background(), "first"); err == nil {
		first.Close()
		t.Fatal("first entry survived limit eviction")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get first error = %v, want os.ErrNotExist", err)
	}

	second, err := c.Get(context.Background(), "second")
	if err != nil {
		t.Fatalf("Get second: %v", err)
	}
	defer second.Close()

	body, err := io.ReadAll(second)
	if err != nil {
		t.Fatalf("ReadAll second: %v", err)
	}
	if !bytes.Equal(body, secondContent) {
		t.Fatalf("second content = %q, want %q", body, secondContent)
	}
}

func TestCacherPutRetriesAfterENOSPCEvictingFile(t *testing.T) {
	c := newCacher()
	t.Cleanup(func() { _ = c.Close() })

	if err := c.Put(context.Background(), "victim", bytes.NewReader([]byte("old cached content"))); err != nil {
		t.Fatalf("Put victim: %v", err)
	}

	oldCopyFunc := copyFunc
	t.Cleanup(func() { copyFunc = oldCopyFunc })

	want := []byte("new cached content after retry")
	calls := 0
	copyFunc = func(dst io.Writer, src io.Reader) (int64, error) {
		calls++
		switch calls {
		case 1:
			buf := make([]byte, 8)
			n, err := src.Read(buf)
			if err != nil {
				return int64(n), err
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return int64(n), err
			}
			return int64(n), unix.ENOSPC
		case 2:
			return oldCopyFunc(dst, src)
		default:
			t.Fatalf("copyFunc called %d times, want 2", calls)
			return 0, nil
		}
	}

	if err := c.Put(context.Background(), "target", bytes.NewReader(want)); err != nil {
		t.Fatalf("Put target: %v", err)
	}
	if calls != 2 {
		t.Fatalf("copyFunc called %d times, want 2", calls)
	}

	if victim, err := c.Get(context.Background(), "victim"); err == nil {
		victim.Close()
		t.Fatal("victim entry survived ENOSPC eviction")
	} else if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get victim error = %v, want os.ErrNotExist", err)
	}

	got, err := c.Get(context.Background(), "target")
	if err != nil {
		t.Fatalf("Get target: %v", err)
	}
	defer got.Close()

	body, err := io.ReadAll(got)
	if err != nil {
		t.Fatalf("ReadAll target: %v", err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("target content = %q, want %q", body, want)
	}
}

func TestCacherPutReturnsENOSPCWhenNothingCanBeEvicted(t *testing.T) {
	c := newCacher()
	t.Cleanup(func() { _ = c.Close() })

	oldCopyFunc := copyFunc
	t.Cleanup(func() { copyFunc = oldCopyFunc })

	copyFunc = func(io.Writer, io.Reader) (int64, error) {
		return 0, unix.ENOSPC
	}

	err := c.Put(context.Background(), "target", bytes.NewReader([]byte("content")))
	if !errors.Is(err, unix.ENOSPC) {
		t.Fatalf("Put error = %v, want unix.ENOSPC", err)
	}

	got, err := c.Get(context.Background(), "target")
	if err == nil {
		got.Close()
		t.Fatal("target was cached despite ENOSPC")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Get target error = %v, want os.ErrNotExist", err)
	}
}
