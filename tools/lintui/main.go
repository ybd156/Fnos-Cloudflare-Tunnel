// Command lintui checks the web UI's selector contract against the frontend
// modules under cloudflared/app/www.
//
//	go run ./tools/lintui
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/lintui"
	"github.com/ybd156/fnos-cloudflared/tools/internal/reporoot"
)

func main() {
	root := flag.String("root", "", "repository root (default: discovered from the working directory)")
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lintui: %v\n", err)
		os.Exit(1)
	}
	if err := lintui.Run(repo, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lintui: %v\n", err)
		os.Exit(1)
	}
}
