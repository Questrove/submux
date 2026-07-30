//go:build linux

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
	"submux/internal/runtimeaccount"
	"submux/internal/runtimenet"
)

const (
	defaultEndpoint  = "/run/submux-runtime-privileged/runtime-net.sock"
	defaultStateRoot = "/var/lib/submux-runtime-privileged/network"
)

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
	flags := flag.NewFlagSet("submux-runtime-net", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaultEndpoint, "fixed internal Runtime network Socket")
	stateRoot := flags.String("state-dir", defaultStateRoot, "privileged Runtime network state directory")
	runtimeUser := flags.String("runtime-user", "submux-runtime", "fixed unprivileged Runtime service account")
	runtimeUID := flags.Int("runtime-uid", -1, "unprivileged Runtime service account UID")
	runtimeGID := flags.Int("runtime-gid", -1, "unprivileged Runtime service account GID")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "submux-runtime-net does not accept positional arguments")
		return 2
	}
	if *endpoint != defaultEndpoint || *stateRoot != defaultStateRoot {
		fmt.Fprintln(stderr, "submux-runtime-net requires the fixed Linux Runtime paths")
		return 2
	}
	if os.Geteuid() != 0 {
		fmt.Fprintln(stderr, "submux-runtime-net must run as root")
		return 1
	}
	resolvedUID, resolvedGID, err := resolveRuntimeIdentity(*runtimeUser, *runtimeUID, *runtimeGID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}

	system, err := runtimenet.NewLinuxSystem(resolvedUID)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manager, err := runtimenet.OpenManager(*stateRoot, resolvedUID, system)
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
	listener, err := runtimenet.Listen(*endpoint, int(resolvedGID))
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
	if joined := errors.Join(err, secondErr, cleanupErr); joined != nil {
		fmt.Fprintln(stderr, joined)
		return 1
	}
	return 0
}

func resolveRuntimeIdentity(name string, uid, gid int) (uint32, uint32, error) {
	if uid == -1 && gid == -1 {
		return runtimeaccount.Lookup(name)
	}
	if uid <= 0 || gid <= 0 {
		return 0, 0, errors.New("--runtime-uid and --runtime-gid must be supplied together and be positive")
	}
	if name != "submux-runtime" {
		return 0, 0, errors.New("Linux Runtime service account name must remain submux-runtime")
	}
	return uint32(uid), uint32(gid), nil
}
