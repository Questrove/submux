package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"submux/internal/agent"
	"submux/internal/agentclient"
	"submux/internal/agentlocal"
	"submux/internal/agentproto"
	"submux/internal/agentstate"
	"submux/internal/buildinfo"
	"submux/internal/hostops"
	"submux/internal/integration"
	"submux/internal/mihomo"
)

type paths struct {
	state, socket, config, core, runtime string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "submux-agent:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return nil
	}
	if len(args) == 1 && (args[0] == "--version" || args[0] == "version") {
		info := buildinfo.Current()
		fmt.Printf("submux-agent %s (%s, %s)\n", info.Version, info.Commit, info.Date)
		return nil
	}
	switch args[0] {
	case "enroll":
		return runEnroll(args[1:])
	case "serve":
		if len(args) != 1 {
			return errors.New("serve accepts no arguments")
		}
		return runAgentService(runServe)
	case "service":
		return runAgentServiceCommand(args[1:])
	case "unenroll":
		return runUnenroll(args[1:])
	case "status":
		return localPrint(http.MethodGet, "/v1/status", nil)
	case "doctor":
		return localPrint(http.MethodGet, "/v1/doctor", nil)
	case "logs":
		return localPrint(http.MethodGet, "/v1/logs", nil)
	case "mihomo":
		return runMihomo(args[1:])
	case "subscription":
		return runSubscription(args[1:])
	case "proxy":
		return runProxy(args[1:])
	case "help", "--help", "-h":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage() {
	fmt.Print(`Usage: submux-agent <command>

  enroll --server https://submux.example.com [--code CODE]
  serve
  service start|stop|status                  (Linux systemd user unit)
  unenroll [--force-local] [--yes]
  status | doctor | logs
  mihomo install --version vX.Y.Z [--channel stable|alpha]
  mihomo status | restart | rollback
  subscription status | rollback
  proxy env bash|powershell
  proxy shell
`)
}

func runEnroll(args []string) error {
	flags := flag.NewFlagSet("enroll", flag.ContinueOnError)
	serverURL := flags.String("server", "", "submux control plane URL")
	code := flags.String("code", "", "one-time pairing code")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || *serverURL == "" {
		return errors.New("enroll requires --server and accepts no positional arguments")
	}
	if err := secureControlURL(*serverURL); err != nil {
		return err
	}
	if *code == "" {
		fmt.Fprint(os.Stderr, "Pairing code: ")
		if _, err := fmt.Fscanln(os.Stdin, code); err != nil {
			return errors.New("could not read pairing code")
		}
	}
	paths := defaultPaths()
	statePath := envOr("SUBMUX_AGENT_STATE", paths.state)
	if err := prepareAgentStateLocation(statePath); err != nil {
		return err
	}
	state, err := agentstate.Open(statePath)
	if err != nil {
		return err
	}
	defer state.Close()
	if _, err := state.Identity(); err == nil {
		return errors.New("agent is already enrolled; revoke the existing instance before enrolling again")
	}
	publicKey, privateKey, err := agentproto.GenerateDeviceKey()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	response, err := agentclient.Enroll(ctx, nil, *serverURL, agentclient.EnrollmentRequest{
		Code: *code, PublicKey: agentproto.EncodePublicKey(publicKey), OS: runtime.GOOS, Arch: runtime.GOARCH,
		AgentVersion: buildinfo.Current().Version, Capabilities: agent.PlatformCapabilities(),
	})
	if err != nil {
		return err
	}
	if err := state.SaveIdentity(agentstate.Identity{
		ServerURL: strings.TrimRight(*serverURL, "/"), InstanceID: response.InstanceID,
		PublicKey: agentproto.EncodePublicKey(publicKey), PrivateKey: base64.RawURLEncoding.EncodeToString(privateKey),
		EnrolledAt: time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		return err
	}
	fmt.Printf("enrolled runtime instance %d\n", response.InstanceID)
	if err := state.Close(); err != nil {
		return err
	}
	if err := activateInstalledService(); err != nil {
		return fmt.Errorf("enrollment was saved, but the installed service could not be started: %w", err)
	}
	return nil
}

func runServe(ctx context.Context) error {
	serveCtx, stopServe := context.WithCancel(ctx)
	defer stopServe()
	defaults := defaultPaths()
	statePath := envOr("SUBMUX_AGENT_STATE", defaults.state)
	if err := prepareAgentStateLocation(statePath); err != nil {
		return err
	}
	state, err := agentstate.Open(statePath)
	if err != nil {
		return err
	}
	defer state.Close()
	identity, err := state.Identity()
	if err != nil {
		return err
	}
	privateKey, err := identity.PrivateKeyBytes()
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return errors.New("stored device private key is invalid")
	}
	runtimeState, err := state.Runtime()
	if err != nil {
		return err
	}
	if runtimeState.MihomoSecret == "" {
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return err
		}
		runtimeState, err = state.UpdateRuntime(func(value *agentstate.Runtime) error {
			value.MihomoSecret = hex.EncodeToString(secret)
			return nil
		})
		if err != nil {
			return err
		}
	}
	core, err := hostops.NewUserManager(defaults.core, defaults.config, defaults.runtime, nil)
	if err != nil {
		return err
	}
	resourceProxy := agentproto.NormalizeResourceProxy(agentproto.ResourceProxy{Mode: runtimeState.ResourceProxyMode, URL: runtimeState.ResourceProxyURL})
	if err := agentproto.ValidateResourceProxy(resourceProxy); err != nil {
		return errors.New("stored Agent resource proxy is invalid")
	}
	proxyController, supported := core.(hostops.ResourceProxyController)
	if resourceProxy.Mode == agentproto.ResourceProxyCustom && !supported {
		return errors.New("this Agent build cannot restore its stored resource proxy")
	}
	if supported {
		proxyURL := ""
		if resourceProxy.Mode == agentproto.ResourceProxyCustom {
			proxyURL = resourceProxy.URL
		}
		if err := proxyController.SetResourceProxy(proxyURL); err != nil {
			return errors.New("stored Agent resource proxy could not be restored")
		}
	}
	defer core.Stop(context.Background())
	controllerURL := "http://127.0.0.1:" + strconv.Itoa(runtimeState.ControllerPort)
	mihomoClient, err := mihomo.NewClient(controllerURL, runtimeState.MihomoSecret, nil)
	if err != nil {
		return err
	}
	verifier := &mihomo.RuntimeCheck{Client: mihomoClient}
	deployer := &mihomo.Deployer{
		Root: defaults.config, ControllerAddr: "127.0.0.1:" + strconv.Itoa(runtimeState.ControllerPort),
		Secret: runtimeState.MihomoSecret, Validator: core, Service: core, Verifier: verifier,
	}
	control := &agentclient.Client{ServerURL: identity.ServerURL, InstanceID: identity.InstanceID, PrivateKey: ed25519.PrivateKey(privateKey)}
	daemon := &agent.Daemon{State: state, Control: control, Core: core, Deployer: deployer, Mihomo: mihomoClient, VerifyRuntime: func(ctx context.Context, port int) error {
		address := ""
		if port > 0 {
			address = net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
		}
		return verifier.VerifyRuntime(ctx, address)
	}, AgentVersion: buildinfo.Current().Version, Capabilities: agent.PlatformCapabilities()}
	localAPI := &agentlocal.API{State: state, Core: core, Daemon: daemon, Stop: stopServe}
	listener, err := agentlocal.Listen(envOr("SUBMUX_AGENT_SOCKET", defaults.socket))
	if err != nil {
		return err
	}
	defer listener.Close()
	server := &http.Server{Handler: localAPI.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	err = daemon.Run(serveCtx)
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = server.Shutdown(shutdownCtx)
	return err
}

func runUnenroll(args []string) error {
	flags := flag.NewFlagSet("unenroll", flag.ContinueOnError)
	forceLocal := flags.Bool("force-local", false, "destroy the local device identity even if the control plane is unavailable")
	yes := flags.Bool("yes", false, "skip the interactive confirmation")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("unenroll accepts only --force-local and --yes")
	}
	if !*yes {
		message := "Revoke this runtime instance and erase its local device identity? [y/N]: "
		if *forceLocal {
			message = "Force a local identity wipe without confirming remote revocation? [y/N]: "
		}
		fmt.Fprint(os.Stderr, message)
		var answer string
		if _, err := fmt.Fscanln(os.Stdin, &answer); err != nil || (answer != "y" && answer != "Y" && !strings.EqualFold(answer, "yes")) {
			return errors.New("unenroll cancelled")
		}
	}
	body, _ := json.Marshal(map[string]bool{"force_local": *forceLocal})
	return localPrint(http.MethodPost, "/v1/unenroll", body)
}

