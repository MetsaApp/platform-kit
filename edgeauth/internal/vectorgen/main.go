// Command vectorgen writes the edgeauth conformance vectors:
//
//	go generate ./edgeauth
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/MetsaApp/platform-kit/edgeauth/internal/vectors"
)

func main() {
	out := flag.String("o", "testdata/vectors.json", "file to write")
	flag.Parse()
	if err := run(*out); err != nil {
		fmt.Fprintln(os.Stderr, "vectorgen:", err)
		os.Exit(1)
	}
}

func run(out string) error {
	data, err := vectors.Generate()
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	if err = os.WriteFile(out, data, 0o644); err != nil { //nolint:gosec // the vectors are public test data
		return fmt.Errorf("write %s: %w", out, err)
	}
	return nil
}
