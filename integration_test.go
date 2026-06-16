package main

// This is an end-to-end integration test for the proxy. It is hermetic and
// requires no internet access:
//
//   - A throwaway git repository stands in for the upstream GitHub repo
//     (github.com/hegeldev/hegel-go). It contains a git-lfs tracked asset with
//     known bytes.
//   - git's url.<base>.insteadOf rewrites the GitHub URL to a file:// path
//     pointing at that repo, so the proxy's direct module fetch never leaves
//     the machine.
//   - The proxy is constructed in-process and served via httptest.
//   - The real `go` binary downloads the vanity module through the proxy and we
//     assert the git-lfs asset arrives smudged (real content, not a pointer).
//
// It only runs under `-tags=integration` and skips unless git, git-lfs and go
// are present (i.e. in practice it runs inside the test container, since the
// proxy requires git-lfs at runtime anyway).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goproxy/goproxy"
)

const (
	vanity   = "hegel.dev/go/hegel"
	upstream = "github.com/hegeldev/hegel-go"
)

func TestIntegrationLFSResolved(t *testing.T) {
	requireBinaries(t)

	home := t.TempDir()
	upRepo := t.TempDir()

	// The LFS asset: deterministic, far larger than an LFS pointer (~130 bytes)
	// so a non-smudged checkout is unmistakable.
	asset := bytes.Repeat([]byte("hegel-lfs-payload\n"), 4096)

	// A git config used by every git/go invocation in this test. It carries an
	// identity (so commits work) and the insteadOf rewrite that keeps fetches
	// offline. The system config (where `git lfs install --system` registered
	// the smudge filter) is left untouched.
	globalCfg := filepath.Join(t.TempDir(), "gitconfig")
	write(t, globalCfg, "[user]\n"+
		"\tname = integration\n"+
		"\temail = integration@example.com\n"+
		"[url \"file://"+upRepo+"\"]\n"+
		"\tinsteadOf = https://"+upstream+"\n")

	gitEnv := mergeEnv(map[string]string{
		"HOME":              home,
		"GIT_CONFIG_GLOBAL": globalCfg,
	})

	buildUpstream(t, upRepo, asset, gitEnv)

	// Stand up the proxy in-process. The server-side fetcher must fetch the
	// upstream module directly via git (GOPROXY=direct) so git-lfs smudging
	// happens; insteadOf then keeps that fetch on the local filesystem.
	f := newLFSFetcher(map[string]string{vanity: upstream})
	f.fetcher.Env = mergeEnv(map[string]string{
		"HOME":              home,
		"GIT_CONFIG_GLOBAL": globalCfg,
		"GOPROXY":           "direct",
		"GOSUMDB":           "off",
		"GONOSUMDB":         "*",
		"GOPRIVATE":         vanity,
		"GOMODCACHE":        t.TempDir(),
		"GOCACHE":           t.TempDir(),
		"GOFLAGS":           "",
	})

	c, err := newCacher()
	if err != nil {
		t.Fatalf("newCacher: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	srv := httptest.NewServer(&goproxy.Goproxy{Fetcher: f, Cacher: c})
	t.Cleanup(srv.Close)

	clientEnv := mergeEnv(map[string]string{
		"HOME":       home,
		"GOPROXY":    srv.URL,
		"GOSUMDB":    "off",
		"GONOSUMDB":  "*",
		"GOPRIVATE":  "",
		"GOMODCACHE": t.TempDir(),
		"GOCACHE":    t.TempDir(),
		"GOPATH":     t.TempDir(),
		"GOFLAGS":    "",
	})

	t.Run("latest resolves and smudges LFS", func(t *testing.T) {
		mod := goModDownload(t, clientEnv, vanity+"@latest")

		if mod.Error != "" {
			t.Fatalf("download error: %s", mod.Error)
		}
		if mod.Path != vanity {
			t.Errorf("Path = %q, want %q", mod.Path, vanity)
		}
		// @latest must resolve to the highest semver tag we created.
		if mod.Version != "v1.1.0" {
			t.Errorf("Version = %q, want v1.1.0", mod.Version)
		}

		got, err := os.ReadFile(filepath.Join(mod.Dir, "model.bin"))
		if err != nil {
			t.Fatalf("read model.bin from %s: %v", mod.Dir, err)
		}
		if bytes.HasPrefix(got, []byte("version https://git-lfs")) {
			t.Fatalf("model.bin is an unsmudged LFS pointer, not content:\n%s", got)
		}
		if !bytes.Equal(got, asset) {
			t.Fatalf("model.bin content mismatch: got %d bytes, want %d", len(got), len(asset))
		}
	})

	t.Run("pinned version resolves", func(t *testing.T) {
		mod := goModDownload(t, clientEnv, vanity+"@v1.0.0")
		if mod.Error != "" {
			t.Fatalf("download error: %s", mod.Error)
		}
		if mod.Version != "v1.0.0" {
			t.Errorf("Version = %q, want v1.0.0", mod.Version)
		}
	})

	t.Run("disallowed module is rejected", func(t *testing.T) {
		// go exits non-zero; -json still emits an object carrying the error.
		out, err := runGo(clientEnv, "mod", "download", "-json", "hegel.dev/go/forbidden@latest")
		if err == nil {
			t.Fatalf("expected failure for disallowed module, got success:\n%s", out)
		}
	})
}

// buildUpstream creates a git repo at dir standing in for the upstream GitHub
// repository: an LFS-tracked asset plus two tagged versions.
func buildUpstream(t *testing.T, dir string, asset []byte, env []string) {
	t.Helper()

	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}

	git("init", "-q", "-b", "main")
	git("lfs", "track", "*.bin")
	write(t, filepath.Join(dir, "go.mod"), "module "+upstream+"\n\ngo 1.25.0\n")
	write(t, filepath.Join(dir, "hegel.go"), "package hegel\n\n// Greeting is served from the upstream fixture.\nfunc Greeting() string { return \"owl of minerva\" }\n")
	if err := os.WriteFile(filepath.Join(dir, "model.bin"), asset, 0o644); err != nil {
		t.Fatalf("write model.bin: %v", err)
	}
	git("add", "-A")
	git("commit", "-qm", "v1.0.0")
	git("tag", "v1.0.0")

	// A second version so @latest has something to choose.
	write(t, filepath.Join(dir, "hegel.go"), "package hegel\n\n// Greeting is served from the upstream fixture.\nfunc Greeting() string { return \"owl of minerva flies at dusk\" }\n")
	git("add", "-A")
	git("commit", "-qm", "v1.1.0")
	git("tag", "v1.1.0")
}

type downloaded struct {
	Path    string
	Version string
	Info    string
	GoMod   string
	Zip     string
	Dir     string
	Error   string
}

func goModDownload(t *testing.T, env []string, module string) downloaded {
	t.Helper()
	out, err := runGo(env, "mod", "download", "-json", module)
	if err != nil {
		t.Fatalf("go mod download %s: %v\n%s", module, err, out)
	}
	var mod downloaded
	if derr := json.NewDecoder(bytes.NewReader(out)).Decode(&mod); derr != nil {
		t.Fatalf("decode -json output: %v\n%s", derr, out)
	}
	return mod
}

func runGo(env []string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(context.Background(), "go", args...)
	cmd.Env = env
	return cmd.CombinedOutput()
}

func requireBinaries(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not found")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not found")
	}
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		t.Skip("git-lfs not found; run this test in the container")
	}
}

// mergeEnv returns os.Environ() with the given keys overridden (no duplicate
// keys, so getenv resolves predictably across platforms).
func mergeEnv(overrides map[string]string) []string {
	out := make([]string, 0, len(os.Environ())+len(overrides))
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := overrides[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range overrides {
		out = append(out, k+"="+v)
	}
	return out
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
