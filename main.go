package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"github.com/goproxy/goproxy"
)

func run() error {
	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}

	proxy, cleanup, err := newProxy()
	if err != nil {
		return err
	}
	defer cleanup()

	log.Printf("Go module proxy listening on :%s", port)
	return http.ListenAndServe(":"+port, proxy)
}

// newProxy builds the module-proxy handler together with a cleanup function
// that releases its cache. It first verifies git-lfs is installed and
// configured, since the proxy relies on the smudge filter at fetch time.
func newProxy() (http.Handler, func() error, error) {
	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		return nil, nil, fmt.Errorf("git lfs: %w", err)
	}

	// `git config get` exits non-zero when the key is unset, which is exactly
	// the case when `git lfs install` has never run. Treat both a non-zero exit
	// and empty output as "not configured" so the actionable message is reached.
	out, _ := exec.Command("git", "config", "get", "filter.lfs.process").Output()
	if strings.TrimSpace(string(out)) == "" {
		return nil, nil, errors.New("git-lfs is not configured; run `git lfs install`")
	}

	c, err := newCacher()
	if err != nil {
		return nil, nil, err
	}

	proxy := &goproxy.Goproxy{
		Fetcher: newLFSFetcher(map[string]string{
			"hegel.dev/go/hegel": "github.com/hegeldev/hegel-go",
		}),
		Cacher: c,
	}
	return proxy, c.Close, nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
