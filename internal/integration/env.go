package integration

import (
	"errors"
	"fmt"
	"strings"
)

var defaultNoProxy = []string{
	"localhost", "127.0.0.1", "::1", ".local",
	"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
}

func ProxyEnvironment(port int) (map[string]string, error) {
	return ProxyEnvironmentFor(port, "mixed")
}

func ProxyEnvironmentFor(port int, kind string) (map[string]string, error) {
	if port < 1 || port > 65535 {
		return nil, errors.New("proxy port is out of range")
	}
	httpProxy := fmt.Sprintf("http://127.0.0.1:%d", port)
	allProxy := fmt.Sprintf("socks5h://127.0.0.1:%d", port)
	switch kind {
	case "mixed":
	case "http":
		allProxy = httpProxy
	case "socks5":
		httpProxy = allProxy
	default:
		return nil, errors.New("proxy listener kind is unavailable")
	}
	return map[string]string{
		"HTTP_PROXY": httpProxy, "HTTPS_PROXY": httpProxy,
		"ALL_PROXY": allProxy,
		"NO_PROXY":  strings.Join(defaultNoProxy, ","),
	}, nil
}

func RenderProxyEnvironment(shell string, port int) (string, error) {
	return RenderProxyEnvironmentFor(shell, port, "mixed")
}

func RenderProxyEnvironmentFor(shell string, port int, kind string) (string, error) {
	values, err := ProxyEnvironmentFor(port, kind)
	if err != nil {
		return "", err
	}
	keys := []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"}
	var output strings.Builder
	switch shell {
	case "bash", "sh", "zsh":
		for _, key := range keys {
			fmt.Fprintf(&output, "export %s='%s'\n", key, values[key])
		}
	case "powershell":
		for _, key := range keys {
			fmt.Fprintf(&output, "$env:%s = '%s'\n", key, values[key])
		}
	default:
		return "", errors.New("shell must be bash or powershell")
	}
	return output.String(), nil
}
