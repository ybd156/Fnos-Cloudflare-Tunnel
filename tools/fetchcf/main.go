// Command fetchcf downloads and verifies the pinned upstream cloudflared
// binary, or checks that the local copy still matches the pin.
//
//	go run ./tools/fetchcf           # fetch if missing or wrong
//	go run ./tools/fetchcf -check    # verify only; never downloads
//	go run ./tools/fetchcf -force    # re-fetch even if present
//	go run ./tools/fetchcf -latest   # move the pin to the newest upstream release
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
	latest := flag.Bool("latest", false, "move the pin to the newest upstream release, then fetch it")
	flag.Parse()

	repo, err := reporoot.Resolve(*root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetchcf: %v\n", err)
		os.Exit(1)
	}
	if *latest {
		if err := fetchcf.FollowLatest(repo, os.Stdout, os.Stderr); err != nil {
			fmt.Fprintf(os.Stderr, "fetchcf -latest: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := fetchcf.Run(repo, fetchcf.Options{Check: *check, Force: *force}, os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "fetchcf: %v\n", err)
		os.Exit(1)
	}
}
