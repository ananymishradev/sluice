package manifest

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ananymishradev/sluice/internal/brand"
	"gopkg.in/yaml.v3"
)

// DefaultFoamBashrc is the last-resort fallback OpenFOAM environment.
// Resolution order for runtime.foamBashrc is:
//  1. explicit runtime.foamBashrc in the manifest,
//  2. FOAM_BASHRC in the environment,
//  3. auto-detection of /opt/openfoam*/etc/bashrc (highest version first),
//  4. DefaultFoamBashrc.
//
// No solver, mesh tool, MPI launcher, or file-handler name is hard-coded:
// every execution choice comes from the manifest so heavySimple,
// heavyBuoyant, and any future OpenFOAM project run through the same path.
const DefaultFoamBashrc = "/opt/openfoam12/etc/bashrc"

type Manifest struct {
	APIVersion string    `yaml:"apiVersion"`
	Name       string    `yaml:"name"`
	Case       Case      `yaml:"case"`
	Resources  Resources `yaml:"resources"`
	Solver     Solver    `yaml:"solver"`
	Mesh       Mesh      `yaml:"mesh"`
	Post       Post      `yaml:"post"`
	Runtime    Runtime   `yaml:"runtime"`
	Storage    Storage   `yaml:"storage"`
	// Build is optional. When present, the generated job script keeps a
	// guarded build stage first: if the solver binary is not on PATH inside
	// the OpenFOAM environment, wmake compiles it from Build.Source before
	// meshing/solving. This lets an unbuilt ("naked") project be submitted
	// and everything — build included — happen on the compute node.
	Build *Build `yaml:"build"`
}

// Build describes the compile-if-necessary bootstrap for projects whose
// solver is not part of the OpenFOAM distribution. The environment comes
// from runtime.foamBashrc; the computed artifacts live in the source tree,
// which must be visible on the compute nodes (it is referenced in place,
// not staged).
// Exactly one of source or commands is required:
//   - source: a wmake project root (Make/files, Make/options); wmake runs
//     there once.
//   - commands: arbitrary shell lines executed serially under foam_serial
//     (e.g. ./Allwmake for multi-project trees, or a script that fetches
//     submodules first). Line no. 1 logs into log.build like the rest.
type Build struct {
	// Source is the wmake project root containing Make/files and
	// Make/options (e.g. /opt/src/MySolver). wmake runs there.
	Source string `yaml:"source"`
	// Commands are serial bootstrap shell lines, run instead of wmake.
	Commands []string `yaml:"commands"`
}

type Case struct {
	Path string `yaml:"path"`
	// Include lists extra top-level case paths to stage alongside the
	// canonical 0/, 0.orig/, initial/, constant/, system/ inputs (e.g.
	// ["Allrun"]).
	Include []string `yaml:"include"`
}

type Resources struct {
	// Partition is optional; when empty the cluster default applies.
	Partition        string `yaml:"partition"`
	Account          string `yaml:"account"`
	Nodes            int    `yaml:"nodes"`
	TasksPerNode     int    `yaml:"tasksPerNode"`
	TimeLimitMinutes int    `yaml:"timeLimitMinutes"`
	// SbatchExtra carries raw SBATCH arguments (e.g. ["--gres=gpu:1"]).
	// They are appended to the generated script on the CLI backend and
	// best-effort mapped into the REST job object.
	SbatchExtra []string `yaml:"sbatchExtra"`
}

type Solver struct {
	Name      string   `yaml:"name"`
	Arguments []string `yaml:"arguments"`
	Container string   `yaml:"container"`
	// FileHandler is collated, uncollated, or none (omit the flag).
	FileHandler string `yaml:"fileHandler"`
	// CheckpointSeconds arms the USR1 writeNow trap N seconds before the
	// walltime. Zero disables checkpoint/requeue.
	CheckpointSeconds int `yaml:"checkpointSeconds"`
}

type Mesh struct {
	// Strategy is auto, blockMesh, custom, or none.
	// auto uses constant/polyMesh when present, else blockMesh when
	// system/blockMeshDict exists, else fails with guidance.
	Strategy string `yaml:"strategy"`
	// Commands run serially before decomposePar under the custom strategy
	// (e.g. ["surfaceFeatureExtract", "snappyHexMesh -overwrite"]).
	// They are skipped when constant/polyMesh already exists so requeued
	// jobs do not remesh.
	Commands []string `yaml:"commands"`
}

