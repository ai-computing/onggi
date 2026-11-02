//go:build linux

// Benchmark-only stateful caller; production materializer/store stay unchanged.
package main

import (
	"fmt"
	"io"
	"os"
)

func runSequence(args []string, stdout, stderr io.Writer) int {
	fmt.Fprintln(stderr, "stateful sequence not implemented")
	return 1
}

func main() { os.Exit(runSequence(os.Args[1:], os.Stdout, os.Stderr)) }
