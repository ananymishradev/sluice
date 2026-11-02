package stage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestStageIncludesExtraTop(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	for _, dir := range []string{"system", "constant", "0"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Allrun"), []byte("#!/bin/sh\n"), 0o750); err != nil {
		t.Fatal(err)
	}
	s, err := New(cache)
	if err != nil {
		t.Fatal(err)
	}
	// Without include, Allrun is ignored.
	base, err := s.Stage(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base.Path, "Allrun")); !os.IsNotExist(err) {
		t.Fatal("Allrun must not be staged without include")
	}
	// With include, it is staged and affects the hash.
	with, err := s.Stage(root, "Allrun")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(with.Path, "Allrun")); err != nil {
		t.Fatalf("Allrun must be staged with include: %v", err)
	}
	if with.Hash == base.Hash {
		t.Fatal("include must affect the case hash")
	}
}

func TestStageAcceptsZeroOrig(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
	for _, dir := range []string{"system", "constant", "0.orig"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "system", "controlDict"), []byte("application simpleFoam;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "0.orig", "p"), []byte("internalField uniform 0;\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err := New(cache)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Stage(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Path, "0.orig", "p")); err != nil {
		t.Fatalf("0.orig must be staged: %v", err)
	}
}

func TestStageAcceptsInitialDir(t *testing.T) {
	root := t.TempDir()
	cache := filepath.Join(t.TempDir(), "cache")
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
	s, err := New(cache)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Stage(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(res.Path, "initial", "alpha.metal")); err != nil {
		t.Fatalf("initial/ must be staged (laserbeamFoam convention): %v", err)
	}
}