func runMihomo(args []string) error {
	if len(args) == 0 {
		return errors.New("mihomo requires install, status, restart or rollback")
	}
	switch args[0] {
	case "status":
		if len(args) != 1 {
			return errors.New("mihomo status accepts no arguments")
		}
		return localPrint(http.MethodGet, "/v1/status", nil)
	case "restart":
		if len(args) != 1 {
			return errors.New("mihomo restart accepts no arguments")
		}
		return localPrint(http.MethodPost, "/v1/mihomo/restart", []byte(`{}`))
	case "rollback":
		if len(args) != 1 {
			return errors.New("mihomo rollback accepts no arguments")
		}
		return localPrint(http.MethodPost, "/v1/mihomo/rollback", []byte(`{}`))
	case "install":
		flags := flag.NewFlagSet("mihomo install", flag.ContinueOnError)
		version := flags.String("version", "", "exact Mihomo version")
		channel := flags.String("channel", "stable", "stable or alpha")
		if err := flags.Parse(args[1:]); err != nil {
			return err
		}
		if flags.NArg() != 0 || *version == "" || (*channel != "stable" && *channel != "alpha") {
			return errors.New("mihomo install requires --version and optional --channel stable|alpha")
		}
		body, _ := json.Marshal(map[string]string{"version": *version, "channel": *channel})
		return localPrint(http.MethodPost, "/v1/mihomo/install", body)
	default:
		return fmt.Errorf("unknown mihomo command %q", args[0])
	}
}

