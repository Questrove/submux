package hostops

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type userManagerReleaseSource struct{}

func (userManagerReleaseSource) Fetch(_ context.Context, _, version, _, _ string) (ReleaseBinary, error) {
	return ReleaseBinary{Version: version, Binary: []byte("verified-user-mihomo"), AssetDigest: strings.Repeat("a", 64)}, nil
}

func (userManagerReleaseSource) ListVersions(_ context.Context, channel, _, _ string, _ int) ([]string, error) {
	if channel == "alpha" {
		return []string{"alpha-e911985"}, nil
	}
	return []string{"v1.19.28"}, nil
}

type userManagerRunner struct{}

func (userManagerRunner) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	if len(args) == 1 && args[0] == "-v" {
		return []byte("Mihomo Meta v1.2.3 v1.2.4"), nil
	}
	return nil, nil
}

func TestUserManagerLifecycleStaysInsideUserDataRoot(t *testing.T) {
	root := t.TempDir()
	manager := &UserManager{
		CoreRoot: filepath.Join(root, "core"), ConfigRoot: filepath.Join(root, "config"), RuntimeRoot: filepath.Join(root, "runtime"),
		Source: userManagerReleaseSource{}, Runner: userManagerRunner{},
	}
	ctx := context.Background()
	if err := manager.Install(ctx, "stable", "v1.2.3"); err != nil {
		t.Fatal(err)
	}
	if err := manager.Install(ctx, "stable", "v1.2.4"); err != nil {
		t.Fatal(err)
	}
	status, err := manager.Status(ctx)
	if err != nil || status.Version != "v1.2.4" || status.PreviousVersion != "v1.2.3" || status.State != "stopped" {
		t.Fatalf("unexpected user manager status: %#v, %v", status, err)
	}
	if err := manager.RollbackCore(ctx); err != nil {
		t.Fatal(err)
	}
	status, err = manager.Status(ctx)
	if err != nil || status.Version != "v1.2.3" || status.PreviousVersion != "v1.2.4" {
		t.Fatalf("unexpected rollback status: %#v, %v", status, err)
	}
	if err := manager.Uninstall(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(manager.CoreRoot, "current")); !os.IsNotExist(err) {
		t.Fatalf("uninstall retained the managed current core: %v", err)
	}
	for _, forbidden := range []string{"unit", "service", "docker"} {
		if _, err := os.Stat(filepath.Join(root, forbidden)); !os.IsNotExist(err) {
			t.Fatalf("user manager created an unrelated host path %q", forbidden)
		}
	}
}

func TestUserManagerRejectsBroadOrRelativeRoots(t *testing.T) {
	for _, manager := range []*UserManager{
		{CoreRoot: ".", ConfigRoot: filepath.Join(t.TempDir(), "config"), RuntimeRoot: filepath.Join(t.TempDir(), "runtime")},
		{CoreRoot: filepath.VolumeName(t.TempDir()) + string(filepath.Separator), ConfigRoot: filepath.Join(t.TempDir(), "config"), RuntimeRoot: filepath.Join(t.TempDir(), "runtime")},
	} {
		if err := manager.validatePaths(); err == nil {
			t.Fatal("unsafe user data root was accepted")
		}
	}
}

func TestUserManagerVersionListingUsesConfiguredReleaseProxy(t *testing.T) {
	root := t.TempDir()
	core, err := NewUserManager(filepath.Join(root, "core"), filepath.Join(root, "config"), filepath.Join(root, "runtime"), nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := core.(*UserManager)
	if err := manager.SetResourceProxy("http://127.0.0.1:18080"); err != nil {
		t.Fatal(err)
	}
	source := manager.Source.(*releaseSource)
	request, _ := http.NewRequest(http.MethodGet, officialAPIBase+"/releases", nil)
	proxyURL, err := source.httpClient.Transport.(*http.Transport).Proxy(request)
	if err != nil || proxyURL == nil || proxyURL.String() != "http://127.0.0.1:18080" {
		t.Fatalf("release list proxy = %v, %v", proxyURL, err)
	}
	manager.Source = userManagerReleaseSource{}
	versions, err := manager.ListCoreVersions(context.Background(), "stable", 30)
	if err != nil || len(versions) != 1 || versions[0] != "v1.19.28" {
		t.Fatalf("listed versions = %#v, %v", versions, err)
	}
}
