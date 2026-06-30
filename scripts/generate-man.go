package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/redentordev/tako-cli/cmd"
)

func main() {
	outputDir := flag.String("dir", "man", "directory for generated man pages")
	flag.Parse()

	if err := os.MkdirAll(*outputDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "failed to create manual page directory: %v\n", err)
		os.Exit(1)
	}
	if err := cmd.GenerateManPages(*outputDir); err != nil {
		fmt.Fprintf(os.Stderr, "failed to generate manual pages: %v\n", err)
		os.Exit(1)
	}
}
