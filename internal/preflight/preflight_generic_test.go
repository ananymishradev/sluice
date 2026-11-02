package preflight

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestAcceptsZeroOrig(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "0.orig"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).Run(context.Background(), root); err != nil {
		t.Fatalf("0.orig must satisfy preflight: %v", err)
	}
}

func TestAcceptsInitialDir(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "initial"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application laserbeamFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "initial", "alpha.metal"), []byte("internalField uniform 0;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).Run(context.Background(), root); err != nil {
		t.Fatalf("initial/ must satisfy preflight (laserbeamFoam convention): %v", err)
	}
}

func TestInitialDirentivesScanned(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "initial"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "initial", "T"), []byte("internalField #codeStream{code (gogogo);};\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).Run(context.Background(), root); err == nil {
		t.Fatal("executable directives under initial/ must be rejected like 0/")
	}
}

func TestAllowedDirectivesExempt(t *testing.T) {
	root := t.TempDir()
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("libs (libfoo);\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := (Runner{}).Run(context.Background(), root); err == nil {
		t.Fatal("expected libs to be rejected by default")
	}
	r := Runner{AllowedDirectives: []string{"libs"}}
	if _, err := r.Run(context.Background(), root); err != nil {
		t.Fatalf("allowed libs must pass: %v", err)
	}
}
