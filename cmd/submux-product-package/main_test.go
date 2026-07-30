package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestBuildProductPackageAndDescriptor(t *testing.T) {
	root := t.TempDir()
	components := make(map[string]string)
	for _, name := range []string{"submux-runtime", "submux-runtime-net", "submux-runtime-gui"} {
		component := filepath.Join(root, name)
		if err := os.WriteFile(component, []byte("component:"+name), 0755); err != nil {
			t.Fatal(err)
		}
		components[name] = component
	}
	output := filepath.Join(root, "submux-runtime-v1.2.3-linux-amd64.zip")
	descriptor := filepath.Join(root, "target.json")
	result, err := buildProductPackage(packageRequest{
		Version:          "v1.2.3",
		Platform:         "linux",
		Arch:             "amd64",
		Runtime:          components["submux-runtime"],
		RuntimeNet:       components["submux-runtime-net"],
		GUI:              components["submux-runtime-gui"],
		Output:           output,
		Descriptor:       descriptor,
		ReleaseNotes:     "Test release.",
		MigrationSummary: "No migration.",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.SHA256 == "" {
		t.Fatal("missing package digest")
	}
	body, err := os.ReadFile(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	var target targetDescriptor
	if err := json.Unmarshal(body, &target); err != nil {
		t.Fatal(err)
	}
	if target.Path != "product/v1.2.3/linux/amd64/"+filepath.Base(output) {
		t.Fatalf("unexpected target path %q", target.Path)
	}
	var custom productCustom
	if err := json.Unmarshal(target.Custom, &custom); err != nil {
		t.Fatal(err)
	}
	if custom.Kind != "submux-runtime-product" || len(custom.Components) != 3 {
		t.Fatalf("unexpected custom metadata %#v", custom)
	}
}

func TestBuildProductPackageRejectsExistingOutput(t *testing.T) {
	root := t.TempDir()
	output := filepath.Join(root, "existing.zip")
	if err := os.WriteFile(output, []byte("existing"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := validateOutputPaths(output, ""); err == nil {
		t.Fatal("expected existing output rejection")
	}
}
