//go:build windows

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"

	"submux/internal/buildinfo"
	"submux/internal/runtimenet"
	"submux/internal/runtimepaths"
)

const windowsInternalRuntimeIdentity = 1

var windowsServiceContext = context.Background()

func main() {
	isService, err := svc.IsWindowsService()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if isService {
		if err := svc.Run("SubmuxRuntimeNet", windowsNetworkService{}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

type windowsNetworkService struct{}

func (windowsNetworkService) Execute(
	arguments []string,
	requests <-chan svc.ChangeRequest,
	changes chan<- svc.Status,
) (bool, uint32) {
	changes <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	windowsServiceContext = ctx
	result := make(chan int, 1)
	go func() {
		result <- run(arguments, io.Discard, io.Discard)
	}()
	changes <- svc.Status{
		State:   svc.Running,
		Accepts: svc.AcceptStop | svc.AcceptShutdown,
	}
	for {
		select {
		case code := <-result:
			if code != 0 {
				return false, uint32(code)
			}
			return false, 0
		case request := <-requests:
			switch request.Cmd {
			case svc.Interrogate:
				changes <- request.CurrentStatus
			case svc.Stop, svc.Shutdown:
				changes <- svc.Status{State: svc.StopPending}
				cancel()
				code := <-result
				if code != 0 {
					return false, uint32(code)
				}
				return false, 0
			}
		}
	}
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
	if len(arguments) != 0 {
		fmt.Fprintln(stderr, "submux-runtime-net for Windows does not accept paths, endpoints, or positional arguments")
		return 2
	}
	if err := requireLocalSystem(); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}

	paths := runtimepaths.Current()
	system, err := runtimenet.NewWindowsSystem()
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	manager, err := runtimenet.OpenManager(
		filepath.Join(paths.StateRoot, "network"),
		windowsInternalRuntimeIdentity,
		system,
	)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	signalContext, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	ctx, cancel := context.WithCancel(signalContext)
	defer cancel()
	if done := windowsServiceContext.Done(); done != nil {
		go func() {
			select {
			case <-done:
				cancel()
			case <-ctx.Done():
			}
		}()
	}
	if _, err := manager.Recover(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	listener, err := runtimenet.Listen(paths.NetworkEndpoint, windowsInternalRuntimeIdentity)
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

func requireLocalSystem() error {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return fmt.Errorf("open privileged Runtime network process token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return fmt.Errorf("read privileged Runtime network process SID: %w", err)
	}
	if user.User.Sid.String() != "S-1-5-18" {
		return errors.New("submux-runtime-net must run as LocalSystem")
	}
	return nil
}
