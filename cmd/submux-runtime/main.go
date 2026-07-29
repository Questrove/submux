package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
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
	"submux/internal/runtimesource"
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
	if arguments[0] == "source" {
		return runSource(arguments[1:], os.Stdin, stdout, stderr)
	}
	if arguments[0] == "resource" {
		return runResource(arguments[1:], os.Stdin, stdout, stderr)
	}
	if arguments[0] == "override" {
		return runOverride(arguments[1:], os.Stdin, stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: submux-runtime [serve|tui|status|import|operation|proxy|source|resource|override|version|--version-json]")
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
	executor.Sources = &runtimesource.Manager{
		State: state,
		Fetcher: &runtimesource.Fetcher{
			MihomoAddress: net.JoinHostPort("127.0.0.1", strconv.Itoa(*proxyPort)),
		},
		Validator: executor,
	}
	supervisor := &runtimeapp.MihomoSupervisor{
		State:  state,
		Target: executor,
	}
	coordinator := &runtimeapp.Coordinator{
		State:         state,
		Executor:      executor,
		Recovery:      supervisor,
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
	fmt.Fprintf(
		stdout,
		"Mihomo desired: %s; actual: %s; recovery: %s\n",
		snapshot.Mihomo.DesiredState,
		snapshot.Mihomo.State,
		snapshot.Mihomo.Recovery,
	)
	if snapshot.Mihomo.CrashAttempts > 0 {
		fmt.Fprintf(stdout, "Mihomo crash attempts: %d\n", snapshot.Mihomo.CrashAttempts)
	}
	if snapshot.Mihomo.NextRestartAt != nil {
		fmt.Fprintf(stdout, "Mihomo next restart: %s\n", snapshot.Mihomo.NextRestartAt.Format(time.RFC3339))
	}
	if snapshot.Mihomo.Fault != nil {
		fmt.Fprintf(stdout, "Mihomo fault: %s: %s\n", snapshot.Mihomo.Fault.Code, snapshot.Mihomo.Fault.Message)
	}
	fmt.Fprintf(stdout, "Revision: %d\n", snapshot.Revision)
	fmt.Fprintf(stdout, "Latest event cursor: %d\n", snapshot.LatestEventCursor)
	writeSourceStatus(stdout, snapshot.Sources)
	writeResourceStatus(stdout, snapshot.Resources)
	if snapshot.AdvancedOverride.Present {
		fmt.Fprintf(stdout, "Advanced override: %s (%d bytes)\n", snapshot.AdvancedOverride.SHA256, snapshot.AdvancedOverride.Size)
	} else {
		fmt.Fprintln(stdout, "Advanced override: not configured")
	}
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
	sourceID := flags.String("source-id", "", "stored remote source ID")
	overrideContentID := flags.String("override-content-id", "", "uploaded prospective advanced override content ID")
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
		if (*contentID == "") == (*sourceID == "") || *wait {
			fmt.Fprintln(stderr, "proxy preview requires exactly one of --content-id or --source-id and does not accept --wait")
			return 2
		}
		preview, err := client.PreviewCandidateRequest(context.Background(), runtimeapi.PreviewCandidateRequest{
			ContentID:         *contentID,
			SourceID:          *sourceID,
			OverrideContentID: *overrideContentID,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(preview)
		} else {
			fmt.Fprintf(stdout, "validated %s via %s at %s\n", preview.CandidateSHA256, preview.ProxyKind, strings.Join(preview.ProxyAddresses, ", "))
			for _, origin := range preview.FieldOrigins {
				fmt.Fprintf(stdout, "%s\t%s\t%s\n", origin.Path, origin.Origin, origin.Status)
			}
			fmt.Fprintln(stdout, preview.CandidateYAML)
		}
		return 0
	}
	if command == "verify" {
		if *contentID != "" || *sourceID != "" || *overrideContentID != "" || *wait {
			fmt.Fprintln(stderr, "proxy verify does not accept content, source, override or wait options")
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
	if command != "preview" && *sourceID != "" {
		fmt.Fprintf(stderr, "proxy %s does not accept --source-id\n", command)
		return 2
	}
	if command != "preview" && *overrideContentID != "" {
		fmt.Fprintf(stderr, "proxy %s does not accept --override-content-id\n", command)
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

func runResource(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime resource [add|list]")
		return 2
	}
	switch arguments[0] {
	case "add":
		return runResourceAdd(arguments[1:], stdin, stdout, stderr)
	case "list":
		return runResourceList(arguments[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime resource [add|list]")
		return 2
	}
}

func runResourceAdd(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("resource add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the resource operation to finish")
	name := flags.String("name", "", "managed resource name")
	kind := flags.String("kind", "", "proxy-provider-yaml, rule-provider-yaml, certificate-pem, or private-key-pem")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 || *name == "" || !validResourceKind(*kind) {
		fmt.Fprintln(stderr, "resource add requires --name, a valid --kind, and one <file|-> argument")
		return 2
	}
	body, err := readContentArgument(flags.Arg(0), stdin, runtimestate.MaxManagedResourceBytes)
	if err != nil {
		fmt.Fprintf(stderr, "read managed resource: %v\n", err)
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	content, err := client.UploadImport(context.Background(), runtimeapi.ManagedResourceContentType, body)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddManagedResource,
			Params: runtimeapi.ActionParams{
				ContentID:    content.ID,
				ResourceKind: *kind,
				ResourceName: *name,
			},
		},
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

func runResourceList(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("resource list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "resource list does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(snapshot.Resources)
	} else {
		writeResourceStatus(stdout, snapshot.Resources)
	}
	return 0
}

func runOverride(arguments []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime override [get|set]")
		return 2
	}
	switch arguments[0] {
	case "get":
		return runOverrideGet(arguments[1:], stdout, stderr)
	case "set":
		return runOverrideSet(arguments[1:], stdin, stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime override [get|set]")
		return 2
	}
}

func runOverrideGet(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("override get", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "override get does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	document, err := client.GetAdvancedOverride(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(document)
	} else {
		fmt.Fprint(stdout, document.YAML)
		if !strings.HasSuffix(document.YAML, "\n") {
			fmt.Fprintln(stdout)
		}
	}
	return 0
}

func runOverrideSet(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("override set", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the override operation to finish")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime override set [options] <file|->")
		return 2
	}
	body, err := readContentArgument(flags.Arg(0), stdin, runtimestate.MaxAdvancedOverrideBytes)
	if err != nil {
		fmt.Fprintf(stderr, "read advanced override: %v\n", err)
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	content, err := client.UploadImport(context.Background(), "application/x-yaml", body)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionSetAdvancedOverride,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
			},
		},
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

func runSource(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime source [add|apply|delete|import|list|refresh|switch]")
		return 2
	}
	switch arguments[0] {
	case "add":
		return runSourceAdd(arguments[1:], stdin, stdout, stderr)
	case "apply":
		return runSourceApply(arguments[1:], stdout, stderr)
	case "delete":
		return runSourceDelete(arguments[1:], stdout, stderr)
	case "import":
		return runSourceImport(arguments[1:], stdin, stdout, stderr)
	case "list":
		return runSourceList(arguments[1:], stdout, stderr)
	case "refresh":
		return runSourceRefresh(arguments[1:], stdout, stderr)
	case "switch":
		return runSourceSwitch(arguments[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime source [add|apply|delete|import|list|refresh|switch]")
		return 2
	}
}

func runSourceAdd(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source add", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	sourceType := flags.String("type", runtimeapi.SourceTypeRemoteHTTP, "source type: remote_http or submux_output")
	name := flags.String("name", "", "source display name")
	sourceURL := flags.String("url", "", "HTTP(S) Mihomo configuration URL")
	route := flags.String("route", runtimeapi.SourceRouteDirect, "download route: direct or mihomo")
	userAgent := flags.String("user-agent", "", "optional HTTP User-Agent")
	username := flags.String("username", "", "optional HTTP Basic username")
	passwordStdin := flags.Bool("password-stdin", false, "read the HTTP Basic password from stdin")
	authorizeTarget := flags.String("authorize-target", "", "exact normalized scheme://host:port for high-risk settings")
	allowPrivate := flags.Bool("allow-private", false, "allow the authorized private target")
	allowHTTP := flags.Bool("allow-http", false, "allow non-loopback HTTP for the authorized target")
	customCAFile := flags.String("ca-file", "", "upload a PEM custom CA for the authorized HTTPS target")
	skipTLSVerify := flags.Bool("skip-tls-verify", false, "persistently skip TLS verification for the authorized target")
	refreshInterval := flags.Duration("refresh-interval", runtimesource.DefaultRefreshInterval, "automatic refresh interval; 0 disables it")
	timeout := flags.Duration("timeout", runtimesource.DefaultFetchTimeout, "per-refresh hard timeout")
	maxBytes := flags.Int64("max-bytes", runtimesource.DefaultResponseBytes, "maximum decoded response bytes")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 || *name == "" || *sourceURL == "" {
		fmt.Fprintln(stderr, "source add requires --name and --url and accepts no positional arguments")
		return 2
	}
	var password string
	if *passwordStdin {
		body, err := io.ReadAll(io.LimitReader(stdin, 4097))
		if err != nil {
			fmt.Fprintf(stderr, "read source password: %v\n", err)
			return 1
		}
		if len(body) > 4096 {
			fmt.Fprintln(stderr, "source password exceeds the allowed size")
			return 1
		}
		password = strings.TrimSuffix(strings.TrimSuffix(string(body), "\n"), "\r")
	}
	var customCAPEM string
	if *customCAFile != "" {
		body, err := readSmallRegularFile(*customCAFile, runtimesource.MaximumCustomCABytes)
		if err != nil {
			fmt.Fprintf(stderr, "read source custom CA: %v\n", err)
			return 1
		}
		customCAPEM = string(body)
	}
	intervalSeconds := int64(*refreshInterval / time.Second)
	draft := runtimeapi.RemoteSourceDraft{
		Type:                   *sourceType,
		Name:                   *name,
		URL:                    *sourceURL,
		Route:                  *route,
		UserAgent:              *userAgent,
		Username:               *username,
		Password:               password,
		AuthorizedTarget:       *authorizeTarget,
		AllowPrivate:           *allowPrivate,
		AllowHTTP:              *allowHTTP,
		CustomCAPEM:            customCAPEM,
		SkipTLSVerify:          *skipTLSVerify,
		RefreshIntervalSeconds: &intervalSeconds,
		TimeoutSeconds:         int(*timeout / time.Second),
		MaxResponseBytes:       *maxBytes,
	}
	if _, err := runtimesource.NormalizeDraft(draft); err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	body, err := json.Marshal(draft)
	if err != nil {
		fmt.Fprintf(stderr, "encode source draft: %v\n", err)
		return 1
	}
	return submitSourceDraft(
		*endpoint,
		body,
		*wait,
		*jsonOutput,
		stdout,
		stderr,
	)
}

func runSourceImport(
	arguments []string,
	stdin io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source import", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	name := flags.String("name", "", "source display name")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 || strings.TrimSpace(*name) == "" {
		fmt.Fprintln(stderr, "source import requires --name and one <file|-> argument")
		return 2
	}
	body, err := readContentArgument(flags.Arg(0), stdin, runtimestate.MaxImportBytes)
	if err != nil {
		fmt.Fprintf(stderr, "read imported source: %v\n", err)
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	content, err := client.UploadImport(context.Background(), "application/x-yaml", body)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddImportedSource,
			Params: runtimeapi.ActionParams{
				ContentID:  content.ID,
				SourceName: *name,
			},
		},
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

func runSourceList(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source list", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "source list does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(snapshot.Sources)
	} else {
		writeSourceStatus(stdout, snapshot.Sources)
	}
	return 0
}

func runSourceRefresh(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source refresh", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	route := flags.String("route", "", "one-time route override: direct or mihomo")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime source refresh [options] <source-id>")
		return 2
	}
	if *route != "" && *route != runtimeapi.SourceRouteDirect && *route != runtimeapi.SourceRouteMihomo {
		fmt.Fprintln(stderr, "source refresh --route must be direct or mihomo")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionRefreshSource,
			Params: runtimeapi.ActionParams{
				SourceID: flags.Arg(0),
				Route:    *route,
			},
		},
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

func runSourceApply(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source apply", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime source apply [options] <source-id>")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionApplySource,
			Params: runtimeapi.ActionParams{
				SourceID: flags.Arg(0),
			},
		},
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

func runSourceSwitch(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source switch", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	route := flags.String("route", "", "one-time refresh route: direct or mihomo")
	useCached := flags.Bool("use-cache", false, "use the last validated revision only if refresh fails")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime source switch [options] <source-id>")
		return 2
	}
	if *route != "" && *route != runtimeapi.SourceRouteDirect && *route != runtimeapi.SourceRouteMihomo {
		fmt.Fprintln(stderr, "source switch --route must be direct or mihomo")
		return 2
	}
	return submitSourceAction(
		*endpoint,
		runtimeapi.Action{
			Kind: runtimeapi.ActionSwitchSource,
			Params: runtimeapi.ActionParams{
				SourceID:  flags.Arg(0),
				Route:     *route,
				UseCached: *useCached,
			},
		},
		*wait,
		*jsonOutput,
		stdout,
		stderr,
	)
}

func runSourceDelete(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source delete", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", true, "wait for the source operation to finish")
	confirm := flags.Bool("confirm-current", false, "confirm deletion if Mihomo is stopped and this is the current source")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: submux-runtime source delete [options] <source-id>")
		return 2
	}
	return submitSourceAction(
		*endpoint,
		runtimeapi.Action{
			Kind: runtimeapi.ActionDeleteSource,
			Params: runtimeapi.ActionParams{
				SourceID: flags.Arg(0),
				Confirm:  *confirm,
			},
		},
		*wait,
		*jsonOutput,
		stdout,
		stderr,
	)
}

func submitSourceAction(
	endpoint string,
	action runtimeapi.Action,
	wait bool,
	jsonOutput bool,
	stdout io.Writer,
	stderr io.Writer,
) int {
	client, err := runtimeipc.NewClient(endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action:     action,
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, jsonOutput, err)
	}
	if wait {
		operation, err = client.WaitOperation(context.Background(), operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, jsonOutput, err)
		}
	}
	return writeOperation(stdout, jsonOutput, operation)
}

