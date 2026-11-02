package stage

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Result struct {
	Hash, Path string
	Cached     bool
}

func New(cacheDir string) (*Stager, error) {
	if cacheDir == "" {
		return nil, fmt.Errorf("cache directory is required")
	}
	if err := os.MkdirAll(cacheDir, 0o750); err != nil {
		return nil, err
	}
	return &Stager{cacheDir: cacheDir}, nil
}

type Stager struct{ cacheDir string }

func (s *Stager) Materialize(cachedPath, runDir string) error {
	if err := os.MkdirAll(runDir, 0o750); err != nil {
		return err
	}
	return copyTree(cachedPath, runDir)
}

func (s *Stager) Stage(caseDir string, include ...string) (Result, error) {
	clean, err := checkIncludes(caseDir, include)
	if err != nil {
		return Result{}, err
	}
	hash, err := hashTree(caseDir, clean)
	if err != nil {
		return Result{}, err
	}
	dest := filepath.Join(s.cacheDir, hash)
	if _, err := os.Stat(dest); err == nil {
		return Result{Hash: hash, Path: dest, Cached: true}, nil
	}
	tmp, err := os.MkdirTemp(s.cacheDir, ".stage-*")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(tmp)
	if err := copyCaseTree(caseDir, tmp, clean); err != nil {
		return Result{}, err
	}
	if err := os.Rename(tmp, dest); err != nil {
		if _, statErr := os.Stat(dest); statErr == nil {
			return Result{Hash: hash, Path: dest, Cached: true}, nil
		}
		return Result{}, err
	}
	return Result{Hash: hash, Path: dest}, nil
}

// caseTopDirs are the canonical OpenFOAM inputs that define a case.
// "0", "0.orig", and "initial" are all accepted as the initial-fields
// directory: many tutorials ship 0.orig and create 0 at run time, and
// laserbeamFoam-based cases ship "initial" that their Allrun stages to 0
// before the first solve. Everything else at the case root (README,
// manifests, *.sh, *.slurm, logs, .foam-cache, .foam-runs, processor*,
// .git) is documentation, artefacts, or runner state and must not affect
// the content hash or the cached copy unless explicitly listed in
// case.include.
var caseTopDirs = map[string]bool{"0": true, "0.orig": true, "initial": true, "constant": true, "system": true}

func topOf(rel string) string {
	if i := strings.Index(rel, string(os.PathSeparator)); i >= 0 {
		return rel[:i]
	}
	return rel
}

func includeRel(rel string, extra map[string]bool) bool {
	if rel == "." {
		return false
	}
	top := topOf(rel)
	if caseTopDirs[top] {
		return true
	}
	if extra != nil && extra[top] {
		return true
	}
	return false
}

// checkIncludes validates explicit case.include entries and returns the set
// of top-level names to stage/hash in addition to the canonical dirs.
func checkIncludes(caseDir string, includes []string) (map[string]bool, error) {
	extra := map[string]bool{}
	for _, inc := range includes {
		if strings.TrimSpace(inc) == "" || strings.ContainsAny(inc, "\r\n") {
			return nil, fmt.Errorf("case.include entries must be non-empty single lines")
		}
		if filepath.IsAbs(inc) {
			return nil, fmt.Errorf("case.include %q must be case-relative", inc)
		}
		clean := filepath.Clean(inc)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
			return nil, fmt.Errorf("case.include %q must stay inside the case", inc)
		}
		top := topOf(clean)
		if caseTopDirs[top] {
			return nil, fmt.Errorf("case.include %q duplicates canonical input %q", inc, top)
		}
		// Never let explicit includes pull runner artefacts into the cache.
		if top == ".git" || top == ".foam-cache" || top == ".foam-runs" ||
			strings.HasPrefix(top, "processor") {
			return nil, fmt.Errorf("case.include %q must not reference runner artefacts", inc)
		}
		base := filepath.Base(clean)
		if strings.HasPrefix(base, "slurm-") || strings.HasPrefix(base, "log.") ||
			strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-shm") {
			return nil, fmt.Errorf("case.include %q must not reference logs or databases", inc)
		}
		full := filepath.Join(caseDir, clean)
		if _, err := os.Lstat(full); err != nil {
			return nil, fmt.Errorf("case.include %q: %w", inc, err)
		}
		extra[top] = true
	}
	return extra, nil
}

func hashTree(root string, extra map[string]bool) (string, error) {
	type entry struct {
		rel      string
		isLink   bool
		linkDest string
		full     string
	}
	var entries []entry
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		if !includeRel(rel, extra) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.IsDir() {
			return nil
		}
		e := entry{rel: rel, full: path}
		if info.Mode()&os.ModeSymlink != 0 {
			e.isLink = true
			dest, err := os.Readlink(path)
			if err != nil {
				return err
			}
			e.linkDest = dest
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return "", err
	}
	// Deterministic hash: sort by relative path, then mix names, link
	// targets, and file contents.
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		io.WriteString(h, e.rel+"\x00")
		if e.isLink {
			io.WriteString(h, "symlink:"+e.linkDest+"\x00")
			continue
		}
		f, err := os.Open(e.full)
		if err != nil {
			return "", err
		}
		if _, err := io.Copy(h, f); err != nil {
			f.Close()
			return "", err
		}
		f.Close()
		io.WriteString(h, "\x00")
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// copyCaseTree copies the canonical inputs (0/, 0.orig/, initial/,
// constant/, system/) plus any explicit case.include tops from src to dst.
func copyCaseTree(src, dst string, extra map[string]bool) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		if rel == "." {
			return os.MkdirAll(dst, 0o750)
		}
		if !includeRel(rel, extra) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(rel), link)), "..") {
				return fmt.Errorf("unsafe symlink %s", rel)
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}

func copyTree(src, dst string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, info.Mode().Perm())
		}
		if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || strings.HasPrefix(filepath.Clean(filepath.Join(filepath.Dir(rel), link)), "..") {
				return fmt.Errorf("unsafe symlink %s", rel)
			}
			return os.Symlink(link, target)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
		if err != nil {
			return err
		}
		_, cpErr := io.Copy(out, in)
		closeErr := out.Close()
		if cpErr != nil {
			return cpErr
		}
		return closeErr
	})
}
