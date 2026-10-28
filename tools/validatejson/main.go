// Command validatejson checks that each named file is parseable JSON.
//
//	go run ./tools/validatejson cloudflared/config/privilege cloudflared/wizard/*
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/ybd156/fnos-cloudflared/tools/internal/validatejson"
)

func main() {
	flag.Usage = func() {
		fmt.Fprintln(os.Stderr, "usage: validatejson <file> [<file> ...]")
	}
	flag.Parse()

	if err := validatejson.Run(flag.Args(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintf(os.Stderr, "validatejson: %v\n", err)
		os.Exit(1)
	}
}
