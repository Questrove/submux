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
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"submux/internal/buildinfo"
	"submux/internal/mihomo"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeapp"
	"submux/internal/runtimecore"
	"submux/internal/runtimeinstance"
	"submux/internal/runtimeipc"
	"submux/internal/runtimepaths"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimestate"
	"submux/internal/runtimetui"
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
	if len(arguments) == 0 {
		if interactiveTerminal(os.Stdin, os.Stdout) {
			return runTUI(nil, os.Stdin, stdout, stderr)
		}
		return runServe(nil, stderr)
	}
	if arguments[0] == "serve" {
		if len(arguments) > 0 {
			arguments = arguments[1:]
		}
		return runServe(arguments, stderr)
	}
	if arguments[0] == "tui" {
		return runTUI(arguments[1:], os.Stdin, stdout, stderr)
	}
	if arguments[0] == "status" {
		return runStatus(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "import" {
		return runImport(arguments[1:], os.Stdin, stdout, stderr)
	}
	if arguments[0] == "operation" {
		return runOperation(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "proxy" {
		return runProxy(arguments[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: submux-runtime [serve|tui|status|import|operation|proxy|version|--version-json]")
	return 2
}

func runTUI(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("tui", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "tui does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	defer client.CloseIdleConnections()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := runtimetui.Run(ctx, client, stdin, stdout); err != nil && !errors.Is(err, context.Canceled) {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	return 0
}

func interactiveTerminal(stdin, stdout *os.File) bool {
	if stdin == nil || stdout == nil {
		return false
	}
	inputInfo, inputErr := stdin.Stat()
	outputInfo, outputErr := stdout.Stat()
	return inputErr == nil && outputErr == nil &&
		inputInfo.Mode()&os.ModeCharDevice != 0 &&
		outputInfo.Mode()&os.ModeCharDevice != 0
}

func runServe(arguments []string, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	flags.SetOutput(stderr)
	stateRoot := flags.String("state-dir", defaults.StateRoot, "Runtime state directory")
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	proxyPort := flags.Int("proxy-port", mihomo.DefaultExplicitProxyPort, "loopback explicit proxy port")
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
	process := &runtimeprocess.Process{
		ConfigPath: filepath.Join(*stateRoot, "config", "current", "config.yaml"),
		DataDir:    filepath.Join(*stateRoot, "mihomo-data"),
	}
	core := &runtimecore.Store{
		Root:       filepath.Join(*stateRoot, "core"),
		Verifier:   runtimecore.CommandVerifier{},
		Activation: process,
	}
	control := runtimeprocess.ControlProbe{Endpoint: defaults.ControlEndpoint}
	verifier := &mihomo.RuntimeCheck{
		Control:    control,
		ProxyProbe: mihomo.LocalHTTPProxyProbe{},
	}
	executor := &runtimeapp.MihomoExecutor{
		State:           state,
		Core:            core,
		Process:         process,
		ConfigRoot:      filepath.Join(*stateRoot, "config"),
		ControlEndpoint: defaults.ControlEndpoint,
		ProxyPort:       *proxyPort,
		Platform:        runtime.GOOS,
		Verifier:        verifier,
	}
	coordinator := &runtimeapp.Coordinator{
		State:         state,
		Executor:      executor,
		Version:       buildinfo.Current().Version,
		QueueCapacity: runtimeapp.DefaultQueueCapacity,
	}
	server, err := runtimeipc.NewServer(coordinator, authorizer)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInternal, err.Error(), false)
		return 1
	}

	contextWithSignal, stopSignal := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopSignal()
	serviceContext, stop := context.WithCancel(contextWithSignal)
	defer stop()
	log.New(stderr, "submux-runtime: ", log.LstdFlags).Printf("serving local IPC at %s", *endpoint)
	workerResult := make(chan error, 1)
	go func() {
		workerErr := coordinator.Run(serviceContext)
		workerResult <- workerErr
		if workerErr != nil {
			stop()
		}
	}()
	serverErr := server.Serve(serviceContext, listener)
	stop()
	workerErr := <-workerResult
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	processErr := process.Stop(shutdownContext)
	if err := errors.Join(serverErr, workerErr, processErr); err != nil {
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

func runImport(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	contentType := flags.String("content-type", "application/x-yaml", "imported content type")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime import [options] <file|->")
		return 2
	}
	var reader io.Reader = stdin
	var file *os.File
	var err error
	if flags.Arg(0) != "-" {
		file, err = os.Open(flags.Arg(0))
		if err != nil {
			fmt.Fprintf(stderr, "open import file: %v\n", err)
			return 1
		}
		defer file.Close()
		reader = file
	}
	body, err := io.ReadAll(io.LimitReader(reader, runtimestate.MaxImportBytes+1))
	if err != nil {
		fmt.Fprintf(stderr, "read import: %v\n", err)
		return 1
	}
	if len(body) == 0 || len(body) > runtimestate.MaxImportBytes {
		fmt.Fprintf(stderr, "import must contain 1 to %d bytes\n", runtimestate.MaxImportBytes)
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	content, err := client.UploadImport(context.Background(), *contentType, body)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(content)
	} else {
		fmt.Fprintln(stdout, content.ID)
	}
	return 0
}

func runOperation(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime operation [get|wait|cancel] <id>")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("operation "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "operation command requires one operation ID")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	id := flags.Arg(0)
	var operation runtimeapi.Operation
	switch command {
	case "get":
		operation, err = client.GetOperation(context.Background(), id)
	case "wait":
		operation, err = client.WaitOperation(context.Background(), id, 250*time.Millisecond)
	case "cancel":
		var snapshot runtimeapi.Snapshot
		snapshot, err = client.Observe(context.Background())
		if err == nil {
			operation, err = client.CancelOperation(context.Background(), id, runtimeapi.CancelOperationRequest{
				IfRevision: snapshot.Revision,
			})
		}
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime operation [get|wait|cancel] <id>")
		return 2
	}
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	return writeOperation(stdout, *jsonOutput, operation)
}

func runProxy(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime proxy [apply|preview|start|stop|verify]")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("proxy "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", false, "wait for the operation to finish")
	contentID := flags.String("content-id", "", "uploaded Mihomo configuration content ID")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "proxy command does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	if command == "preview" {
		if *contentID == "" || *wait {
			fmt.Fprintln(stderr, "proxy preview requires --content-id and does not accept --wait")
			return 2
		}
		preview, err := client.PreviewCandidate(context.Background(), *contentID)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(preview)
		} else {
			fmt.Fprintf(stdout, "validated %s via %s at %s\n", preview.CandidateSHA256, preview.ProxyKind, strings.Join(preview.ProxyAddresses, ", "))
			fmt.Fprintln(stdout, preview.CandidateYAML)
		}
		return 0
	}
	if command == "verify" {
		if *contentID != "" || *wait {
			fmt.Fprintln(stderr, "proxy verify does not accept --content-id or --wait")
			return 2
		}
		verification, err := client.VerifyProxy(context.Background())
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(verification)
		} else if verification.Available {
			fmt.Fprintf(stdout, "available via %s at %s\n", verification.Kind, strings.Join(verification.Addresses, ", "))
		} else {
			fmt.Fprintln(stdout, "unavailable")
		}
		if !verification.Available {
			return 1
		}
		return 0
	}
	if command != "apply" && command != "start" && command != "stop" {
		fmt.Fprintln(stderr, "usage: submux-runtime proxy [apply|preview|start|stop|verify]")
		return 2
	}
	if command == "apply" && *contentID == "" {
		fmt.Fprintln(stderr, "proxy apply requires --content-id")
		return 2
	}
	if command != "apply" && *contentID != "" {
		fmt.Fprintf(stderr, "proxy %s does not accept --content-id\n", command)
		return 2
	}
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	action := runtimeapi.Action{Kind: runtimeapi.ActionStartProxy}
	if command == "stop" {
		action.Kind = runtimeapi.ActionStopProxy
	} else if command == "apply" {
		action.Kind = runtimeapi.ActionApplyImportedConfig
		action.Params.ContentID = *contentID
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action:     action,
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *wait {
		operation, err = client.WaitOperation(context.Background(), operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
	}
	return writeOperation(stdout, *jsonOutput, operation)
}

func writeOperation(stdout io.Writer, asJSON bool, operation runtimeapi.Operation) int {
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(runtimeapi.OperationResponse{Operation: operation})
	} else {
		fmt.Fprintf(stdout, "%s %s %s\n", operation.ID, operation.State, operation.Stage)
	}
	switch operation.State {
	case runtimeapi.OperationFailed, runtimeapi.OperationCancelled, runtimeapi.OperationOutcomeUnknown:
		return 1
	default:
		return 0
	}
}

func writeClientFailure(stdout io.Writer, stderr io.Writer, asJSON bool, err error) int {
	var clientError *runtimeipc.ClientError
	if !errors.As(err, &clientError) {
		clientError = &runtimeipc.ClientError{
			Code:      runtimeapi.ErrorServiceUnavailable,
			Message:   err.Error(),
			Retryable: true,
		}
	}
	return writeStatusError(stdout, stderr, asJSON, clientError)
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
			CurrentRevision: clientError.CurrentRevision,
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
	if clientError.Code == runtimeapi.ErrorRevisionConflict || clientError.Code == runtimeapi.ErrorRequestConflict {
		return 6
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
