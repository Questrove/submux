package main

import (
	"flag"
	"fmt"
	"io"

	"submux/internal/releasepolicy"
)

func runMihomo(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("mihomo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	name := flags.String("file", "", "Mihomo release provenance JSON")
	assets := flags.String("assets-dir", "", "optional downloaded asset verification directory")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *name == "" {
		fmt.Fprintln(stderr, "mihomo requires --file and no positional arguments")
		return 2
	}
	provenance, err := releasepolicy.LoadMihomoProvenance(*name)
	if err != nil {
		fmt.Fprintf(stderr, "validate Mihomo provenance: %v\n", err)
		return 1
	}
	if *assets != "" {
		if err := releasepolicy.VerifyMihomoAssets(*assets, provenance); err != nil {
			fmt.Fprintf(stderr, "verify Mihomo assets: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "Mihomo provenance valid: %s at %s with %d platform assets\n",
		provenance.Version, provenance.Commit, len(provenance.Assets))
	return 0
}
