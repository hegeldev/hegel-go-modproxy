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
//   - A local checksum-database sentinel asserts the proxy never verifies the
//     upstream module against a checksum database (which would leak the private
//     repo into the public transparency log).
//
// It skips unless git, git-lfs and go are present (i.e. in practice it runs
// inside the test container, since the proxy requires git-lfs at runtime
// anyway).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

const (
	vanity   = "hegel.dev/go/hegel"
	upstream = "github.com/hegeldev/hegel-go"

	// A throwaway checksum-database verifier key (generated for this test only,
	// never the real sum.golang.org key). It lets us point the server-side
	// GOSUMDB at a local sentinel so any attempt by the proxy to verify the
	// upstream module against a checksum database is recorded and refused.
	sumdbKey = "lfsproxy-test-sumdb+d82fdfef+Ac0wBCLSg/823lSLXD3ZHtjdKZJXxXOYpO0d+Zfj+Fyn"
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

	// Stand up the proxy in-process via the same newProxy() main() uses. The
	// server-side fetcher must fetch the upstream module directly via git
	// (GOPROXY=direct) so git-lfs smudging happens; insteadOf then keeps that
	// fetch on the local filesystem.
	//
	// newProxy builds its GoFetcher from a snapshot of os.Environ(), so
	// configure the fetch environment in the process via t.Setenv and let the
	// real constructor pick it up, rather than reaching past it to overwrite
	// fetcher.Env. (GOPROXY=direct and GOSUMDB=off are pinned by newLFSFetcher.)
	//
	// A checksum-database sentinel. The proxy must never create GOSUMDB entries
	// for the upstream module it fetches: doing so would publish the private
	// upstream repo into the public transparency log. This server records any
	// request and fails it, so a single hit both records the violation and
	// breaks the download. With the proxy configured correctly it is never
	// contacted.
	sumdb := &sumdbRecorder{}
	sumdbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sumdb.record(r.URL.Path)
		http.Error(w, "proxy must not contact the checksum database", http.StatusInternalServerError)
	}))
	t.Cleanup(sumdbSrv.Close)

	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", globalCfg)
	// Deliberately do NOT set GOPROXY=direct or GOSUMDB=off here: newProxy must
	// provide those guarantees itself (see newLFSFetcher). Instead, point GOSUMDB
	// at our own local sentinel (never the real sum.golang.org) and leave
	// GONOSUMDB unset so nothing excludes the upstream repo that is actually
	// fetched. If a regression re-enabled checksum-db verification for that fetch,
	// the sentinel below would be hit. With GOSUMDB=off pinned by newLFSFetcher,
	// this value is overridden and no checksum-db client is ever built.
	t.Setenv("GOSUMDB", sumdbKey+" "+sumdbSrv.URL)
	t.Setenv("GONOSUMDB", "")
	t.Setenv("GOMODCACHE", t.TempDir())
	t.Setenv("GOCACHE", t.TempDir())
	// -modcacherw leaves the module cache writable so t.TempDir's RemoveAll can
	// clean it up; go otherwise writes cache entries read-only.
	t.Setenv("GOFLAGS", "-modcacherw")

	// newProxy uses the production module map; the vanity/upstream consts above
	// mirror its single entry (hegel.dev/go/hegel -> github.com/hegeldev/hegel-go).
	handler, cleanup, err := newProxy()
	if err != nil {
		t.Fatalf("newProxy: %v", err)
	}
	t.Cleanup(func() { _ = cleanup() })

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	// A stand-in for the hegel.dev vanity server. The client resolves the module
	// the way the real world does: GOPROXY=direct makes `go` fetch
	// https://hegel.dev/go/hegel?go-get=1 and follow the go-import meta tag,
	// rather than being handed the proxy URL directly.
	//
	// `go` can't reach the real hegel.dev, so HTTP(S)_PROXY routes that lookup
	// here. This handler doubles as a forward proxy: it refuses the CONNECT that
	// go's https-first attempt makes (so go falls back to plain HTTP), then
	// answers the proxied GET with a meta tag pointing module downloads ("mod")
	// at our proxy. The root echoes the requested path, so a disallowed module
	// (hegel.dev/go/forbidden) also routes to the proxy and is rejected there,
	// exercising the real allow-list rather than failing at resolution.
	vanitySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			http.Error(w, "no CONNECT", http.StatusBadGateway)
			return
		}
		root := r.Host + r.URL.Path // e.g. hegel.dev/go/hegel
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprintf(w, `<!DOCTYPE html><meta name="go-import" content="%s mod %s">`, root, srv.URL)
	}))
	t.Cleanup(vanitySrv.Close)

	clientEnv := mergeEnv(map[string]string{
		"HOME": home,
		// Resolve via go-import meta tags (see vanitySrv) instead of a direct
		// GOPROXY. GOINSECURE lets the hegel.dev lookup fall back to plain HTTP;
		// HTTP(S)_PROXY routes it to vanitySrv; NO_PROXY keeps the subsequent
		// proxy fetch (loopback) direct rather than bouncing through vanitySrv.
		"GOPROXY":     "direct",
		"GOINSECURE":  "hegel.dev",
		"HTTP_PROXY":  vanitySrv.URL,
		"HTTPS_PROXY": vanitySrv.URL,
		"NO_PROXY":    "127.0.0.1",
		"GOSUMDB":     "off",
		"GONOSUMDB":   "*",
		"GOPRIVATE":   "",
		"GOMODCACHE":  t.TempDir(),
		"GOCACHE":     t.TempDir(),
		"GOPATH":      t.TempDir(),
		"GOFLAGS":     "-modcacherw", // keep the cache writable for t.TempDir cleanup
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
		// The client computes and records its own checksum for the vanity module
		// (the go.sum entry the proxy must never create on its behalf). Its
		// presence confirms client-side checksumming still works.
		if mod.Sum == "" {
			t.Errorf("client recorded no checksum (Sum empty); want a go.sum entry for %s", vanity)
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

	t.Run("disallowed module returns 404 so go falls through", func(t *testing.T) {
		// A module the proxy does not serve must return 404 (not 500): only then
		// does `go` fall through to the next GOPROXY entry, which is how
		// transitive public dependencies get resolved by the public proxy.
		resp, err := http.Get(srv.URL + "/hegel.dev/go/forbidden/@v/list")
		if err != nil {
			t.Fatalf("GET forbidden module: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("status = %d, want %d (404 enables GOPROXY fall-through)", resp.StatusCode, http.StatusNotFound)
		}
	})

	if hits := sumdb.hits(); len(hits) != 0 {
		t.Fatalf("proxy contacted the checksum database %d time(s); it must never do so for upstream fetches:\n%s",
			len(hits), strings.Join(hits, "\n"))
	}
}

// sumdbRecorder records the request paths a checksum-database sentinel receives.
type sumdbRecorder struct {
	mu   sync.Mutex
	seen []string
}

func (r *sumdbRecorder) record(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, path)
}

func (r *sumdbRecorder) hits() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
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
	Path     string
	Version  string
	Info     string
	GoMod    string
	Zip      string
	Dir      string
	Sum      string // h1: hash the client computed and would record in go.sum
	GoModSum string
	Error    string
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
