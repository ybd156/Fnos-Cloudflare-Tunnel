// Command icons regenerates the six app icons the package ships.
//
//	go run ./tools/icons
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/icons"
	"github.com/ybd156/fnos-cloudflared/tools/internal/reporoot"
)

func main() {
	root := flag.String("root", "", "repository root (default: discovered from the working directory)")
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "icons: %v\n", err)
		os.Exit(1)
	}
	if err := icons.Generate(repo, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "icons: %v\n", err)
		os.Exit(1)
	}
}