type Post struct {
	// Reconstruct runs reconstructPar after a successful solve.
	Reconstruct bool `yaml:"reconstruct"`
	// Commands run after the solve (and after reconstruct) when the solver
	// exited 0 without a checkpoint request.
	Commands []string `yaml:"commands"`
}

type MPI struct {
	// Launcher is mpirun or srun.
	Launcher string `yaml:"launcher"`
	// Flavour is the srun --mpi value (pmix, pmi2, openmpi). mpirun ignores it.
	Flavour   string   `yaml:"flavour"`
	ExtraArgs []string `yaml:"extraArgs"`
}

type Runtime struct {
	FoamBashrc     string `yaml:"foamBashrc"`
	ThreadsPerRank int    `yaml:"threadsPerRank"`
	MPI            MPI    `yaml:"mpi"`
}

type Storage struct {
	CacheDir string `yaml:"cacheDir"`
	RunDir   string `yaml:"runDir"`
}

func Load(path string) (Manifest, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read manifest: %w", err)
	}
	var m Manifest
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		return Manifest{}, fmt.Errorf("decode manifest: %w", err)
	}
	// The base directory must be absolute: a relative manifest path would
	// otherwise resolve storage defaults against the caller's cwd, and
	// sbatch opens generated scripts relative to its own working directory.
	base, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return Manifest{}, fmt.Errorf("resolve manifest directory: %w", err)
	}
	if !filepath.IsAbs(m.Case.Path) {
		m.Case.Path = filepath.Join(base, m.Case.Path)
	}
	if m.Storage.CacheDir != "" && !filepath.IsAbs(m.Storage.CacheDir) {
		m.Storage.CacheDir = filepath.Join(base, m.Storage.CacheDir)
	}
	if m.Storage.RunDir != "" && !filepath.IsAbs(m.Storage.RunDir) {
		m.Storage.RunDir = filepath.Join(base, m.Storage.RunDir)
	}
	// Defaults resolve against the manifest directory too: relative
	// storage would break scheduler submission once cwd changes, and
	// sbatch resolves script paths against its own working directory.
	if m.Storage.CacheDir == "" {
		m.Storage.CacheDir = filepath.Join(base, ".foam-cache")
	}
	if m.Storage.RunDir == "" {
		m.Storage.RunDir = filepath.Join(base, ".foam-runs")
	}
	if m.Build != nil && m.Build.Source != "" && !filepath.IsAbs(m.Build.Source) {
		m.Build.Source = filepath.Join(base, m.Build.Source)
	}
	m.applyDefaults()
	if err := m.Validate(); err != nil {
		return Manifest{}, err
	}
	if m.APIVersion == brand.LegacyAPIVersion {
		fmt.Fprintln(os.Stderr, "warning: apiVersion "+brand.LegacyAPIVersion+" is deprecated; use "+brand.APIVersion+" instead")
	}
	return m, nil
}

func (m *Manifest) applyDefaults() {
	if m.Solver.FileHandler == "" {
		m.Solver.FileHandler = "collated"
	}
	if m.Mesh.Strategy == "" {
		m.Mesh.Strategy = "auto"
	}
	if m.Runtime.MPI.Launcher == "" {
		m.Runtime.MPI.Launcher = "mpirun"
	}
	if m.Runtime.ThreadsPerRank <= 0 {
		m.Runtime.ThreadsPerRank = 1
	}
	if m.Runtime.FoamBashrc == "" {
		if v := strings.TrimSpace(os.Getenv("FOAM_BASHRC")); v != "" {
			m.Runtime.FoamBashrc = v
		} else if auto := detectFoamBashrc(); auto != "" {
			m.Runtime.FoamBashrc = auto
		} else {
			m.Runtime.FoamBashrc = DefaultFoamBashrc
		}
	}
}

