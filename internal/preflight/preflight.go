package preflight

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type Finding struct{ Severity, Path, Message string }
type Report struct {
	Hash     string
	Findings []Finding
	Warnings []Finding
	// Notes are informational observations (never fatal). They make the
	// compile-if-necessary behavior visible at validate time — e.g. whether
	// the job's guarded build stage will wmake or skip.
	Notes []Finding
}

// Runner validates an OpenFOAM case directory before queue time is spent.
// It is generic over every solver and physics model: the required layout,
// the executable-directive blocklist, and the optional tool checks apply
// identically to heavySimple (steady incompressibleFluid), heavyBuoyant
// (transient buoyant fluid), and any future project.
//
// AllowedDirectives optionally exempts specific blocklist tokens for cases
// whose physics legitimately needs them (e.g. a vetted functionObject that
// loads a libs entry). Empty means the strict default: reject all five.
type Runner struct {
	FoamDictionary string
	CheckMesh      string
	// AllowedDirectives lists blocklist tokens that are accepted for this
	// case (exact match against the default token set).
	AllowedDirectives []string
}

// DefaultBlockedTokens are OpenFOAM dictionary mechanisms that load and run
// arbitrary code at run time. They are rejected by default in multi-user
// service because a case file would otherwise become code execution.
var DefaultBlockedTokens = []string{"#codeStream", "#calc", "codedFixedValue", "codedMixed", "libs"}

func (r Runner) Run(ctx context.Context, caseDir string) (Report, error) {
	info, err := os.Stat(caseDir)
	if err != nil {
		return Report{}, fmt.Errorf("case: %w", err)
	}
	if !info.IsDir() {
		return Report{}, fmt.Errorf("case %q is not a directory", caseDir)
	}
	// constant/ and system/ are always required. The initial-fields dir is
	// "0", "0.orig", or "initial" (tutorials commonly ship 0.orig and create
	// 0 at run time; laserbeamFoam-based cases ship "initial" and stage it
	// before the first solve). At least one must exist so serial and
	// parallel runs alike have boundary conditions to start from.
	for _, name := range []string{"system", "constant"} {
		if st, err := os.Stat(filepath.Join(caseDir, name)); err != nil || !st.IsDir() {
			return Report{}, fmt.Errorf("case missing directory %s", name)
		}
	}
	hasZero := false
	for _, name := range []string{"0", "0.orig", "initial"} {
		if st, err := os.Stat(filepath.Join(caseDir, name)); err == nil && st.IsDir() {
			hasZero = true
			break
		}
	}
	if !hasZero {
		return Report{}, fmt.Errorf("case missing directory 0 (or 0.orig, initial)")
	}
	allowed := map[string]bool{}
	for _, a := range r.AllowedDirectives {
		allowed[a] = true
	}
	tokens := blockedTokens(allowed)
	report := Report{}
	err = filepath.WalkDir(caseDir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, _ := filepath.Rel(caseDir, path)
		if rel == "." {
			return nil
		}
		top := rel
		if i := strings.Index(rel, string(os.PathSeparator)); i >= 0 {
			top = rel[:i]
		}
		// Runtime artefacts and VCS state are never part of the case input.
		// Skip them entirely (no symlink or directive checks inside).
		if d.IsDir() {
			if top == ".git" || top == ".foam-cache" || top == ".foam-runs" {
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), "processor") {
				return filepath.SkipDir
			}
			return nil
		}
		if top == ".foam-cache" || top == ".foam-runs" || top == ".git" {
			return nil
		}
		if strings.HasPrefix(rel, "processor") {
			return nil
		}
		// Slurm logs, solver logs, and DB files are artefacts, not input.
		base := d.Name()
		if strings.HasPrefix(base, "slurm-") || strings.HasPrefix(base, "log.") || base == "log.solver" || strings.HasSuffix(base, ".db") || strings.HasSuffix(base, ".db-wal") || strings.HasSuffix(base, ".db-shm") {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			target, err := filepath.EvalSymlinks(path)
			if err != nil {
				return err
			}
			if !within(caseDir, target) {
				report.Findings = append(report.Findings, Finding{"error", rel, "symlink escapes case directory"})
			}
		}
		// Executable-directive scan applies only to OpenFOAM input
		// dictionaries under 0/, 0.orig/, constant/, system/. Scanning README,
		// shell scripts, manifests, or Slurm files produces false
		// positives when docs mention directive names.
		if !isDictPath(rel) {
			return nil
		}
		if err := scanFile(path, rel, tokens, &report); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return Report{}, fmt.Errorf("scan case: %w", err)
	}
	if r.FoamDictionary != "" {
		runTool(ctx, r.FoamDictionary, caseDir, &report, "foamDictionary")
	}
	if r.CheckMesh != "" {
		runTool(ctx, r.CheckMesh, caseDir, &report, "checkMesh")
	}
	if len(report.Findings) > 0 {
		return report, fmt.Errorf("preflight failed with %d finding(s)", len(report.Findings))
	}
	return report, nil
}

func blockedTokens(allowed map[string]bool) []string {
	var out []string
	for _, t := range DefaultBlockedTokens {
		if !allowed[t] {
			out = append(out, t)
		}
	}
	return out
}

func scanFile(path, rel string, tokens []string, report *Report) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Buffer(make([]byte, 4096), 1<<20)
	for line := 1; s.Scan(); line++ {
		text := strings.TrimSpace(s.Text())
		for _, token := range tokens {
			if strings.Contains(text, token) {
				report.Findings = append(report.Findings, Finding{"error", fmt.Sprintf("%s:%d", rel, line), "executable OpenFOAM directive " + token + " is not allowed"})
			}
		}
	}
	return s.Err()
}

func runTool(ctx context.Context, tool, dir string, report *Report, name string) {
	cmd := exec.CommandContext(ctx, tool, "-case", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		report.Findings = append(report.Findings, Finding{"error", name, fmt.Sprintf("%v: %s", err, strings.TrimSpace(string(out)))})
	}
}

func within(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}

// isDictPath reports whether rel is an OpenFOAM input dictionary that must
// be scanned for executable directives. Only files under 0/, 0.orig/,
// initial/, constant/, system/ qualify. Everything else (README, scripts,
// manifests, logs) is documentation or artefacts and must not trigger
// findings.
func isDictPath(rel string) bool {
	sep := string(os.PathSeparator)
	return strings.HasPrefix(rel, "0"+sep) ||
		strings.HasPrefix(rel, "0.orig"+sep) ||
		strings.HasPrefix(rel, "initial"+sep) ||
		strings.HasPrefix(rel, "constant"+sep) ||
		strings.HasPrefix(rel, "system"+sep) ||
		rel == "0" || rel == "0.orig" || rel == "initial" || rel == "constant" || rel == "system"
}

// Subdomains reads system/decomposeParDict and returns numberOfSubdomains.
// It returns an error when the file is missing or unparsable so callers can
// fail fast before queue time is spent.
func Subdomains(caseDir string) (int, error) {
	raw, err := os.ReadFile(filepath.Join(caseDir, "system", "decomposeParDict"))
	if err != nil {
		return 0, fmt.Errorf("read decomposeParDict: %w", err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		t := strings.TrimSpace(strings.SplitN(line, "//", 2)[0])
		if !strings.HasPrefix(t, "numberOfSubdomains") {
			continue
		}
		fields := strings.Fields(t)
		if len(fields) < 2 {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(strings.TrimSuffix(fields[1], ";"), "%d", &n); err == nil && n > 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("numberOfSubdomains not found in system/decomposeParDict")
}
