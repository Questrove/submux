package releasepolicy

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestWorkflowYAMLParses(t *testing.T) {
	workflows, err := filepath.Glob(filepath.Join("..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(workflows) == 0 {
		t.Fatal("no GitHub Actions workflows found")
	}
	for _, name := range workflows {
		name := name
		t.Run(filepath.Base(name), func(t *testing.T) {
			body, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			var document yaml.Node
			if err := yaml.Unmarshal(body, &document); err != nil {
				t.Fatalf("parse workflow YAML: %v", err)
			}
		})
	}
}
