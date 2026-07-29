package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"submux/internal/buildinfo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeapp"
	"submux/internal/runtimeinstance"
	"submux/internal/runtimeipc"
	"submux/internal/runtimepaths"
	"submux/internal/runtimestate"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 1 && (arguments[0] == "--version" || arguments[0] == "version") {
		info := buildinfo.Current()
		fmt.Fprintf(stdout, "submux-runtime %s (%s, %s)\n", info.Version, info.Commit, info.Date)
		return 0
	}
	if len(arguments) == 1 && arguments[0] == "--version-json" {
		_ = json.NewEncoder(stdout).Encode(buildinfo.Current())
		return 0
	}
	if len(arguments) == 0 || arguments[0] == "serve" {
		if len(arguments) > 0 {
			arguments = arguments[1:]
		}
		return runServe(arguments, stderr)
	}
	if arguments[0] == "status" {
		return runStatus(arguments[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: submux-runtime [serve|status|version|--version-json]")
	return 2
}

func runServe(arguments []string, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-dir", defaults.StateRoot, "Runtime state directory")
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "serve does not accept positional arguments")
		return 2
	}

	guard, err := runtimeinstance.Acquire(defaults.LockFile)
	if err != nil {
		if errors.Is(err, runtimeinstance.ErrAlreadyRunning) {
			writeCLIError(stderr, runtimeapi.ErrorAlreadyRunning, err.Error(), false)
			return 3
		}
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	defer guard.Close()

	state, err := runtimestate.Open(*stateRoot)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	defer state.Close()

	listener, err := runtimeipc.Listen(*endpoint)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	defer listener.Close()

	authorizer, err := runtimeipc.CurrentUserAuthorizer()
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorUnauthorized, err.Error(), false)
		return 1
	}
	service := &runtimeapp.Service{State: state, Version: buildinfo.Current().Version}
	server, err := runtimeipc.NewServer(service, authorizer)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInternal, err.Error(), false)
		return 1
	}

	contextWithSignal, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.New(stderr, "submux-runtime: ", log.LstdFlags).Printf("serving local IPC at %s", *endpoint)
	if err := server.Serve(contextWithSignal, listener); err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	return 0
}

func runStatus(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "status does not accept positional arguments")
		return 2
	}

	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		return writeStatusError(stdout, stderr, *jsonOutput, &runtimeipc.ClientError{
			Code:    runtimeapi.ErrorInvalidRequest,
			Message: err.Error(),
		})
	}
	defer client.CloseIdleConnections()

	snapshot, err := client.Observe(context.Background())
	if err != nil {
		var clientError *runtimeipc.ClientError
		if !errors.As(err, &clientError) {
			clientError = &runtimeipc.ClientError{
				Code:      runtimeapi.ErrorServiceUnavailable,
				Message:   err.Error(),
				Retryable: true,
			}
		}
		return writeStatusError(stdout, stderr, *jsonOutput, clientError)
	}

	if *jsonOutput {
		if err := json.NewEncoder(stdout).Encode(snapshot); err != nil {
			fmt.Fprintf(stderr, "encode Runtime status: %v\n", err)
			return 1
		}
		return 0
	}
	fmt.Fprintf(stdout, "Runtime %s: %s\n", snapshot.Runtime.Version, snapshot.Runtime.ServiceState)
	fmt.Fprintf(stdout, "Mihomo: %s\n", snapshot.Mihomo.State)
	fmt.Fprintf(stdout, "Revision: %d\n", snapshot.Revision)
	fmt.Fprintf(stdout, "Latest event cursor: %d\n", snapshot.LatestEventCursor)
	return 0
}

func writeStatusError(stdout io.Writer, stderr io.Writer, asJSON bool, clientError *runtimeipc.ClientError) int {
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(runtimeapi.ErrorEnvelope{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Error: runtimeapi.ProtocolError{
				Code:      clientError.Code,
				Message:   clientError.Message,
				Retryable: clientError.Retryable,
			},
		})
	} else {
		writeCLIError(stderr, clientError.Code, clientError.Message, clientError.Retryable)
	}
	if clientError.Code == runtimeapi.ErrorProtocolUnsupported {
		return 4
	}
	if clientError.Code == runtimeapi.ErrorUnauthorized {
		return 5
	}
	return 1
}

func writeCLIError(writer io.Writer, code string, message string, retryable bool) {
	fmt.Fprintf(writer, "%s: %s", code, message)
	if retryable {
		fmt.Fprint(writer, " (retryable)")
	}
	fmt.Fprintln(writer)
}
