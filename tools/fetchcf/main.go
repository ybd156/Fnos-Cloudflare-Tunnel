// Command fetchcf downloads and verifies the pinned upstream cloudflared
// binary, or checks that the local copy still matches the pin.
//
//	go run ./tools/fetchcf           # fetch if missing or wrong
//	go run ./tools/fetchcf -check    # verify only; never downloads
//	go run ./tools/fetchcf -force    # re-fetch even if present
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/fetchcf"
	"github.com/ybd156/fnos-cloudflared/tools/internal/reporoot"
)

func main() {
	root := flag.String("root", "", "repository root (default: discovered from the working directory)")
	check := flag.Bool("check", false, "verify the local copy only; never download")
	force := flag.Bool("force", false, "re-fetch even when a correct copy is present")
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetchcf: %v\n", err)
		os.Exit(1)
	}
	if err := fetchcf.Run(repo, fetchcf.Options{Check: *check, Force: *force}, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fetchcf: %v\n", err)
		os.Exit(1)
	}
}
