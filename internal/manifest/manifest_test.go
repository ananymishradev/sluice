package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadResolvesRelativePaths(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(caseDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(root, "manifest.yaml")
	data := []byte("apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\nresources:\n  partition: normal\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\nsolver:\n  name: simpleFoam\n")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Case.Path != caseDir {
		t.Fatalf("case path = %q", m.Case.Path)
	}
}

func TestLoadBuildSectionResolvesRelativeSource(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(caseDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "solver", "Make"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "solver", "Make", "files"), []byte("EXE = mySolver\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "manifest.yaml")
	data := []byte("apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\nresources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\nsolver:\n  name: mySolver\nbuild:\n  source: ./solver\n")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if m.Build == nil || m.Build.Source != filepath.Join(root, "solver") {
		t.Fatalf("build.source = %+v", m.Build)
	}
}

func TestLoadBuildSourceMustBeWmakeProject(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(caseDir, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	empty := filepath.Join(root, "nope")
	if err := os.MkdirAll(empty, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "manifest.yaml")
	data := []byte("apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\nresources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\nsolver:\n  name: mySolver\nbuild:\n  source: " + empty + "\n")
	if err := os.WriteFile(path, data, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "Make/files") {
		t.Fatalf("expected Make/files error, got %v", err)
	}
}
