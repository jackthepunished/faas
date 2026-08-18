package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/onebox-faas/faas/pkg/manifest"
)

func TestRenderManifestAnsibleFiles_DerivesRouting(t *testing.T) {
	yaml := strings.Replace(validManifestYAML,
		"    - name: fsn-1\n      role: control-plane\n",
		"    - name: fsn-1\n      role: control-plane\n      address: 10.42.0.1:7100\n    - name: fsn-2\n      role: compute-only\n      address: 10.42.0.2:50051\n", 1)
	m, err := manifest.Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("manifest.Parse: %v", err)
	}
	if errs := m.Validate(); errs != nil {
		t.Fatalf("manifest.Validate: %v", errs)
	}

	files, err := renderManifestAnsibleFiles(m, t.TempDir())
	if err != nil {
		t.Fatalf("renderManifestAnsibleFiles: %v", err)
	}
	if len(files) != 3 {
		t.Fatalf("generated files = %d, want inventory + 2 host_vars", len(files))
	}
	var inventory, computeVars string
	for _, file := range files {
		switch {
		case strings.HasSuffix(file.Path, filepath.Join("inventory", "hosts.ini")):
			inventory = string(file.Body)
		case strings.HasSuffix(file.Path, "fsn-2.yml"):
			computeVars = string(file.Body)
		}
	}
	if !strings.Contains(inventory, "[compute_nodes]\nfsn-2\n") {
		t.Errorf("inventory missing compute node:\n%s", inventory)
	}
	if !strings.Contains(computeVars, `ansible_host: "10.42.0.2"`) {
		t.Errorf("compute host vars missing host address:\n%s", computeVars)
	}
	if !strings.Contains(computeVars, `faas_vmmd_target_url: "tcp://vmmd.faas:50051"`) {
		t.Errorf("compute host vars missing derived target:\n%s", computeVars)
	}
	if !strings.Contains(computeVars, `10.42.0.1"`) || !strings.Contains(computeVars, `schedd.faas`) {
		t.Errorf("compute host vars missing control-plane private alias:\n%s", computeVars)
	}
	if !strings.Contains(computeVars, `10.42.0.2"`) || !strings.Contains(computeVars, `vmmd.faas`) {
		t.Errorf("compute host vars missing compute private alias:\n%s", computeVars)
	}
}

func TestWriteGeneratedAnsibleFile_RefusesDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory", "hosts.ini")
	if err := writeGeneratedAnsibleFile(path, []byte("first\n"), false); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := writeGeneratedAnsibleFile(path, []byte("second\n"), false); err == nil {
		t.Fatal("drifted generated file was overwritten without --force")
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "first\n" {
		t.Fatalf("file after refused overwrite = %q, err=%v", got, err)
	}
}
