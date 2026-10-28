// Command fpk normalises and verifies a built .fpk archive.
//
//	go run ./tools/fpk normalize dist/cloudflared.fpk
//	go run ./tools/fpk verify dist/cloudflared.fpk [cloudflared]
//	go run ./tools/fpk info dist/cloudflared.fpk
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/fpk"
)

const usage = `usage:
  fpk normalize <path-to.fpk>              rewrite permission bits in both tar layers and repair the manifest checksum
  fpk verify <path-to.fpk> [source-dir]    assert the artefact matches the source tree (default source-dir: cloudflared)
  fpk info <path-to.fpk>                   print size, SHA-256 and the manifest checksum`

func main() {
	flag.Usage = func() { fmt.Fprintln(os.Stderr, usage) }
	flag.Parse()

	args := flag.Args()
	if len(args) < 2 {
		fmt.Fprintln(os.Stderr, usage)
		os.Exit(2)
	}

	cmd, fpkPath := args[0], args[1]
	switch cmd {
	case "normalize":
		if _, err := fpk.Normalize(fpkPath, os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fpk normalize: %v\n", err)
			os.Exit(1)
		}
	case "verify":
		src := fpk.DefaultSource
		if len(args) > 2 {
			src = args[2]
		}
		if err := fpk.Verify(fpkPath, src, fpk.DefaultVerifyOptions(), os.Stdout); err != nil {
			fmt.Fprintf(os.Stderr, "fpk verify: %v\n", err)
			os.Exit(1)
		}
	case "info":
		info, err := fpk.Summary(fpkPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fpk info: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("  size              : %d bytes\n", info.Size)
		fmt.Printf("  sha256            : %s\n", info.SHA256)
		fmt.Printf("  manifest checksum : %s\n", info.ManifestChecksum)
	default:
		fmt.Fprintf(os.Stderr, "fpk: unknown subcommand %q\n\n%s\n", cmd, usage)
		os.Exit(2)
	}
}
