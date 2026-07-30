package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	"submux/internal/productupdate"
	"submux/internal/runtimeapi"
	"submux/internal/runtimeapp"
	"submux/internal/runtimebackup"
	"submux/internal/runtimebackupfile"
	"submux/internal/runtimecore"
	"submux/internal/runtimediag"
	"submux/internal/runtimeinstance"
	"submux/internal/runtimeipc"
	"submux/internal/runtimelog"
	"submux/internal/runtimenet"
	"submux/internal/runtimepaths"
	"submux/internal/runtimeprivacy"
	"submux/internal/runtimeprivileged"
	"submux/internal/runtimeprocess"
	"submux/internal/runtimesource"
	"submux/internal/runtimestate"
	"submux/internal/runtimetui"
	"submux/internal/runtimeupdate"
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
	if arguments[0] == "mihomo" {
		return runMihomo(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "product" {
		return runProduct(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "network" {
		return runNetwork(arguments[1:], stdout, stderr)
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
	if arguments[0] == "diagnostics" {
		return runDiagnostics(arguments[1:], stdout, stderr)
	}
	if arguments[0] == "backup" {
		return runBackup(arguments[1:], stdout, stderr)
	}
	fmt.Fprintln(stderr, "usage: submux-runtime [serve|tui|status|import|operation|proxy|mihomo|product|network|source|resource|override|diagnostics|backup|version|--version-json]")
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
	client, err := runtimeipc.NewTypedClient(*endpoint, "tui", buildinfo.Current().Version)
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
	networkEndpoint := flags.String("network-endpoint", defaults.NetworkEndpoint, "internal privileged Runtime network endpoint")
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
	configRoot := filepath.Join(*stateRoot, "config")
	if err := runtimebackup.RecoverConfigurationReplacement(configRoot); err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	installationID, err := state.InstallationID()
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	var network *runtimenet.Connector
	if (runtime.GOOS == "linux" || runtime.GOOS == "windows" || runtime.GOOS == "darwin") &&
		*networkEndpoint != "" {
		network = &runtimenet.Connector{
			Endpoint:          *networkEndpoint,
			RuntimeInstanceID: installationID,
		}
		defer network.Close()
	}
	logs, err := runtimelog.Open(filepath.Join(*stateRoot, "logs"))
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	if err := logs.GC(); err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	runtimeLogWriter, err := logs.Writer("runtime", "service")
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInternal, err.Error(), false)
		return 1
	}
	defer runtimeLogWriter.Flush()
	mihomoStdout, err := logs.Writer("mihomo", "stdout")
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInternal, err.Error(), false)
		return 1
	}
	mihomoStderr, err := logs.Writer("mihomo", "stderr")
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInternal, err.Error(), false)
		return 1
	}

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
		ConfigPath: filepath.Join(configRoot, "current", "config.yaml"),
		DataDir:    filepath.Join(*stateRoot, "mihomo-data"),
		Stdout:     mihomoStdout,
		Stderr:     mihomoStderr,
	}
	if runtime.GOOS == "darwin" && network != nil {
		delegate, delegateErr := runtimeprivileged.NewDelegate(network, *stateRoot)
		if delegateErr != nil {
			writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, delegateErr.Error(), true)
			return 1
		}
		process.Delegate = delegate
	}
	control := runtimeprocess.ControlProbe{Endpoint: defaults.ControlEndpoint}
	verifier := &mihomo.RuntimeCheck{
		Control:    control,
		ProxyProbe: mihomo.LocalHTTPProxyProbe{},
	}
	resourceRoot, err := state.ManagedResourceRoot()
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), true)
		return 1
	}
	candidateVerifier := runtimeapp.CoreCandidateVerifier{
		ConfigPath: process.ConfigPath,
		DataDir:    process.DataDir,
		SafePaths:  []string{resourceRoot},
	}
	core := &runtimecore.Store{
		Root:     filepath.Join(*stateRoot, "core"),
		Verifier: candidateVerifier,
		Activation: &runtimeapp.CoreActivation{
			Process:    process,
			Verifier:   verifier,
			ConfigPath: process.ConfigPath,
		},
	}
	updateTrust := &runtimeupdate.Verifier{
		InitialRoot: runtimeupdate.InitialRoot(),
		StateRoot:   filepath.Join(*stateRoot, "trust", "tuf"),
	}
	updateManager := &runtimeupdate.Manager{
		Root:              filepath.Join(*stateRoot, "updates"),
		Platform:          runtime.GOOS,
		Arch:              runtime.GOARCH,
		Trust:             updateTrust,
		Official:          runtimecore.NewOfficialReleaseSource(nil),
		Core:              core,
		CandidateVerifier: candidateVerifier,
	}
	productUpdateManager := &productupdate.Manager{
		Root:           filepath.Join(*stateRoot, "product-updates"),
		Platform:       runtime.GOOS,
		Arch:           runtime.GOARCH,
		CurrentVersion: buildinfo.Current().Version,
		InstallationID: installationID,
		Trust:          updateTrust,
		State:          state,
	}
	backupManager := &runtimebackup.Service{
		State:      state,
		Root:       filepath.Join(*stateRoot, "backups"),
		ConfigRoot: configRoot,
	}
	executor := &runtimeapp.MihomoExecutor{
		State:           state,
		Core:            core,
		Process:         process,
		ConfigRoot:      configRoot,
		ControlEndpoint: defaults.ControlEndpoint,
		ProxyPort:       *proxyPort,
		Platform:        runtime.GOOS,
		Verifier:        verifier,
		Network:         network,
		TUN:             network,
		Updates:         updateManager,
		ProductUpdates:  productUpdateManager,
		ProductNetwork:  network,
		Backups:         backupManager,
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
		State:          state,
		Executor:       executor,
		Recovery:       supervisor,
		Network:        network,
		Updates:        updateManager,
		ProductUpdates: productUpdateManager,
		Backups:        backupManager,
		Diagnostics: &runtimediag.Service{
			State:          state,
			StateRoot:      *stateRoot,
			RuntimeVersion: buildinfo.Current().Version,
		},
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
	if err := productUpdateManager.RecoverIncomplete(
		serviceContext,
		executor,
		func(string, int, bool) error { return nil },
	); err != nil {
		writeCLIError(stderr, runtimeapi.ErrorServiceUnavailable, err.Error(), false)
		return 1
	}
	runtimeLogger := log.New(runtimeLogWriter, "submux-runtime: ", log.LstdFlags)
	runtimeLogger.Print("serving local IPC")
	workerResult := make(chan error, 1)
	go func() {
		workerErr := coordinator.Run(serviceContext)
		workerResult <- workerErr
		if workerErr != nil {
			stop()
		}
	}()
	logGCResult := make(chan error, 1)
	go func() {
		logGCErr := logs.RunGC(serviceContext, runtimelog.DefaultGCInterval)
		logGCResult <- logGCErr
		if logGCErr != nil {
			stop()
		}
	}()
	serverErr := server.Serve(serviceContext, listener)
	stop()
	workerErr := <-workerResult
	logGCErr := <-logGCResult
	shutdownContext, cancelShutdown := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelShutdown()
	var networkErr error
	if network != nil {
		networkErr = network.FailOpen(shutdownContext)
	}
	var processErr error
	if networkErr == nil {
		processErr = process.Stop(shutdownContext)
	}
	if err := errors.Join(serverErr, workerErr, logGCErr, processErr, networkErr); err != nil {
		runtimeLogger.Printf("service stopped with error: %s", runtimeprivacy.RedactError(err))
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
			fmt.Fprintf(stderr, "encode Runtime status: %s\n", runtimeprivacy.RedactError(err))
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
	writeNetworkStatus(stdout, snapshot.Network)
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

func runNetwork(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime network [status|preview|enable|disable]")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("network "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	wait := flags.Bool("wait", false, "wait for the operation to finish")
	mode := flags.String("mode", runtimeapi.RunModeTUN, "network mode: tun or gateway")
	ipv6Policy := flags.String("ipv6", "", "IPv6 policy: proxy, direct, or block")
	dnsPolicy := flags.String("dns", "", "DNS policy: hijack or off")
	captureTCP := flags.Bool("tcp", true, "capture gateway TCP")
	captureUDP := flags.Bool("udp", true, "capture gateway UDP")
	proxyHost := flags.Bool("proxy-host", false, "capture gateway host traffic")
	planID := flags.String("plan-id", "", "validated Runtime network plan ID")
	var captureRoutes stringListFlag
	flags.Var(&captureRoutes, "capture-route", "specific route ID to include in TUN; repeat as needed")
	var excludedRoutes stringListFlag
	flags.Var(&excludedRoutes, "exclude-route", "gateway LAN route ID to exclude; repeat as needed")
	var dnsDirect stringListFlag
	flags.Var(&dnsDirect, "dns-direct", "gateway DNS server IPv4 CIDR to keep direct; repeat as needed")
	var udpExceptions gatewayExceptionListFlag
	flags.Var(&udpExceptions, "udp-exception", "typed gateway UDP exception as JSON; repeat as needed")
	var hostExceptions gatewayExceptionListFlag
	flags.Var(&hostExceptions, "host-exception", "typed gateway host exception as JSON; repeat as needed")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "network command does not accept positional arguments")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	defer client.CloseIdleConnections()
	ctx := context.Background()
	switch command {
	case "status":
		if *wait || *planID != "" || len(captureRoutes) > 0 ||
			len(excludedRoutes) > 0 || len(dnsDirect) > 0 ||
			len(udpExceptions) > 0 || len(hostExceptions) > 0 ||
			*mode != runtimeapi.RunModeTUN || *ipv6Policy != "" ||
			*dnsPolicy != "" || !*captureTCP || !*captureUDP || *proxyHost {
			fmt.Fprintln(stderr, "network status accepts only --endpoint and --json")
			return 2
		}
		snapshot, err := client.Observe(ctx)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(snapshot.Network)
		} else {
			writeNetworkStatus(stdout, snapshot.Network)
		}
		return 0
	case "preview":
		if *wait || *planID != "" {
			fmt.Fprintln(stderr, "network preview does not accept --wait or --plan-id")
			return 2
		}
		request := runtimeapi.NetworkPreviewRequest{
			Mode:            *mode,
			IPv6Policy:      *ipv6Policy,
			DNSPolicy:       *dnsPolicy,
			CaptureRouteIDs: append([]string(nil), captureRoutes...),
		}
		if *mode == runtimeapi.RunModeGateway {
			request.CaptureTCP = captureTCP
			request.CaptureUDP = captureUDP
			request.ProxyHostTraffic = *proxyHost
			request.ExcludedRouteIDs = append([]string(nil), excludedRoutes...)
			request.UDPExceptions = append([]runtimeapi.GatewayTrafficException(nil), udpExceptions...)
			request.DNSDirectCIDRs = append([]string(nil), dnsDirect...)
			request.HostExceptions = append([]runtimeapi.GatewayTrafficException(nil), hostExceptions...)
			if len(captureRoutes) > 0 {
				fmt.Fprintln(stderr, "gateway preview does not accept --capture-route")
				return 2
			}
		} else if *mode == runtimeapi.RunModeTUN {
			if len(excludedRoutes) > 0 || len(dnsDirect) > 0 ||
				len(udpExceptions) > 0 || len(hostExceptions) > 0 ||
				!*captureTCP || !*captureUDP || *proxyHost {
				fmt.Fprintln(stderr, "ordinary TUN preview does not accept gateway settings")
				return 2
			}
		} else {
			fmt.Fprintln(stderr, "network preview --mode must be tun or gateway")
			return 2
		}
		preview, err := client.PreviewNetwork(ctx, request)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(preview)
			return 0
		}
		fmt.Fprintf(stdout, "Plan: %s (expires %s)\n", preview.PlanID, preview.ExpiresAt.Format(time.RFC3339))
		if preview.PreviewOnly {
			fmt.Fprintln(stdout, "Feature status: preview; real-system acceptance is still required.")
		}
		if preview.GatewaySettings != nil {
			fmt.Fprintf(
				stdout,
				"Device: %s; mode: gateway; IPv6: %s; DNS: %s; TCP: %t; UDP: %t; host: %t\n",
				preview.Device,
				preview.GatewaySettings.IPv6Policy,
				preview.GatewaySettings.DNSPolicy,
				preview.GatewaySettings.CaptureTCP,
				preview.GatewaySettings.CaptureUDP,
				preview.GatewaySettings.ProxyHostTraffic,
			)
		} else {
			fmt.Fprintf(stdout, "Device: %s; mode: tun; IPv6: %s; DNS: %s\n", preview.Device, preview.Settings.IPv6Policy, preview.Settings.DNSPolicy)
		}
		for _, route := range preview.Routes {
			disposition := "bypass"
			if !route.Bypass {
				disposition = "capture"
			}
			fmt.Fprintf(stdout, "%s\t%s\t%s\t%s\t%s\t%s\n", route.ID, route.Family, route.CIDR, route.Interface, route.Role, disposition)
		}
		for _, conflict := range preview.Conflicts {
			fmt.Fprintf(stdout, "conflict: %s: %s (%s)\n", conflict.Kind, conflict.Detail, conflict.Owner)
		}
		for _, warning := range preview.Warnings {
			fmt.Fprintf(stdout, "warning: %s\n", warning)
		}
		return 0
	case "enable", "disable":
		if command == "enable" && *planID == "" {
			fmt.Fprintln(stderr, "network enable requires --plan-id from network preview")
			return 2
		}
		if command == "disable" && *planID != "" {
			fmt.Fprintln(stderr, "network disable does not accept --plan-id")
			return 2
		}
		if len(captureRoutes) > 0 || len(excludedRoutes) > 0 ||
			len(dnsDirect) > 0 || len(udpExceptions) > 0 || len(hostExceptions) > 0 ||
			*ipv6Policy != "" || *dnsPolicy != "" ||
			!*captureTCP || !*captureUDP || *proxyHost {
			fmt.Fprintf(stderr, "network %s accepts settings only through a validated preview\n", command)
			return 2
		}
		snapshot, err := client.Observe(ctx)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *mode != runtimeapi.RunModeTUN && *mode != runtimeapi.RunModeGateway {
			fmt.Fprintln(stderr, "network enable/disable --mode must be tun or gateway")
			return 2
		}
		action := runtimeapi.Action{Kind: runtimeapi.ActionDisableTUN}
		if *mode == runtimeapi.RunModeGateway {
			action.Kind = runtimeapi.ActionDisableGateway
		}
		if command == "enable" {
			action.Kind = runtimeapi.ActionEnableTUN
			if *mode == runtimeapi.RunModeGateway {
				action.Kind = runtimeapi.ActionEnableGateway
			}
			action.Params.PlanID = *planID
		}
		operation, err := client.Execute(ctx, runtimeapi.CreateOperationRequest{
			IfRevision: snapshot.Revision,
			Action:     action,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *wait {
			operation, err = client.WaitOperation(ctx, operation.ID, 250*time.Millisecond)
			if err != nil {
				return writeClientFailure(stdout, stderr, *jsonOutput, err)
			}
		}
		return writeOperation(stdout, *jsonOutput, operation)
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime network [status|preview|enable|disable]")
		return 2
	}
}

func writeNetworkStatus(writer io.Writer, status runtimeapi.NetworkStatus) {
	fmt.Fprintf(
		writer,
		"Network available: %t; mode: %s; state: %s",
		status.Available,
		status.Mode,
		status.State,
	)
	if status.Device != "" {
		fmt.Fprintf(writer, "; device: %s", status.Device)
	}
	fmt.Fprintln(writer)
	if status.PreviewOnly {
		fmt.Fprintln(writer, "Feature status: preview; real-system acceptance is still required.")
	}
	if status.LeaseExpiresAt != nil {
		fmt.Fprintf(writer, "Network lease expires: %s\n", status.LeaseExpiresAt.Format(time.RFC3339))
	}
	if status.GatewaySettings != nil {
		fmt.Fprintf(
			writer,
			"Gateway IPv6: %s; DNS: %s; TCP: %t; UDP: %t; host: %t\n",
			status.GatewaySettings.IPv6Policy,
			status.GatewaySettings.DNSPolicy,
			status.GatewaySettings.CaptureTCP,
			status.GatewaySettings.CaptureUDP,
			status.GatewaySettings.ProxyHostTraffic,
		)
	}
	for _, conflict := range status.Conflicts {
		fmt.Fprintf(writer, "Network conflict: %s: %s (%s)\n", conflict.Kind, conflict.Detail, conflict.Owner)
	}
	for _, residual := range status.Residuals {
		fmt.Fprintf(writer, "Network residual: %s %s %s\n", residual.Kind, residual.Name, residual.State)
	}
	if status.Fault != nil {
		fmt.Fprintf(writer, "Network fault: %s: %s\n", status.Fault.Code, status.Fault.Message)
	}
}

type stringListFlag []string

func (values *stringListFlag) String() string {
	return strings.Join(*values, ",")
}

func (values *stringListFlag) Set(value string) error {
	if value == "" {
		return errors.New("value must not be empty")
	}
	*values = append(*values, value)
	return nil
}

type gatewayExceptionListFlag []runtimeapi.GatewayTrafficException

func (values *gatewayExceptionListFlag) String() string {
	body, _ := json.Marshal(values)
	return string(body)
}

func (values *gatewayExceptionListFlag) Set(value string) error {
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var exception runtimeapi.GatewayTrafficException
	if err := decoder.Decode(&exception); err != nil {
		return fmt.Errorf("decode typed gateway exception: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("typed gateway exception must contain one JSON object")
	}
	*values = append(*values, exception)
	return nil
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
			fmt.Fprintf(stderr, "open import file: %s\n", runtimeprivacy.RedactError(err))
			return 1
		}
		defer file.Close()
		reader = file
	}
	body, err := io.ReadAll(io.LimitReader(reader, runtimestate.MaxImportBytes+1))
	if err != nil {
		fmt.Fprintf(stderr, "read import: %s\n", runtimeprivacy.RedactError(err))
		return 1
	}
	if len(body) == 0 || len(body) > runtimestate.MaxImportBytes {
		fmt.Fprintf(stderr, "import must contain 1 to %d bytes\n", runtimestate.MaxImportBytes)
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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

func runMihomo(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime mihomo [check|import|install|rollback]")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("mihomo "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	version := flags.String("version", "", "exact stable Mihomo vX.Y.Z version")
	source := flags.String("source", runtimeapi.MihomoUpdateSourceOnlineTUF, "online_tuf or upstream_only")
	planID := flags.String("plan", "", "verified Mihomo update plan ID")
	trust := flags.String("trust", runtimeapi.MihomoUpdateTrustTUF, "tuf or upstream_only")
	confirm := flags.Bool("confirm", false, "explicitly confirm core replacement or rollback")
	wait := flags.Bool("wait", false, "wait for the operation to finish")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if command != "check" && command != "import" && command != "install" && command != "rollback" {
		fmt.Fprintln(stderr, "usage: submux-runtime mihomo [check|import|install|rollback]")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
		return 1
	}
	defer client.CloseIdleConnections()
	ctx := context.Background()

	if command == "check" {
		if flags.NArg() != 0 || *planID != "" || *confirm || *wait ||
			(*source != runtimeapi.MihomoUpdateSourceOnlineTUF &&
				*source != runtimeapi.MihomoUpdateSourceUpstreamOnly) {
			fmt.Fprintln(stderr, "mihomo check accepts --source online_tuf|upstream_only and optional --version")
			return 2
		}
		preview, err := client.PreviewMihomoUpdate(ctx, runtimeapi.MihomoUpdatePreviewRequest{
			Source:  *source,
			Version: *version,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		return writeMihomoUpdatePlan(stdout, *jsonOutput, preview)
	}

	if command == "import" {
		if flags.NArg() != 1 || *planID != "" || *confirm || *wait ||
			*source != runtimeapi.MihomoUpdateSourceOnlineTUF {
			fmt.Fprintln(stderr, "mihomo import accepts optional --version and exactly one offline bundle file")
			return 2
		}
		fileName, openFile, size, digest, err := inspectUpdateBundle(flags.Arg(0))
		if err != nil {
			writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
			return 1
		}
		defer openFile.Close()
		bundle, err := client.UploadMihomoUpdateBundle(ctx, openFile, size, digest)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		preview, err := client.PreviewMihomoUpdate(ctx, runtimeapi.MihomoUpdatePreviewRequest{
			Source:   runtimeapi.MihomoUpdateSourceOfflineTUF,
			Version:  *version,
			BundleID: bundle.ID,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if !*jsonOutput {
			fmt.Fprintf(stdout, "verified offline bundle %s\n", filepath.Base(fileName))
		}
		return writeMihomoUpdatePlan(stdout, *jsonOutput, preview)
	}

	if flags.NArg() != 0 || *version != "" || *source != runtimeapi.MihomoUpdateSourceOnlineTUF {
		fmt.Fprintf(stderr, "mihomo %s does not accept version, source, or positional arguments\n", command)
		return 2
	}
	snapshot, err := client.Observe(ctx)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	action := runtimeapi.Action{Kind: runtimeapi.ActionRollbackMihomo}
	action.Params.Confirm = *confirm
	if command == "install" {
		if *planID == "" || !*confirm ||
			(*trust != runtimeapi.MihomoUpdateTrustTUF &&
				*trust != runtimeapi.MihomoUpdateTrustUpstreamOnly) {
			fmt.Fprintln(stderr, "mihomo install requires --plan, --trust tuf|upstream_only, and --confirm")
			return 2
		}
		action.Kind = runtimeapi.ActionUpdateMihomo
		action.Params.PlanID = *planID
		action.Params.Trust = *trust
	} else if *planID != "" || !*confirm || *trust != runtimeapi.MihomoUpdateTrustTUF {
		fmt.Fprintln(stderr, "mihomo rollback requires --confirm and does not accept --plan or --trust")
		return 2
	}
	operation, err := client.Execute(ctx, runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action:     action,
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *wait {
		operation, err = client.WaitOperation(ctx, operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
	}
	return writeOperation(stdout, *jsonOutput, operation)
}

func inspectUpdateBundle(name string) (string, *os.File, int64, string, error) {
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", nil, 0, "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", nil, 0, "", err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 ||
		info.Size() <= 0 || info.Size() > runtimeupdate.MaxBundleBytes {
		return "", nil, 0, "", errors.New("offline Mihomo update bundle must be a bounded regular non-linked file")
	}
	file, err := os.Open(absolute)
	if err != nil {
		return "", nil, 0, "", err
	}
	hash := sha256.New()
	written, err := io.Copy(hash, file)
	if err != nil || written != info.Size() {
		file.Close()
		return "", nil, 0, "", errors.New("offline Mihomo update bundle could not be hashed completely")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		file.Close()
		return "", nil, 0, "", err
	}
	return absolute, file, info.Size(), hex.EncodeToString(hash.Sum(nil)), nil
}

func writeMihomoUpdatePlan(stdout io.Writer, jsonOutput bool, plan runtimeapi.MihomoUpdatePlan) int {
	if jsonOutput {
		_ = json.NewEncoder(stdout).Encode(plan)
		return 0
	}
	fmt.Fprintf(
		stdout,
		"%s %s (%s, %s/%s)\nasset %s bytes sha256:%s\nplan %s expires %s\n",
		plan.Version,
		plan.Repository,
		plan.Trust,
		plan.Platform,
		plan.Arch,
		strconv.FormatInt(plan.AssetSize, 10),
		plan.AssetSHA256,
		plan.PlanID,
		plan.ExpiresAt.Format(time.RFC3339),
	)
	if plan.Warning != "" {
		fmt.Fprintln(stdout, plan.Warning)
	}
	return 0
}

func runProduct(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime product [check|import|install|rollback]")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("product "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	version := flags.String("version", "", "exact stable Runtime vX.Y.Z version")
	planID := flags.String("plan", "", "verified Runtime product update plan ID")
	trust := flags.String("trust", runtimeapi.ProductUpdateTrustTUF, "must be tuf")
	confirm := flags.Bool("confirm", false, "explicitly confirm product replacement or rollback")
	wait := flags.Bool("wait", false, "wait for the operation to finish")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if command != "check" && command != "import" && command != "install" && command != "rollback" {
		fmt.Fprintln(stderr, "usage: submux-runtime product [check|import|install|rollback]")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
		return 1
	}
	defer client.CloseIdleConnections()
	ctx := context.Background()

	if command == "check" {
		if flags.NArg() != 0 || *planID != "" || *confirm || *wait ||
			*trust != runtimeapi.ProductUpdateTrustTUF {
			fmt.Fprintln(stderr, "product check accepts only optional --version")
			return 2
		}
		preview, err := client.PreviewProductUpdate(ctx, runtimeapi.ProductUpdatePreviewRequest{
			Source:  runtimeapi.ProductUpdateSourceOnlineTUF,
			Version: *version,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		return writeProductUpdatePlan(stdout, *jsonOutput, preview)
	}

	if command == "import" {
		if flags.NArg() != 1 || *planID != "" || *confirm || *wait ||
			*trust != runtimeapi.ProductUpdateTrustTUF {
			fmt.Fprintln(stderr, "product import accepts optional --version and exactly one offline TUF bundle file")
			return 2
		}
		body, err := runtimebackupfile.Read(flags.Arg(0), runtimeapi.RuntimeProductUpdateMaxBytes)
		if err != nil {
			writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
			return 1
		}
		content, err := client.UploadImport(ctx, runtimeapi.ProductUpdateBundleContentType, body)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		preview, err := client.PreviewProductUpdate(ctx, runtimeapi.ProductUpdatePreviewRequest{
			Source:    runtimeapi.ProductUpdateSourceOfflineTUF,
			Version:   *version,
			ContentID: content.ID,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		return writeProductUpdatePlan(stdout, *jsonOutput, preview)
	}

	if flags.NArg() != 0 || *version != "" {
		fmt.Fprintf(stderr, "product %s does not accept version or positional arguments\n", command)
		return 2
	}
	snapshot, err := client.Observe(ctx)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	action := runtimeapi.Action{
		Kind: runtimeapi.ActionRollbackProduct,
		Params: runtimeapi.ActionParams{
			Confirm: *confirm,
		},
	}
	if command == "install" {
		if *planID == "" || !*confirm || *trust != runtimeapi.ProductUpdateTrustTUF {
			fmt.Fprintln(stderr, "product install requires --plan, --trust tuf, and --confirm")
			return 2
		}
		action.Kind = runtimeapi.ActionUpdateProduct
		action.Params.PlanID = *planID
		action.Params.Trust = *trust
	} else if *planID != "" || !*confirm || *trust != runtimeapi.ProductUpdateTrustTUF {
		fmt.Fprintln(stderr, "product rollback requires --confirm and does not accept --plan or --trust")
		return 2
	}
	operation, err := client.Execute(ctx, runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action:     action,
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *wait {
		operation, err = client.WaitOperation(ctx, operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
	}
	return writeOperation(stdout, *jsonOutput, operation)
}

func writeProductUpdatePlan(stdout io.Writer, jsonOutput bool, plan runtimeapi.ProductUpdatePlan) int {
	if jsonOutput {
		_ = json.NewEncoder(stdout).Encode(plan)
		return 0
	}
	fmt.Fprintf(
		stdout,
		"Runtime %s -> %s (%s, %s/%s)\nasset %s bytes sha256:%s\nschema %d -> %d; IPC protocol %d in %d-%d\nnetwork interruption: %s\nplan %s expires %s\n",
		plan.CurrentVersion,
		plan.Version,
		plan.Trust,
		plan.Platform,
		plan.Arch,
		strconv.FormatInt(plan.AssetSize, 10),
		plan.AssetSHA256,
		plan.Migration.CurrentSchema,
		plan.Migration.TargetSchema,
		plan.Migration.CurrentProtocol,
		plan.Migration.ProtocolMin,
		plan.Migration.ProtocolMax,
		plan.NetworkInterruption,
		plan.PlanID,
		plan.ExpiresAt.Format(time.RFC3339),
	)
	if plan.ReleaseNotes != "" {
		fmt.Fprintln(stdout, plan.ReleaseNotes)
	}
	if plan.Migration.Summary != "" {
		fmt.Fprintln(stdout, plan.Migration.Summary)
	}
	if plan.Warning != "" {
		fmt.Fprintln(stdout, plan.Warning)
	}
	return 0
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
		fmt.Fprintf(stderr, "read managed resource: %s\n", runtimeprivacy.RedactError(err))
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
	reveal := flags.Bool("reveal", false, "confirm displaying the advanced override body")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "override get does not accept positional arguments")
		return 2
	}
	if !*reveal {
		fmt.Fprintln(stderr, runtimeapi.SensitiveDataWarning)
		fmt.Fprintln(stderr, "override get requires --reveal")
		return 2
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
		return 1
	}
	defer client.CloseIdleConnections()
	document, err := client.GetAdvancedOverride(context.Background(), true)
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
		fmt.Fprintf(stderr, "read advanced override: %s\n", runtimeprivacy.RedactError(err))
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, "usage: submux-runtime source [add|apply|delete|import|list|refresh|reveal-url|switch]")
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
	case "reveal-url":
		return runSourceRevealURL(arguments[1:], stdout, stderr)
	case "switch":
		return runSourceSwitch(arguments[1:], stdout, stderr)
	default:
		fmt.Fprintln(stderr, "usage: submux-runtime source [add|apply|delete|import|list|refresh|reveal-url|switch]")
		return 2
	}
}

func runSourceRevealURL(arguments []string, stdout io.Writer, stderr io.Writer) int {
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("source reveal-url", flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	reveal := flags.Bool("reveal", false, "explicitly confirm revealing the full source URL")
	if err := flags.Parse(arguments); err != nil {
		return 2
	}
	if flags.NArg() != 1 || !*reveal {
		fmt.Fprintln(stderr, "source reveal-url requires --reveal and one <source-id>")
		return 2
	}
	fmt.Fprintln(stderr, runtimeapi.SensitiveDataWarning)
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	defer client.CloseIdleConnections()
	result, err := client.RevealSourceURL(context.Background(), flags.Arg(0), true)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(result)
	} else {
		fmt.Fprintln(stdout, result.URL)
	}
	return 0
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
			fmt.Fprintf(stderr, "read source password: %s\n", runtimeprivacy.RedactError(err))
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
			fmt.Fprintf(stderr, "read source custom CA: %s\n", runtimeprivacy.RedactError(err))
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
		fmt.Fprintf(stderr, "encode source draft: %s\n", runtimeprivacy.RedactError(err))
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
		fmt.Fprintf(stderr, "read imported source: %s\n", runtimeprivacy.RedactError(err))
		return 1
	}
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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

func runDiagnostics(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 || (arguments[0] != "preview" && arguments[0] != "create") {
		fmt.Fprintln(stderr, "usage: submux-runtime diagnostics [preview|create] [options]")
		return 2
	}
	command := arguments[0]
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("diagnostics "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	includeRawConfig := flags.Bool("include-raw-config", false, "include the current raw Mihomo configuration")
	includeFullLogs := flags.Bool("include-full-logs", false, "include retained Runtime and Mihomo logs")
	includeNetworkInfo := flags.Bool("include-network-info", false, "include local Runtime network state")
	confirmSensitive := flags.Bool("confirm-sensitive", false, "confirm inclusion of every selected sensitive item")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}
	if flags.NArg() != 0 {
		fmt.Fprintln(stderr, "diagnostics does not accept positional arguments")
		return 2
	}
	request := runtimeapi.DiagnosticsRequest{
		IncludeRawConfig:   *includeRawConfig,
		IncludeFullLogs:    *includeFullLogs,
		IncludeNetworkInfo: *includeNetworkInfo,
		ConfirmSensitive:   *confirmSensitive,
	}
	if command == "create" &&
		(request.IncludeRawConfig || request.IncludeFullLogs || request.IncludeNetworkInfo) &&
		!request.ConfirmSensitive {
		fmt.Fprintln(stderr, "sensitive diagnostics require --confirm-sensitive after reviewing diagnostics preview")
		return 2
	}
	fmt.Fprintln(stderr, runtimeapi.SensitiveDataWarning)
	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	defer client.CloseIdleConnections()
	if command == "preview" {
		preview, err := client.PreviewDiagnostics(context.Background(), request)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(preview)
			return 0
		}
		for _, item := range preview.Items {
			sensitivity := "已脱敏"
			if item.Sensitive {
				sensitivity = "敏感"
			}
			fmt.Fprintf(stdout, "%s\t%d bytes\t%s\n", item.Name, item.Size, sensitivity)
		}
		return 0
	}
	preview, err := client.PreviewDiagnostics(context.Background(), request)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	for _, item := range preview.Items {
		sensitivity := "已脱敏"
		if item.Sensitive {
			sensitivity = "敏感"
		}
		fmt.Fprintf(stderr, "%s\t%d bytes\t%s\n", item.Name, item.Size, sensitivity)
	}
	result, err := client.CreateDiagnostics(context.Background(), request)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *jsonOutput {
		_ = json.NewEncoder(stdout).Encode(result)
	} else {
		fmt.Fprintf(stdout, "Diagnostics: %s (%d bytes, sha256:%s)\n", result.FileName, result.Size, result.SHA256)
	}
	return 0
}

func runBackup(arguments []string, stdout io.Writer, stderr io.Writer) int {
	if len(arguments) == 0 {
		fmt.Fprintln(stderr, "usage: submux-runtime backup [preview|export|inspect|restore] [options]")
		return 2
	}
	command := arguments[0]
	if command != "preview" && command != "export" && command != "inspect" && command != "restore" {
		fmt.Fprintln(stderr, "usage: submux-runtime backup [preview|export|inspect|restore] [options]")
		return 2
	}
	defaults := runtimepaths.Current()
	flags := flag.NewFlagSet("backup "+command, flag.ContinueOnError)
	flags.SetOutput(stderr)
	endpoint := flags.String("endpoint", defaults.Endpoint, "local Runtime IPC endpoint")
	jsonOutput := flags.Bool("json", false, "print stable JSON")
	includeSecrets := flags.Bool("include-secrets", false, "include secrets and produce a restorable plaintext backup")
	confirmPlaintext := flags.Bool("confirm-plaintext", false, "confirm writing an unencrypted plaintext backup")
	confirmRestore := flags.Bool("confirm", false, "confirm replacing portable Runtime state")
	wait := flags.Bool("wait", true, "wait for the restore operation to finish")
	if err := flags.Parse(arguments[1:]); err != nil {
		return 2
	}

	switch command {
	case "preview":
		if flags.NArg() != 0 || *confirmPlaintext || *confirmRestore || !*wait {
			fmt.Fprintln(stderr, "backup preview accepts only --include-secrets, --endpoint and --json")
			return 2
		}
	case "export":
		if flags.NArg() != 1 || !*confirmPlaintext || *confirmRestore || !*wait {
			fmt.Fprintln(stderr, "backup export requires --confirm-plaintext and exactly one new output file")
			return 2
		}
	case "inspect":
		if flags.NArg() != 1 || *includeSecrets || *confirmPlaintext || *confirmRestore || !*wait {
			fmt.Fprintln(stderr, "backup inspect accepts exactly one backup file")
			return 2
		}
	case "restore":
		if flags.NArg() != 1 || *includeSecrets || *confirmPlaintext || !*confirmRestore {
			fmt.Fprintln(stderr, "backup restore requires --confirm and exactly one backup file")
			return 2
		}
	}

	client, err := runtimeipc.NewClient(*endpoint, buildinfo.Current().Version)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	defer client.CloseIdleConnections()
	ctx := context.Background()

	if command == "preview" {
		preview, err := client.PreviewBackup(ctx, runtimeapi.BackupPreviewRequest{
			IncludeSecrets: *includeSecrets,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		return writeBackupPreview(stdout, *jsonOutput, preview)
	}

	if command == "export" {
		preview, err := client.PreviewBackup(ctx, runtimeapi.BackupPreviewRequest{
			IncludeSecrets: *includeSecrets,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		fmt.Fprintln(stderr, preview.Warning)
		archive, err := client.ExportBackup(ctx, runtimeapi.BackupExportRequest{
			IncludeSecrets:   *includeSecrets,
			ConfirmPlaintext: *confirmPlaintext,
		})
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
		output, err := runtimebackupfile.WriteNew(flags.Arg(0), archive.Body, runtimeapi.RuntimeBackupMaxBytes)
		if err != nil {
			writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
			return 1
		}
		archive.Body = nil
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(archive)
		} else {
			fmt.Fprintf(stdout, "Backup: %s (%d bytes, sha256:%s)\n", output, archive.Size, archive.SHA256)
			if !archive.Restorable {
				fmt.Fprintln(stdout, "This is a redacted inventory and cannot be restored.")
			}
		}
		return 0
	}

	body, err := runtimebackupfile.Read(flags.Arg(0), runtimeapi.RuntimeBackupMaxBytes)
	if err != nil {
		writeCLIError(stderr, runtimeapi.ErrorInvalidRequest, err.Error(), false)
		return 1
	}
	content, err := client.UploadImport(ctx, runtimeapi.RuntimeBackupContentType, body)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	preview, err := client.PreviewBackupRestore(ctx, runtimeapi.BackupRestorePreviewRequest{ContentID: content.ID})
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if command == "inspect" {
		if *jsonOutput {
			_ = json.NewEncoder(stdout).Encode(preview)
		} else {
			writeBackupRestorePreview(stdout, preview)
		}
		return 0
	}
	writeBackupRestorePreview(stderr, preview)
	snapshot, err := client.Observe(ctx)
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	operation, err := client.Execute(ctx, runtimeapi.CreateOperationRequest{
		IfRevision: snapshot.Revision,
		Action: runtimeapi.Action{
			Kind: runtimeapi.ActionRestoreBackup,
			Params: runtimeapi.ActionParams{
				ContentID: content.ID,
				Confirm:   true,
			},
		},
	})
	if err != nil {
		return writeClientFailure(stdout, stderr, *jsonOutput, err)
	}
	if *wait {
		operation, err = client.WaitOperation(ctx, operation.ID, 250*time.Millisecond)
		if err != nil {
			return writeClientFailure(stdout, stderr, *jsonOutput, err)
		}
	}
	return writeOperation(stdout, *jsonOutput, operation)
}

func writeBackupPreview(writer io.Writer, asJSON bool, preview runtimeapi.BackupPreview) int {
	if asJSON {
		_ = json.NewEncoder(writer).Encode(preview)
		return 0
	}
	for _, item := range preview.Items {
		included := "excluded"
		if item.Included {
			included = "included"
		}
		fmt.Fprintf(writer, "%s\t%s\t%d items\t%d bytes\n", item.Name, included, item.Count, item.Size)
	}
	fmt.Fprintln(writer, preview.Warning)
	return 0
}

func writeBackupRestorePreview(writer io.Writer, preview runtimeapi.BackupRestorePreview) {
	fmt.Fprintf(
		writer,
		"Backup created %s; %d sources, %d managed resources, %d recent configurations\n",
		preview.CreatedAt.Format(time.RFC3339),
		preview.SourceCount,
		preview.ManagedResourceCount,
		preview.RecentConfigurationCount,
	)
	if len(preview.PendingSettings) > 0 {
		fmt.Fprintf(writer, "Pending local confirmation: %s\n", strings.Join(preview.PendingSettings, ", "))
	}
	fmt.Fprintln(writer, preview.Warning)
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
		fmt.Fprintln(stderr, runtimeprivacy.RedactError(err))
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
	message := runtimeprivacy.RedactText(clientError.Message)
	if asJSON {
		_ = json.NewEncoder(stdout).Encode(runtimeapi.ErrorEnvelope{
			ProtocolVersion: runtimeapi.ProtocolVersion,
			Error: runtimeapi.ProtocolError{
				Code:      clientError.Code,
				Message:   message,
				Retryable: clientError.Retryable,
			},
			CurrentRevision: clientError.CurrentRevision,
		})
	} else {
		writeCLIError(stderr, clientError.Code, message, clientError.Retryable)
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
	fmt.Fprintf(writer, "%s: %s", code, runtimeprivacy.RedactText(message))
	if retryable {
		fmt.Fprint(writer, " (retryable)")
	}
	fmt.Fprintln(writer)
}
