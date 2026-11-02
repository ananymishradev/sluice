package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestUpdateReleaseVerifiesAndReplacesExecutable(t *testing.T) {
	binary := []byte("new sluice executable")
	sum := sha256.Sum256(binary)
	var server *httptest.Server
	server = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/latest":
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName: "v2.0.0",
				Assets: []releaseAsset{
					{Name: fmt.Sprintf("sluice-%s-%s", runtime.GOOS, runtime.GOARCH), BrowserDownloadURL: server.URL + "/binary"},
					{Name: fmt.Sprintf("sluice-%s-%s.sha256", runtime.GOOS, runtime.GOARCH), BrowserDownloadURL: server.URL + "/checksum"},
				},
			})
		case "/binary":
			_, _ = w.Write(binary)
		case "/checksum":
			_, _ = fmt.Fprintf(w, "%s  sluice-%s-%s\n", hex.EncodeToString(sum[:]), runtime.GOOS, runtime.GOARCH)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "sluice")
	if err := os.WriteFile(path, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := updateRelease(context.Background(), server.Client(), server.URL+"/latest", path, "1.0.0", &output); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(binary) {
		t.Fatalf("installed bytes = %q, want %q", got, binary)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("updated binary is not executable: mode %v", info.Mode().Perm())
	}
	if !strings.Contains(output.String(), "Updated Sluice 1.0.0 to v2.0.0") {
		t.Fatalf("unexpected update output: %s", output.String())
	}
}

func TestUpdateReleaseChecksumMismatchDoesNotReplaceExecutable(t *testing.T) {
	binary := []byte("untrusted executable")
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest" {
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName: "v2.0.0",
				Assets: []releaseAsset{
					{Name: fmt.Sprintf("sluice-%s-%s", runtime.GOOS, runtime.GOARCH), BrowserDownloadURL: serverURL(r) + "/binary"},
					{Name: fmt.Sprintf("sluice-%s-%s.sha256", runtime.GOOS, runtime.GOARCH), BrowserDownloadURL: serverURL(r) + "/checksum"},
				},
			})
			return
		}
		if r.URL.Path == "/binary" {
			_, _ = w.Write(binary)
		} else if r.URL.Path == "/checksum" {
			_, _ = w.Write([]byte(strings.Repeat("0", sha256.Size*2)))
		} else {
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "sluice")
	if err := os.WriteFile(path, []byte("old executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := updateRelease(context.Background(), server.Client(), server.URL+"/latest", path, "1.0.0", &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "SHA-256 verification failed") {
		t.Fatalf("update error = %v, want checksum failure", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "old executable" {
		t.Fatalf("failed update changed executable to %q", got)
	}
}

func TestUpdateReleaseDoesNotDowngrade(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(githubRelease{TagName: "v1.0.0"})
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "sluice")
	if err := os.WriteFile(path, []byte("current executable"), 0o755); err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	if err := updateRelease(context.Background(), server.Client(), server.URL+"/latest", path, "2.0.0", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "newer than the latest published release") {
		t.Fatalf("unexpected update output: %s", output.String())
	}
}

func TestFindReleaseAssetsRequiresBinaryAndChecksum(t *testing.T) {
	assets := []releaseAsset{
		{Name: "sluice-linux-amd64", BrowserDownloadURL: "https://example.test/sluice"},
		{Name: "sluice-linux-amd64.sha256", BrowserDownloadURL: "https://example.test/sluice.sha256"},
	}
	binary, checksum := findReleaseAssets(assets, "sluice-linux-amd64")
	if binary.Name != "sluice-linux-amd64" || checksum.Name != "sluice-linux-amd64.sha256" {
		t.Fatalf("release assets = %q and %q", binary.Name, checksum.Name)
	}
	binary, checksum = findReleaseAssets(assets[:1], "sluice-linux-amd64")
	if binary.Name != "" || checksum.Name != "" {
		t.Fatalf("assets without a checksum must be rejected, got %q and %q", binary.Name, checksum.Name)
	}
}

func serverURL(r *http.Request) string {
	return "https://" + r.Host
}
