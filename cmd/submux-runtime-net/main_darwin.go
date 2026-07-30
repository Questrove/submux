//go:build darwin

package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"submux/internal/buildinfo"
	"submux/internal/runtimenet"
	"submux/internal/runtimepaths"
)

const darwinPrivilegedStateRoot = "/Library/Application Support/SubmuxRuntimePrivileged/network"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 1 && (arguments[0] == "--version" || arguments[0] == "version") {
		info := buildinfo.Current()
		fmt.Fprintf(stdout, "submux-runtime-net %s (%s, %s)\n", info.Version, info.Commit, info.Date)
		return 0
	}
	if len(arguments) == 1 && arguments[0] == "--version-json" {
		_ = json.NewEncoder(stdout).Encode(buildinfo.Current())
		return 0
	}
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("submux-runtime-net", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.NetworkEndpoint, "fixed internal Runtime network Socket")
	stateRoot := flags.String("state-dir", darwinPrivilegedStateRoot, "privileged Runtime network state directory")
	runtimeRoot := flags.String("runtime-dir", defaults.StateRoot, "low-privilege Runtime state directory")
	controlEndpoint := flags.String(
		"control-endpoint",
		defaults.ControlEndpoint,
		"fixed Mihomo control Socket",
	)
	runtimeUID := flags.Int("runtime-uid", -1, "low-privilege Runtime service account UID")
	runtimeGID := flags.Int("runtime-gid", -1, "low-privilege Runtime service account GID")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "submux-runtime-net does not accept positional arguments")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "submux-runtime-net must run as root")
		return 1
	}
	if *runtimeUID <= 0 || *runtimeGID <= 0 {
		fmt.Fprintln(stderr, "submux-runtime-net requires --runtime-uid and --runtime-gid")
		return 2
	}
	if *endpoint != defaults.NetworkEndpoint ||
		*stateRoot != darwinPrivilegedStateRoot ||
		*runtimeRoot != defaults.StateRoot ||
		*controlEndpoint != defaults.ControlEndpoint {
		fmt.Fprintln(stderr, "submux-runtime-net requires the fixed macOS Runtime paths")
		return 2
	}

	system, err := runtimenet.NewDarwinSystem(
		*runtimeRoot,
		uint32(*runtimeUID),
		uint32(*runtimeGID),
		*controlEndpoint,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manager, err := runtimenet.OpenManager(*stateRoot, uint32(*runtimeUID), system)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if _, err := manager.Recover(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	listener, err := runtimenet.Listen(*endpoint, uint32(*runtimeGID))
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer listener.Close()
	server, err := runtimenet.NewServer(manager)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	results := make(chan error, 2)
	go func() {
		results <- manager.Run(ctx)
	}()
	go func() {
		results <- server.Serve(ctx, listener)
	}()
	err = <-results
	stop()
	secondErr := <-results
	cleanupContext, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
	_, cleanupErr := manager.FailOpen(cleanupContext, runtimenet.ReleaseServiceRestart)
	cleanupCancel()
	coreContext, coreCancel := context.WithTimeout(context.Background(), 15*time.Second)
	_, coreErr := system.StopCore(coreContext, runtimenet.PrivilegedCoreObjectMihomo)
	coreCancel()
	if joined := errors.Join(err, secondErr, cleanupErr, coreErr); joined != nil {
		fmt.Fprintln(stderr, joined)
		return 1
	}
	return 0
}
