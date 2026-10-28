// Command lintshell runs the static checks for the lifecycle scripts and the
// build wrappers.
//
//	go run ./tools/lintshell
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/lintshell"
	"github.com/ybd156/fnos-cloudflared/tools/internal/reporoot"
)

func main() {
	root := flag.String("root", "", "repository root (default: discovered from the working directory)")
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lintshell: %v\n", err)
		os.Exit(1)
	}
	if err := lintshell.Run(repo, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "lintshell: %v\n", err)
		os.Exit(1)
	}
}
