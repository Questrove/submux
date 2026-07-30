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
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"submux/internal/buildinfo"
	"submux/internal/runtimeaccount"
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
	runtimeUser := flags.String("runtime-user", "_submux-runtime", "fixed low-privilege Runtime service account")
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
	resolvedUID, resolvedGID, err := resolveDarwinRuntimeIdentity(*runtimeUser)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	if *endpoint != defaults.NetworkEndpoint ||
		*stateRoot != darwinPrivilegedStateRoot ||
		*runtimeRoot != defaults.StateRoot ||
		*controlEndpoint != defaults.ControlEndpoint {
		fmt.Fprintln(stderr, "submux-runtime-net requires the fixed macOS Runtime paths")
		return 2
	}
	operatorGID, err := lookupDarwinOperatorGID()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	if err := prepareDarwinManagementDirectory(resolvedUID, operatorGID); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	system, err := runtimenet.NewDarwinSystem(
		*runtimeRoot,
		resolvedUID,
		resolvedGID,
		*controlEndpoint,
	)
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
	listener, err := runtimenet.Listen(*endpoint, resolvedGID)
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

func lookupDarwinOperatorGID() (uint32, error) {
	group, err := user.LookupGroup("submux-runtime-operators")
	if err != nil {
		return 0, fmt.Errorf("resolve macOS Runtime operator group: %w", err)
	}
	value, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil || value == 0 {
		return 0, errors.New("macOS Runtime operator group GID is invalid")
	}
	return uint32(value), nil
}

func prepareDarwinManagementDirectory(runtimeUID, operatorGID uint32) error {
	runRoot, err := filepath.EvalSymlinks("/var/run")
	if err != nil {
		return fmt.Errorf("resolve macOS run directory: %w", err)
	}
	directory := filepath.Join(runRoot, "submux-runtime")
	if info, err := os.Lstat(directory); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("macOS Runtime management path is not a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect macOS Runtime management directory: %w", err)
	} else if err := os.Mkdir(directory, 0750); err != nil {
		return fmt.Errorf("create macOS Runtime management directory: %w", err)
	}
	if err := os.Chown(directory, int(runtimeUID), int(operatorGID)); err != nil {
		return fmt.Errorf("assign macOS Runtime management directory ownership: %w", err)
	}
	if err := os.Chmod(directory, 0750); err != nil {
		return fmt.Errorf("secure macOS Runtime management directory: %w", err)
	}
	return nil
}

func resolveDarwinRuntimeIdentity(name string) (uint32, uint32, error) {
	if name != "_submux-runtime" {
		return 0, 0, errors.New("macOS Runtime service account name must remain _submux-runtime")
	}
	return runtimeaccount.Lookup(name)
}
