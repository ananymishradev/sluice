package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/ananymishradev/sluice/internal/brand"
	"github.com/spf13/cobra"
)

const (
	latestReleaseURL = "https://api.github.com/repos/ananymishradev/sluice/releases/latest"
	maxReleaseSize   = 100 << 20
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

func newUpdateCommand() *cobra.Command {
	return &cobra.Command{
		Use:     "update",
		Short:   "Update Sluice to the latest published release",
		Long:    "Download and verify the latest Sluice release, then atomically replace this executable.",
		Example: "  " + brand.Name + " update",
		RunE: func(cmd *cobra.Command, _ []string) error {
			executable, err := os.Executable()
			if err != nil {
				return fmt.Errorf("locate current executable: %w", err)
			}
			executable, err = filepath.EvalSymlinks(executable)
			if err != nil {
				return fmt.Errorf("resolve current executable: %w", err)
			}
			client := &http.Client{
				Timeout: 2 * time.Minute,
				CheckRedirect: func(req *http.Request, via []*http.Request) error {
					if req.URL.Scheme != "https" {
						return errors.New("release download redirected away from HTTPS")
					}
					if len(via) >= 10 {
						return errors.New("too many release download redirects")
					}
					return nil
				},
			}
			return updateRelease(cmd.Context(), client, latestReleaseURL, executable, Version, cmd.OutOrStdout())
		},
	}
}

func updateRelease(ctx context.Context, client *http.Client, apiURL, executable, currentVersion string, out io.Writer) error {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		return fmt.Errorf("self-update is not supported on %s; download a release from https://github.com/ananymishradev/sluice/releases", runtime.GOOS)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return fmt.Errorf("create latest-release request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "sluice-updater")
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("fetch latest Sluice release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("fetch latest Sluice release: GitHub returned %s", resp.Status)
	}
	var release githubRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&release); err != nil {
		return fmt.Errorf("decode latest Sluice release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return errors.New("latest Sluice release has no tag")
	}
	if cmp, ok := compareReleaseVersions(currentVersion, release.TagName); ok {
		if cmp == 0 {
			fmt.Fprintf(out, "Sluice %s is already up to date.\n", currentVersion)
			return nil
		}
		if cmp > 0 {
			fmt.Fprintf(out, "Sluice %s is newer than the latest published release %s; no update installed.\n", currentVersion, release.TagName)
			return nil
		}
	}

	binaryName := fmt.Sprintf("%s-%s-%s", brand.Name, runtime.GOOS, runtime.GOARCH)
	binary, checksum := findReleaseAssets(release.Assets, binaryName)
	if binary.Name == "" || checksum.Name == "" {
		return fmt.Errorf("release %s does not provide a binary and SHA-256 checksum for %s/%s", release.TagName, runtime.GOOS, runtime.GOARCH)
	}

	wantHash, err := downloadChecksum(ctx, client, checksum.BrowserDownloadURL)
	if err != nil {
		return err
	}
	data, err := downloadBinary(ctx, client, binary.BrowserDownloadURL)
	if err != nil {
		return err
	}
	gotHash := sha256.Sum256(data)
	if !strings.EqualFold(wantHash, hex.EncodeToString(gotHash[:])) {
		return fmt.Errorf("SHA-256 verification failed for release %s; executable was not changed", release.TagName)
	}
	if err := replaceExecutable(executable, data); err != nil {
		return fmt.Errorf("install Sluice %s: %w", release.TagName, err)
	}
	fmt.Fprintf(out, "Updated Sluice %s to %s.\n", currentVersion, release.TagName)
	return nil
}

func findReleaseAssets(assets []releaseAsset, binaryName string) (releaseAsset, releaseAsset) {
	byName := make(map[string]releaseAsset, len(assets))
	for _, asset := range assets {
		byName[asset.Name] = asset
	}
	binary, ok := byName[binaryName]
	if !ok {
		return releaseAsset{}, releaseAsset{}
	}
	checksum, ok := byName[binaryName+".sha256"]
	if !ok {
		return releaseAsset{}, releaseAsset{}
	}
	return binary, checksum
}

func downloadChecksum(ctx context.Context, client *http.Client, assetURL string) (string, error) {
	data, err := downloadAsset(ctx, client, assetURL, 4096)
	if err != nil {
		return "", fmt.Errorf("download release checksum: %w", err)
	}
	fields := strings.Fields(string(data))
	if len(fields) == 0 || len(fields[0]) != sha256.Size*2 {
		return "", errors.New("release checksum is missing or malformed")
	}
	if _, err := hex.DecodeString(fields[0]); err != nil {
		return "", errors.New("release checksum is not valid hexadecimal")
	}
	return fields[0], nil
}

func downloadBinary(ctx context.Context, client *http.Client, assetURL string) ([]byte, error) {
	data, err := downloadAsset(ctx, client, assetURL, maxReleaseSize)
	if err != nil {
		return nil, fmt.Errorf("download release binary: %w", err)
	}
	if len(data) == 0 {
		return nil, errors.New("release binary is empty")
	}
	return data, nil
}

func downloadAsset(ctx context.Context, client *http.Client, assetURL string, maxSize int64) ([]byte, error) {
	parsed, err := url.Parse(assetURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return nil, fmt.Errorf("release asset URL must use HTTPS")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create asset request: %w", err)
	}
	req.Header.Set("User-Agent", "sluice-updater")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request asset: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("request asset: GitHub returned %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxSize+1))
	if err != nil {
		return nil, fmt.Errorf("read asset: %w", err)
	}
	if int64(len(data)) > maxSize {
		return nil, fmt.Errorf("release asset exceeds %d bytes", maxSize)
	}
	return data, nil
}

func replaceExecutable(path string, data []byte) (err error) {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat current executable: %w", err)
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		mode = 0o755
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".sluice-update-*")
	if err != nil {
		return fmt.Errorf("create temporary executable beside %s: %w", path, err)
	}
	tmpPath := tmp.Name()
	defer func() {
		_ = os.Remove(tmpPath)
	}()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary executable: %w", err)
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("set executable permissions: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary executable: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary executable: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace executable (check write permissions): %w", err)
	}
	return nil
}

func compareReleaseVersions(current, latest string) (int, bool) {
	parse := func(value string) ([3]int, bool) {
		var version [3]int
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		if i := strings.IndexAny(value, "-+"); i >= 0 {
			value = value[:i]
		}
		parts := strings.Split(value, ".")
		if len(parts) != len(version) {
			return version, false
		}
		for i, part := range parts {
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 {
				return version, false
			}
			version[i] = n
		}
		return version, true
	}
	a, okA := parse(current)
	b, okB := parse(latest)
	if !okA || !okB {
		return 0, false
	}
	for i := range a {
		switch {
		case a[i] < b[i]:
			return -1, true
		case a[i] > b[i]:
			return 1, true
		}
	}
	return 0, true
}
