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
	if len(arguments) == 0 {
		printUsage(stderr)
		return 2
	}
	switch arguments[0] {
	case "matrix":
		return runMatrix(arguments[1:], stdout, stderr)
	case "scan":
		return runScan(arguments[1:], stdout, stderr)
	case "mihomo":
		return runMihomo(arguments[1:], stdout, stderr)
	case "tuf-parity":
		return runTUFParity(arguments[1:], stdout, stderr)
	default:
		printUsage(stderr)
		return 2
	}
}

func printUsage(writer io.Writer) {
	fmt.Fprintln(writer, "usage:")
	fmt.Fprintln(writer, "  submux-release-check matrix --file PATH")
	fmt.Fprintln(writer, "  submux-release-check scan --dir ABS --policy PATH")
	fmt.Fprintln(writer, "  submux-release-check mihomo --file PATH [--assets-dir ABS]")
	fmt.Fprintln(writer, "  submux-release-check tuf-parity --root ABS --bundle ABS --repository ABS --platform PLATFORM --arch ARCH --version VERSION --kind mihomo|product")
}

func runMatrix(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("matrix", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("file", "", "runtime support matrix JSON")
	if err := flags.Parse(arguments); err != nil {
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
