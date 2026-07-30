package main

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"

	"submux/internal/releasepolicy"
)

func runScan(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("dir", "", "absolute release artifact directory")
	policyPath := flags.String("policy", "", "release artifact policy JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *root == "" || *policyPath == "" {
		fmt.Fprintln(stderr, "scan requires --dir and --policy")
		return 2
	}
	absolute, err := filepath.Abs(*root)
	if err != nil {
		fmt.Fprintf(stderr, "resolve artifact directory: %v\n", err)
		return 1
	}
	policy, err := releasepolicy.LoadArtifactPolicy(*policyPath)
	if err != nil {
		fmt.Fprintf(stderr, "load artifact policy: %v\n", err)
		return 1
	}
	audit, err := releasepolicy.AuditArtifacts(absolute, policy)
	if err != nil {
		fmt.Fprintf(stderr, "audit release artifacts: %v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "release artifact audit passed: %d files, %d bytes\n", audit.Files, audit.Bytes)
	for _, artifact := range audit.Artifacts {
		fmt.Fprintf(stdout, "artifact size: %s %d bytes\n", artifact.Path, artifact.Bytes)
	}
	for _, approval := range audit.ApprovedExceptions {
		fmt.Fprintf(stdout, "approved size exception: %s\n", approval)
	}
	return 0
}
