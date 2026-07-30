package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"

	"submux/internal/runtimeupdate"
)

func runTUFParity(arguments []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("tuf-parity", flag.ContinueOnError)
	flags.SetOutput(stderr)
	rootPath := flags.String("root", "", "trusted root.json")
	bundle := flags.String("bundle", "", "offline verification bundle ZIP")
	repository := flags.String("repository", "", "public repository directory containing metadata and targets")
	platform := flags.String("platform", "", "target platform")
	arch := flags.String("arch", "", "target architecture")
	version := flags.String("version", "", "exact stable target version")
	kind := flags.String("kind", "", "mihomo or product")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *rootPath == "" || *bundle == "" || *repository == "" ||
		*platform == "" || *arch == "" || *version == "" ||
		(*kind != "mihomo" && *kind != "product") {
		fmt.Fprintln(stderr, "tuf-parity requires root, bundle, repository, platform, arch, version, and kind")
		return 2
	}
	rootBody, err := os.ReadFile(*rootPath)
	if err != nil {
		fmt.Fprintf(stderr, "read trusted Root: %v\n", err)
		return 1
	}
	repositoryPath, err := filepath.Abs(*repository)
	if err != nil {
		fmt.Fprintf(stderr, "resolve repository: %v\n", err)
		return 1
	}
	server := httptest.NewTLSServer(http.FileServer(http.Dir(repositoryPath)))
	defer server.Close()
	onlineState, err := os.MkdirTemp("", "submux-tuf-online-")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer os.RemoveAll(onlineState)
	offlineState, err := os.MkdirTemp("", "submux-tuf-offline-")
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer os.RemoveAll(offlineState)
	online := &runtimeupdate.Verifier{
		InitialRoot: rootBody,
		StateRoot:   onlineState,
		MetadataURL: server.URL + "/metadata/",
		TargetsURL:  server.URL + "/targets/",
		HTTPClient:  server.Client(),
	}
	offline := &runtimeupdate.Verifier{
		InitialRoot: rootBody,
		StateRoot:   offlineState,
	}
	ctx := context.Background()
	if *kind == "product" {
		onlineTarget, err := online.RefreshProductOnline(ctx, *platform, *arch, *version)
		if err != nil {
			fmt.Fprintf(stderr, "verify online product metadata: %v\n", err)
			return 1
		}
		onlineBody, err := online.DownloadProduct(ctx, onlineTarget)
		if err != nil {
			fmt.Fprintf(stderr, "download online product: %v\n", err)
			return 1
		}
		offlineTarget, offlineBody, err := offline.RefreshProductOffline(ctx, *bundle, *platform, *arch, *version)
		if err != nil {
			fmt.Fprintf(stderr, "verify offline product: %v\n", err)
			return 1
		}
		if !reflect.DeepEqual(onlineTarget, offlineTarget) || !bytes.Equal(onlineBody, offlineBody) {
			fmt.Fprintln(stderr, "online and offline product verification results differ")
			return 1
		}
	} else {
		onlineTarget, err := online.RefreshOnline(ctx, *platform, *arch, *version)
		if err != nil {
			fmt.Fprintf(stderr, "verify online Mihomo metadata: %v\n", err)
			return 1
		}
		offlineTarget, offlineBody, err := offline.RefreshOffline(ctx, *bundle, *platform, *arch, *version)
		if err != nil {
			fmt.Fprintf(stderr, "verify offline Mihomo target: %v\n", err)
			return 1
		}
		if !reflect.DeepEqual(onlineTarget, offlineTarget) {
			fmt.Fprintln(stderr, "online and offline Mihomo verification results differ")
			return 1
		}
		if err := offline.VerifyTarget(offlineTarget, offlineBody); err != nil {
			fmt.Fprintf(stderr, "verify offline Mihomo body: %v\n", err)
			return 1
		}
	}
	fmt.Fprintf(stdout, "online/offline TUF parity passed for %s %s/%s %s\n", *kind, *platform, *arch, *version)
	return 0
}
