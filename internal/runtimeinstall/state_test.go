package runtimeinstall

import (
	"path/filepath"
	"testing"
	"time"
)

func TestInstallStateMachine(t *testing.T) {
	root := filepath.Join(t.TempDir(), "program")
	request := Request{
		Version:            "v2.0.0",
		InstallRoot:        root,
		Platform:           "windows",
		Arch:               "amd64",
		PackageKind:        PackageOffline,
		DatabaseCompatible: true,
	}
	fresh, err := Decide(nil, request)
	if err != nil || fresh.Mode != ModeFresh || !fresh.PreserveState {
		t.Fatalf("fresh plan=%#v err=%v", fresh, err)
	}
	receipt, err := NewReceipt(fresh, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	request.Version = "v2.1.0"
	upgrade, err := Decide(&receipt, request)
	if err != nil || upgrade.Mode != ModeUpgrade {
		t.Fatalf("upgrade plan=%#v err=%v", upgrade, err)
	}
	request.Version = receipt.Version
	if _, err := Decide(&receipt, request); err == nil {
		t.Fatal("same-version install did not require repair")
	}
	request.Repair = true
	repair, err := Decide(&receipt, request)
	if err != nil || repair.Mode != ModeRepair {
		t.Fatalf("repair plan=%#v err=%v", repair, err)
	}
	request.Repair = false
	request.Version = "v1.9.0"
	if _, err := Decide(&receipt, request); err == nil {
		t.Fatal("downgrade did not require authorization")
	}
	request.AllowDowngrade = true
	downgrade, err := Decide(&receipt, request)
	if err != nil || downgrade.Mode != ModeDowngrade {
		t.Fatalf("downgrade plan=%#v err=%v", downgrade, err)
	}
}

func TestInstallStateMachineRejectsSecondInstanceAndUnsupportedStableClaims(t *testing.T) {
	current := Receipt{
		FormatVersion: ReceiptFormatVersion,
		ProductID:     ProductID,
		Version:       "v1.0.0",
		InstallRoot:   filepath.Join(t.TempDir(), "first"),
		Platform:      "linux",
		Arch:          "amd64",
		PackageKind:   PackageOnline,
		InstalledAt:   time.Now().UTC(),
	}
	_, err := Decide(&current, Request{
		Version:            "v2.0.0",
		InstallRoot:        filepath.Join(t.TempDir(), "second"),
		Platform:           "linux",
		Arch:               "amd64",
		PackageKind:        PackageOnline,
		DatabaseCompatible: true,
	})
	if err == nil {
		t.Fatal("second Runtime installation target was accepted")
	}
	for _, item := range []struct {
		platform string
		arch     string
	}{
		{platform: "darwin", arch: "universal"},
		{platform: "windows", arch: "arm64"},
		{platform: "linux-musl", arch: "amd64"},
		{platform: "openwrt", arch: "arm64"},
		{platform: "linux-nonsystemd", arch: "amd64"},
	} {
		if !IsPreviewPlatform(item.platform, item.arch) {
			t.Fatalf("%s/%s was marked stable", item.platform, item.arch)
		}
	}
}