// detectFoamBashrc probes well-known OpenFOAM install locations so clusters
// that do not use /opt/openfoam12 work without extra configuration.
// It returns "" when nothing is found; callers fall back to DefaultFoamBashrc
// and surface a clear error at submit time if the file is still missing.
func detectFoamBashrc() string {
	if _, err := os.Stat(DefaultFoamBashrc); err == nil {
		return DefaultFoamBashrc
	}
	matches, _ := filepath.Glob("/opt/openfoam*/etc/bashrc")
	// Prefer the highest version path for determinism.
	sortStringsDesc(matches)
	for _, p := range matches {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	matches, _ = filepath.Glob("/usr/lib/openfoam*/etc/bashrc")
	sortStringsDesc(matches)
	for _, p := range matches {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

func sortStringsDesc(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] > s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// Ranks is the total MPI rank count derived from resources, never hard-coded
// per case.
func (m Manifest) Ranks() int { return m.Resources.Nodes * m.Resources.TasksPerNode }

// WallTime renders timeLimitMinutes as Slurm HH:MM:SS.
func (m Manifest) WallTime() string {
	min := m.Resources.TimeLimitMinutes
	if min <= 0 {
		min = 60
	}
	return fmt.Sprintf("%02d:%02d:00", min/60, min%60)
}

func (m Manifest) Validate() error {
	var problems []string
	if m.APIVersion == "" {
		problems = append(problems, "apiVersion is required")
	}
	if m.APIVersion != "" && m.APIVersion != brand.APIVersion && m.APIVersion != brand.LegacyAPIVersion {
		problems = append(problems, "apiVersion must be "+brand.APIVersion+" or "+brand.LegacyAPIVersion)
	}
	if m.Name == "" {
		problems = append(problems, "name is required")
	}
	if strings.ContainsAny(m.Name, "\r\n") {
		problems = append(problems, "name must not contain newlines")
	}
	if filepath.IsAbs(m.Case.Path) == false || m.Case.Path == "." {
		problems = append(problems, "case.path must resolve to an absolute directory")
	}
	for _, inc := range m.Case.Include {
		if strings.TrimSpace(inc) == "" || strings.ContainsAny(inc, "\r\n") {
			problems = append(problems, "case.include entries must be non-empty single lines")
			break
		}
		if filepath.IsAbs(inc) || inc == "." || inc == ".." || strings.HasPrefix(filepath.Clean(inc), "..") {
			problems = append(problems, "case.include entries must be case-relative paths")
			break
		}
	}
	if strings.ContainsAny(m.Resources.Partition, "\r\n") {
		problems = append(problems, "resources.partition must not contain newlines")
	}
	if strings.ContainsAny(m.Resources.Account, "\r\n") {
		problems = append(problems, "resources.account must not contain newlines")
	}
	if m.Resources.Nodes < 1 {
		problems = append(problems, "resources.nodes must be greater than zero")
	}
	if m.Resources.TasksPerNode < 1 {
		problems = append(problems, "resources.tasksPerNode must be greater than zero")
	}
	if m.Resources.TimeLimitMinutes < 1 {
		problems = append(problems, "resources.timeLimitMinutes must be greater than zero")
	}
	for _, e := range m.Resources.SbatchExtra {
		if err := validSbatchExtra(e); err != nil {
			problems = append(problems, err.Error())
			break
		}
	}
	if m.Solver.Name == "" {
		problems = append(problems, "solver.name is required")
	} else if !validExecutable(m.Solver.Name) {
		problems = append(problems, "solver.name must be a bare executable name (letters, digits, ., _, +, -)")
	}
	if strings.TrimSpace(m.Solver.Container) != "" {
		if strings.ContainsAny(m.Solver.Container, "\r\n") || !filepath.IsAbs(m.Solver.Container) {
			problems = append(problems, "solver.container must be an absolute image path")
		}
	}
	for _, a := range m.Solver.Arguments {
		if strings.ContainsAny(a, "\r\n") {
			problems = append(problems, "solver.arguments must not contain newlines")
			break
		}
	}
	switch m.Solver.FileHandler {
	case "collated", "uncollated", "none":
	default:
		problems = append(problems, "solver.fileHandler must be collated, uncollated, or none")
	}
	if m.Solver.CheckpointSeconds < 0 {
		problems = append(problems, "solver.checkpointSeconds cannot be negative")
	}
	if m.Solver.CheckpointSeconds > 0 && m.Resources.TimeLimitMinutes > 0 &&
		m.Solver.CheckpointSeconds >= m.Resources.TimeLimitMinutes*60 {
		problems = append(problems, "solver.checkpointSeconds must be less than the walltime")
	}
	switch m.Mesh.Strategy {
	case "auto", "blockMesh", "custom", "none":
	default:
		problems = append(problems, "mesh.strategy must be auto, blockMesh, custom, or none")
	}
	if m.Mesh.Strategy == "custom" && len(m.Mesh.Commands) == 0 {
		problems = append(problems, "mesh.commands is required with mesh.strategy custom")
	}
	for _, c := range m.Mesh.Commands {
		if strings.TrimSpace(c) == "" || strings.ContainsAny(c, "\r\n") {
			problems = append(problems, "mesh.commands must be non-empty single lines")
			break
		}
	}
	for _, c := range m.Post.Commands {
		if strings.TrimSpace(c) == "" || strings.ContainsAny(c, "\r\n") {
			problems = append(problems, "post.commands must be non-empty single lines")
			break
		}
	}
	if strings.ContainsAny(m.Runtime.FoamBashrc, "\r\n") || !filepath.IsAbs(m.Runtime.FoamBashrc) {
		problems = append(problems, "runtime.foamBashrc must be an absolute path")
	}
	if m.Build != nil {
		hasSrc := strings.TrimSpace(m.Build.Source) != ""
		hasCmds := len(m.Build.Commands) > 0
		switch {
		case hasSrc && hasCmds:
			problems = append(problems, "build.source and build.commands are mutually exclusive: source wmake-runs a project, commands run custom bootstrap lines")
		case !hasSrc && !hasCmds:
			problems = append(problems, "build requires build.source (wmake project path) or build.commands (bootstrap shell lines)")
		case hasSrc && (strings.ContainsAny(m.Build.Source, "\r\n") || !filepath.IsAbs(m.Build.Source)):
			problems = append(problems, "build.source must be an absolute path")
		case hasSrc:
			if st, err := os.Stat(m.Build.Source); err != nil || !st.IsDir() {
				problems = append(problems, "build.source must exist and be a directory")
			} else if _, err := os.Stat(filepath.Join(m.Build.Source, "Make", "files")); err != nil {
				problems = append(problems, "build.source must be a wmake project (Make/files not found) — otherwise use build.commands")
			}
		}
		for _, c := range m.Build.Commands {
			if strings.TrimSpace(c) == "" || strings.ContainsAny(c, "\r\n") {
				problems = append(problems, "build.commands must be non-empty single lines")
				break
			}
		}
	}
	if m.Runtime.ThreadsPerRank < 1 {
		problems = append(problems, "runtime.threadsPerRank must be greater than zero")
	}
	switch m.Runtime.MPI.Launcher {
	case "mpirun", "srun":
	default:
		problems = append(problems, "runtime.mpi.launcher must be mpirun or srun")
	}
	switch m.Runtime.MPI.Flavour {
	case "", "pmix", "pmi2", "openmpi":
	default:
		problems = append(problems, "runtime.mpi.flavour must be pmix, pmi2, or openmpi")
	}
	for _, a := range m.Runtime.MPI.ExtraArgs {
		if strings.ContainsAny(a, "\r\n") {
			problems = append(problems, "runtime.mpi.extraArgs must not contain newlines")
			break
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}

// validExecutable accepts any bare program name so custom-compiled solvers
// work. The name is always shell-quoted in generated scripts; path
// separators, whitespace, and shell metacharacters are rejected here.
func validExecutable(name string) bool {
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if alnum {
			continue
		}
		if i > 0 && (c == '.' || c == '_' || c == '+' || c == '-') {
			continue
		}
		return false
	}
	return len(name) > 0
}

// reservedSbatch holds SBYPDJ directives the runner generates itself.
var reservedSbatch = map[string]bool{
	"job_name": true, "partition": true, "account": true, "nodes": true,
	"ntasks": true, "ntasks_per_node": true, "time": true, "output": true,
	"error": true, "signal": true, "exclusive": true, "hint": true,
	"requeue": true, "standard_output": true, "standard_error": true,
}

func validSbatchExtra(e string) error {
	if !strings.HasPrefix(e, "--") || strings.ContainsAny(e, "\r\n") {
		return fmt.Errorf("resources.sbatchExtra entries must look like --key or --key=value")
	}
	body := strings.TrimPrefix(e, "--")
	key := body
	if i := strings.Index(body, "="); i >= 0 {
		key = body[:i]
	}
	norm := strings.ReplaceAll(strings.ToLower(key), "-", "_")
	if norm == "" {
		return fmt.Errorf("resources.sbatchExtra entries must look like --key or --key=value")
	}
	for _, c := range norm {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_') {
			return fmt.Errorf("resources.sbatchExtra key %q has invalid characters", key)
		}
	}
	if reservedSbatch[norm] {
		return fmt.Errorf("resources.sbatchExtra must not repeat runner-managed option --%s", key)
	}
	return nil
}

// SbatchExtraVars maps --key=value entries onto REST job fields
// (best effort; the CLI backend uses the raw lines verbatim).
func SbatchExtraVars(entries []string) map[string]any {
	out := map[string]any{}
	for _, e := range entries {
		body := strings.TrimPrefix(e, "--")
		if i := strings.Index(body, "="); i >= 0 {
			out[strings.ReplaceAll(body[:i], "-", "_")] = body[i+1:]
		} else {
			out[strings.ReplaceAll(body, "-", "_")] = true
		}
	}
	return out
}
