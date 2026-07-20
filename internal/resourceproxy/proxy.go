package resourceproxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"submux/internal/store"
)

const (
	ModeDirect = "direct"
	ModeHTTP   = "http"
	ModeSOCKS5 = "socks5"

	SettingMode = "platform_resource_proxy_mode"
	SettingURL  = "platform_resource_proxy_url"
)

// Config applies only to resource requests made by the submux control plane.
// It never changes process-wide environment variables or another program's
// proxy settings.
type Config struct {
	Mode string `json:"mode"`
	URL  string `json:"url,omitempty"`
}

func Normalize(value Config) Config {
	value.Mode = strings.TrimSpace(value.Mode)
	value.URL = strings.TrimSpace(value.URL)
	if value.Mode == "" {
		value.Mode = ModeDirect
	}
	return value
}

func Validate(value Config) error {
	value = Normalize(value)
	if value.Mode == ModeDirect {
		if value.URL != "" {
			return errors.New("direct mode does not accept a proxy URL")
		}
		return nil
	}
	if value.Mode != ModeHTTP && value.Mode != ModeSOCKS5 {
		return errors.New("platform resource proxy mode must be direct, http or socks5")
	}
	parsed, err := url.Parse(value.URL)
	if err != nil || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "" {
		return errors.New("platform resource proxy URL must not contain credentials, path, query or fragment")
	}
	expectedScheme := value.Mode
	if parsed.Scheme != expectedScheme {
		return fmt.Errorf("platform resource proxy URL scheme must be %s", expectedScheme)
	}
	port, err := strconv.Atoi(parsed.Port())
	if parsed.Hostname() == "" || len(parsed.Hostname()) > 253 || strings.ContainsAny(parsed.Hostname(), "\r\n\x00") || err != nil || port < 1 || port > 65535 {
		return errors.New("platform resource proxy URL must contain a valid host and port")
	}
	return nil
}

func Load(st *store.Store) (Config, error) {
	mode, err := st.GetSetting(SettingMode)
	if err != nil {
		return Config{}, err
	}
	rawURL, err := st.GetSetting(SettingURL)
	if err != nil {
		return Config{}, err
	}
	value := Normalize(Config{Mode: mode, URL: rawURL})
	if err := Validate(value); err != nil {
		return Config{}, err
	}
	return value, nil
}

func Save(st *store.Store, value Config) error {
	value = Normalize(value)
	if err := Validate(value); err != nil {
		return err
	}
	if err := st.SetSetting(SettingMode, value.Mode); err != nil {
		return err
	}
	return st.SetSetting(SettingURL, value.URL)
}

// NewClient clones the default transport but always sets Proxy explicitly.
// Direct resource requests therefore cannot silently inherit HTTP_PROXY or
// HTTPS_PROXY from the server process.
func NewClient(value Config, timeout time.Duration) (*http.Client, error) {
	value = Normalize(value)
	if err := Validate(value); err != nil {
		return nil, err
	}
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		return nil, errors.New("default HTTP transport is unavailable")
	}
	clone := transport.Clone()
	clone.Proxy = nil
	if value.Mode != ModeDirect {
		parsed, _ := url.Parse(value.URL)
		clone.Proxy = http.ProxyURL(parsed)
	}
	return &http.Client{Transport: clone, Timeout: timeout}, nil
}
