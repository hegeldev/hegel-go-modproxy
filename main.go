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

	if err := exec.Command("git", "lfs", "version").Run(); err != nil {
		return fmt.Errorf("git lfs: %w", err)
	}

	// `git config get` exits non-zero when the key is unset, which is exactly
	// the case when `git lfs install` has never run. Treat both a non-zero exit
	// and empty output as "not configured" so the actionable message is reached.
	out, _ := exec.Command("git", "config", "get", "filter.lfs.process").Output()
	if strings.TrimSpace(string(out)) == "" {
		return errors.New("git-lfs is not configured; run `git lfs install`")
	}

	c, err := newCacher()
	if err != nil {
		return err
	}
	defer c.Close()

	proxy := &goproxy.Goproxy{
		Fetcher: newLFSFetcher(map[string]string{
			"hegel.dev/go/hegel": "github.com/hegeldev/hegel-go",
		}),
		Cacher: c,
	}

	log.Printf("Go module proxy listening on :%s", port)
	return http.ListenAndServe(":"+port, proxy)
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		os.Exit(1)
	}
}