func submitSourceDraft(
	endpoint string,
	body []byte,
	wait bool,
	jsonOutput bool,
	stdout io.Writer,
	stderr io.Writer,
) int {
	client, err := runtimeipc.NewClient(endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	defer client.CloseIdleConnections()
	content, err := client.UploadImport(context.Background(), runtimeapi.SourceDraftContentType, body)
	if err != nil {
		return writeClientFailure(stdout, stderr, jsonOutput, err)
	}
	snapshot, err := client.Observe(context.Background())
	if err != nil {
		return writeClientFailure(stdout, stderr, jsonOutput, err)
	}
	operation, err := client.Execute(context.Background(), runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionAddRemoteSource,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
			},
		},
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, jsonOutput, err)
	}
	if wait {
		operation, err = client.WaitOperation(context.Background(), operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, jsonOutput, err)
		}
	}
	return writeOperation(stdout, jsonOutput, operation)
}

func writeSourceStatus(writer io.Writer, status runtimeapi.SourceStatus) {
	fmt.Fprintf(writer, "Sources: %d", status.Count)
	if status.CurrentSourceID != "" {
		fmt.Fprintf(writer, " (current %s)", status.CurrentSourceID)
	}
	fmt.Fprintln(writer)
	for _, source := range status.Items {
		fmt.Fprint(writer, "- ")
		if source.Current {
			fmt.Fprint(writer, "[current] ")
		}
		fmt.Fprintf(writer, "%s (%s) %s · %s", source.Name, source.Type, source.ID, source.RedactedTarget)
		if source.Route != "" {
			fmt.Fprintf(writer, " via %s", source.Route)
		}
		if source.LastRefreshResult != "" {
			fmt.Fprintf(writer, " · %s", source.LastRefreshResult)
		}
		if len(source.HighRiskSettings) > 0 {
			fmt.Fprintf(writer, " · high-risk: %s", strings.Join(source.HighRiskSettings, ","))
		}
		fmt.Fprintln(writer)
	}
}

