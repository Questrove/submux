package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	"submux/internal/releasepolicy"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout, stderr io.Writer) int {
	if len(arguments) == 0 || arguments[0] != "matrix" {
		fmt.Fprintln(stderr, "usage: submux-release-check matrix --file PATH")
		return 2
	}
	flags := flag.NewFlagSet("matrix", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("file", "", "runtime support matrix JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *name == "" {
		fmt.Fprintln(stderr, "matrix requires --file and no positional arguments")
		return 2
	}
	matrix, err := releasepolicy.LoadSupportMatrix(*name)
	if err != nil {
		fmt.Fprintf(stderr, "validate Runtime support matrix: %v\n", err)
		return 1
	}
	stable := 0
	preview := 0
	for _, target := range matrix.Entries {
		if target.Status == "stable" {
			stable++
		} else {
			preview++
		}
	}
	fmt.Fprintf(stdout, "Runtime support matrix valid: %d stable, %d preview\n", stable, preview)
	return 0
}