func runSubscription(args []string) error {
	if len(args) != 1 {
		return errors.New("subscription requires exactly one of status or rollback")
	}
	switch args[0] {
	case "status":
		return localPrint(http.MethodGet, "/v1/status", nil)
	case "rollback":
		return localPrint(http.MethodPost, "/v1/subscription/"+args[0], []byte(`{}`))
	default:
		return fmt.Errorf("unknown subscription command %q", args[0])
	}
}

func runProxy(args []string) error {
	if len(args) == 2 && args[0] == "env" && (args[1] == "bash" || args[1] == "powershell") {
		return localPrint(http.MethodGet, "/v1/proxy/env/"+args[1], nil)
	}
	if len(args) == 1 && args[0] == "shell" {
		status, err := localCall(http.MethodGet, "/v1/status", nil)
		if err != nil {
			return err
		}
		var value struct {
			Runtime struct {
				ProxyPort  int    `json:"proxy_port"`
				ProxyKind  string `json:"proxy_kind"`
				CoreStatus string `json:"core_status"`
			} `json:"runtime"`
		}
		if err := json.Unmarshal(status, &value); err != nil {
			return err
		}
		if value.Runtime.CoreStatus != "running" {
			return errors.New("Mihomo is not running")
		}
		environment, err := integration.ProxyEnvironmentFor(value.Runtime.ProxyPort, value.Runtime.ProxyKind)
		if err != nil {
			return err
		}
		shell, shellArgs := defaultShell()
		command := exec.Command(shell, shellArgs...)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		command.Env = mergeEnvironment(os.Environ(), environment)
		return command.Run()
	}
	return errors.New("proxy requires 'env bash', 'env powershell', or 'shell'")
}

func localPrint(method, path string, body []byte) error {
	value, err := localCall(method, path, body)
	if err != nil {
		return err
	}
	_, err = os.Stdout.Write(value)
	return err
}

func localCall(method, path string, body []byte) ([]byte, error) {
	defaults := defaultPaths()
	client, err := agentlocal.NewClient(envOr("SUBMUX_AGENT_SOCKET", defaults.socket))
	if err != nil {
		return nil, err
	}
	request, err := http.NewRequest(method, "http://local"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	value, err := io.ReadAll(io.LimitReader(response.Body, 2<<20))
	if err != nil {
		return nil, err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("local Agent returned %d: %s", response.StatusCode, strings.TrimSpace(string(value)))
	}
	return value, nil
}

func secureControlURL(raw string) error {
	value, err := url.Parse(raw)
	if err != nil || value.Host == "" || value.User != nil || value.RawQuery != "" || value.Fragment != "" {
		return errors.New("server must be an absolute URL without user info")
	}
	if value.Scheme == "https" {
		return nil
	}
	host := value.Hostname()
	ip := net.ParseIP(host)
	if value.Scheme == "http" && (strings.EqualFold(host, "localhost") || ip != nil && ip.IsLoopback()) {
		return nil
	}
	return errors.New("server must use HTTPS; plain HTTP is allowed only for loopback enrollment")
}

func mergeEnvironment(current []string, additions map[string]string) []string {
	result := make([]string, 0, len(current)+len(additions))
	for _, value := range current {
		key, _, _ := strings.Cut(value, "=")
		if _, replaced := additions[strings.ToUpper(key)]; !replaced {
			result = append(result, value)
		}
	}
	for key, value := range additions {
		result = append(result, key+"="+value)
	}
	return result
}

func envOr(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