func writeResourceStatus(writer io.Writer, status runtimeapi.ResourceStatus) {
	fmt.Fprintf(writer, "Managed resources: %d (%d bytes)\n", status.Count, status.TotalBytes)
	for _, resource := range status.Items {
		fmt.Fprintf(writer, "- %s %s %s %d bytes %s\n",
			resource.ID,
			resource.Kind,
			resource.Name,
			resource.Size,
			resource.SHA256,
		)
	}
}

func validResourceKind(kind string) bool {
	switch kind {
	case runtimeapi.ResourceKindProxyProvider,
		runtimeapi.ResourceKindRuleProvider,
		runtimeapi.ResourceKindCertificate,
		runtimeapi.ResourceKindPrivateKey:
		return true
	default:
		return false
	}
}

func readContentArgument(path string, stdin io.Reader, maximum int) ([]byte, error) {
	if path != "-" {
		return readSmallRegularFile(path, maximum)
	}
	body, err := io.ReadAll(io.LimitReader(stdin, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > maximum {
		return nil, errors.New("content size is outside the allowed range")
	}
	return body, nil
}

func readSmallRegularFile(path string, maximum int) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > int64(maximum) {
		return nil, errors.New("file must be a small regular file without symbolic links")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if err := validateOpenedSmallRegularFile(info, openedInfo, maximum); err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(file, int64(maximum)+1))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 || len(body) > maximum {
		return nil, errors.New("file size is outside the allowed range")
	}
	return body, nil
}

func validateOpenedSmallRegularFile(pathInfo, openedInfo os.FileInfo, maximum int) error {
	if !openedInfo.Mode().IsRegular() ||
		openedInfo.Size() > int64(maximum) ||
		!os.SameFile(pathInfo, openedInfo) {
		return errors.New("file changed while it was being opened")
	}
	return nil
}

func writeOperation(stdout io.Writer, asJSON bool, operation runtimeapi.Operation) int {
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(runtimeapi.OperationResponse{Operation: operation})
	} else {
		fmt.Fprintf(stdout, "%s %s %s\n", operation.ID, operation.State, operation.Stage)
		if operation.Result != nil {
			if operation.Result.SourceID != "" {
				switch {
				case operation.Result.Deleted:
					fmt.Fprintf(stdout, "source %s deleted\n", operation.Result.SourceID)
				case operation.Result.PreviousSourceID != "":
					fmt.Fprintf(stdout, "source %s -> %s", operation.Result.PreviousSourceID, operation.Result.SourceID)
					if operation.Result.UsedCachedSource {
						fmt.Fprint(stdout, " using cached validated revision")
					}
					fmt.Fprintln(stdout)
				default:
					fmt.Fprintf(stdout, "source %s: %s", operation.Result.SourceID, operation.Result.RefreshResult)
					if operation.Result.RefreshRoute != "" {
						fmt.Fprintf(stdout, " via %s", operation.Result.RefreshRoute)
					}
					fmt.Fprintln(stdout)
				}
			}
			if operation.Result.ResourceID != "" {
				fmt.Fprintf(stdout, "resource %s: %s\n",
					operation.Result.ResourceID,
					operation.Result.ResourceKind)
			}
			if operation.Result.AdvancedOverrideSHA256 != "" {
				fmt.Fprintf(stdout, "advanced override: %s\n",
					operation.Result.AdvancedOverrideSHA256)
			}
		}
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
