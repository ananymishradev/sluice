package service

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	slurmrest "github.com/ananymishradev/sluice"
	"github.com/ananymishradev/sluice/internal/brand"
	"github.com/ananymishradev/sluice/internal/manifest"
	"github.com/ananymishradev/sluice/internal/preflight"
	"github.com/ananymishradev/sluice/internal/stage"
	"github.com/ananymishradev/sluice/internal/store"
	"github.com/ananymishradev/sluice/internal/telemetry"
	"github.com/google/uuid"
)

type Service struct {
	Scheduler slurmrest.Scheduler
	Store     *store.Store
	Preflight preflight.Runner
	Metrics   *telemetry.Metrics
	Observer  Observer
	// ProbeSolver reports whether the manifest solver binary is on PATH
	// inside the OpenFOAM environment (runtime.foamBashrc). Nil selects the
	// real probe; tests inject fakes to stay machine-independent.
	ProbeSolver func(ctx context.Context, bashrc, name string) (bool, error)
}

// execProbeSolver is the production probe: it sources the bashrc and asks
// for the binary exactly the way the job will. The bashrc's exit status is
// deliberately ignored (ESI bashrcs can return nonzero after a successful
// source); the presence of the binary is the only signal.
//
// The bashrc and solver name travel via environment variables instead of
// positional arguments: OpenFOAM's etc/config.sh/setup treats unnamed
// positional arguments as VAR=value settings or as files to source, so a
// positional bashrc path makes the bashrc source itself recursively.
func execProbeSolver(ctx context.Context, bashrc, name string) (bool, error) {
	if _, err := os.Stat(bashrc); err != nil {
		return false, err
	}
	cmd := exec.CommandContext(ctx, "bash", "-c",
		`set +e; source "$FOAM_PROBE_BASHRC" >/dev/null 2>&1; command -v "$FOAM_PROBE_SOLVER"`)
	cmd.Env = append(os.Environ(),
		"FOAM_PROBE_BASHRC="+bashrc,
		"FOAM_PROBE_SOLVER="+name)
	if err := cmd.Run(); err != nil {
		return false, nil // environment sourced; binary simply absent
	}
	return true, nil
}

// noteBuildStage turns the compile-if-necessary decision into report notes:
// informational, never fatal. On the compute node the generated script
// re-checks with the real environment — this note is advisory.
func (s *Service) noteBuildStage(ctx context.Context, m manifest.Manifest, report *preflight.Report) {
	probe := s.ProbeSolver
	if probe == nil {
		probe = execProbeSolver
	}
	found, envErr := probe(ctx, m.Runtime.FoamBashrc, m.Solver.Name)
	note := func(path, format string, args ...any) {
		report.Notes = append(report.Notes, preflight.Finding{
			Severity: "note",
			Path:     path,
			Message:  fmt.Sprintf(format, args...),
		})
	}
	switch {
	case envErr != nil:
		note("runtime.foamBashrc", "cannot probe solver %q locally (%v); the build decision happens on the compute node", m.Solver.Name, envErr)
	case m.Build != nil && found:
		note("build", "build stage will be skipped: %q is already on PATH inside %s", m.Solver.Name, m.Runtime.FoamBashrc)
	case m.Build != nil:
		what := "wmake " + m.Build.Source
		if len(m.Build.Commands) > 0 {
			what = fmt.Sprintf("run %d build.commands", len(m.Build.Commands))
		}
		note("build", "build stage will %s before running: %q is not on PATH locally", what, m.Solver.Name)
	case !found:
		note("solver", "%q is not on PATH locally and the manifest has no build: section — job may fail at solver launch", m.Solver.Name)
	}
}

type Result struct {
	ID        string
	CaseHash  string
	CachePath string
	RunPath   string
	SlurmID   int
}

type Plan struct {
	Manifest manifest.Manifest
	Staged   stage.Result
	Script   string
}

// Plan validates and stages a case, then renders the batch script without
// creating a database record, run directory, or scheduler submission.
func (s *Service) Plan(ctx context.Context, manifestPath string) (Plan, error) {
	s.observe("manifest", StageStarted, manifestPath)
	m, _, err := s.Validate(ctx, manifestPath)
	if err != nil {
		s.observe("manifest", StageFailed, err.Error())
		return Plan{}, err
	}
	s.observe("manifest", StageDone, m.Name)
	cacheDir := m.Storage.CacheDir
	if cacheDir == "" {
		// Manifest loading already resolves defaults to absolute paths;
		// this keeps a plan built straight from a manifest struct robust
		// to a relative manifest path and a different cwd.
		baseDir, _ := filepath.Abs(filepath.Dir(manifestPath))
		cacheDir = filepath.Join(baseDir, ".foam-cache")
	}
	stager, err := stage.New(cacheDir)
	if err != nil {
		return Plan{}, err
	}
	staged, err := stager.Stage(m.Case.Path, m.Case.Include...)
	if err != nil {
		s.observe("cache", StageFailed, err.Error())
		return Plan{}, fmt.Errorf("stage case: %w", err)
	}
	s.observe("cache", StageDone, staged.Path)
	script, err := checkpointScript(m, staged.Path)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Manifest: m, Staged: staged, Script: script}, nil
}

func (s *Service) Validate(ctx context.Context, manifestPath string) (manifest.Manifest, preflight.Report, error) {
	m, err := manifest.Load(manifestPath)
	if err != nil {
		return manifest.Manifest{}, preflight.Report{}, err
	}
	report, err := s.Preflight.Run(ctx, m.Case.Path)
	if err != nil {
		if s.Metrics != nil {
			s.Metrics.PreflightFailures.Add(1)
		}
		return m, report, err
	}
	for _, check := range []func(manifest.Manifest) error{
		checkDecomposition,
		checkCheckpoint,
		checkMeshSource,
	} {
		if err := check(m); err != nil {
			if s.Metrics != nil {
				s.Metrics.PreflightFailures.Add(1)
			}
			return m, report, err
		}
	}
	warning, err := checkControlDict(m)
	if err != nil {
		if s.Metrics != nil {
			s.Metrics.PreflightFailures.Add(1)
		}
		return m, report, err
	}
	// Fold a solver/application drift into the report as a warning so
	// `sluice validate` surfaces it, without failing the run.
	if warning != "" {
		report.Warnings = append(report.Warnings, preflight.Finding{
			Severity: "warning",
			Path:     "system/controlDict",
			Message:  warning,
		})
	}
	// Make the compile-if-necessary decision visible at validate time.
	s.noteBuildStage(ctx, m, &report)
	return m, report, nil
}

// checkDecomposition fails fast when system/decomposeParDict disagrees with
// the requested rank count. Serial runs (ranks==1) and mesh.strategy==none
// never decompose, so the check is skipped there. A mismatch would otherwise
// waste queue time and abort inside the job.
func checkDecomposition(m manifest.Manifest) error {
	if m.Ranks() <= 1 || m.Mesh.Strategy == "none" {
		return nil
	}
	got, err := preflight.Subdomains(m.Case.Path)
	if err != nil {
		return err
	}
	if got != m.Ranks() {
		return fmt.Errorf("decomposeParDict numberOfSubdomains=%d does not match resources %d nodes x %d tasks = %d ranks",
			got, m.Resources.Nodes, m.Resources.TasksPerNode, m.Ranks())
	}
	return nil
}

var (
	applicationRe = regexp.MustCompile(`(?m)^\s*application\s+(\S+?)\s*;`)
	solverRe      = regexp.MustCompile(`(?m)^\s*solver\s+(\S+?)\s*;`)
	runTimeModRe  = regexp.MustCompile(`(?m)^\s*runTimeModifiable\s+true\s*;`)
)

func stripDictComments(s string) string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// checkControlDict ensures the case actually runs the solver named in the
// manifest. It is generic over every OpenFOAM project:
//   - application foamRun (OpenFOAM 12+ runner): the manifest solver must be
//     foamRun and the case must declare a physics model via "solver <model>;".
//     The model name is never hard-coded: incompressibleFluid (heavySimple),
//     fluid (heavyBuoyant), or any future model all pass.
//   - native solver (simpleFoam, buoyantPimpleFoam, customMySolver, ...):
//     the manifest solver should equal controlDict's application entry; a
//     drift (like laserbeamFoam's Allrun running "laserbeamFoam" while its
//     controlDict declares "Flint_multiphaseEulerFoamD") is reported as a
//     warning, not an error, because the manifest solver is authoritative
//     for submission and controlDict's application is advisory metadata.
//
// This catches copy-paste manifests without an allow-list. It returns an
// optional drift warning together with a fatal error.
func checkControlDict(m manifest.Manifest) (warning string, err error) {
	raw, err := os.ReadFile(filepath.Join(m.Case.Path, "system", "controlDict"))
	if err != nil {
		return "", fmt.Errorf("read system/controlDict: %w", err)
	}
	text := stripDictComments(string(raw))
	app := ""
	if mm := applicationRe.FindStringSubmatch(text); mm != nil {
		app = mm[1]
	}
	if app == "" {
		return "", fmt.Errorf("system/controlDict does not declare application <solver>;")
	}
	if app == "foamRun" {
		if m.Solver.Name != "foamRun" {
			return "", fmt.Errorf("system/controlDict declares application foamRun but manifest solver is %q", m.Solver.Name)
		}
		if mm := solverRe.FindStringSubmatch(text); mm == nil {
			return "", fmt.Errorf("system/controlDict declares application foamRun but no solver <model>; entry")
		}
		return "", nil
	}
	if m.Solver.Name != app {
		return fmt.Sprintf("application drift: controlDict declares %q but manifest solver %q is authoritative",
			app, m.Solver.Name), nil
	}
	return "", nil
}

// checkCheckpoint requires runTimeModifiable when the USR1 writeNow trap is
// armed. Without it the checkpoint silently does nothing and the job is
// killed at walltime with no usable restart.
func checkCheckpoint(m manifest.Manifest) error {
	if m.Solver.CheckpointSeconds <= 0 {
		return nil
	}
	raw, err := os.ReadFile(filepath.Join(m.Case.Path, "system", "controlDict"))
	if err != nil {
		return fmt.Errorf("read system/controlDict: %w", err)
	}
	if !runTimeModRe.MatchString(stripDictComments(string(raw))) {
		return fmt.Errorf("solver.checkpointSeconds=%d requires runTimeModifiable true in system/controlDict (or set checkpointSeconds: 0)",
			m.Solver.CheckpointSeconds)
	}
	return nil
}

// checkMeshSource validates that mesh.strategy can actually produce a mesh
// from the staged inputs.
func checkMeshSource(m manifest.Manifest) error {
	switch m.Mesh.Strategy {
	case "none", "custom":
		return nil
	case "blockMesh":
		if _, err := os.Stat(filepath.Join(m.Case.Path, "system", "blockMeshDict")); err != nil {
			return fmt.Errorf("mesh.strategy=blockMesh requires system/blockMeshDict: %w", err)
		}
		return nil
	default: // auto
		if _, err := os.Stat(filepath.Join(m.Case.Path, "constant", "polyMesh", "points")); err == nil {
			return nil
		}
		if _, err := os.Stat(filepath.Join(m.Case.Path, "system", "blockMeshDict")); err == nil {
			return nil
		}
		return fmt.Errorf("mesh.strategy=auto found neither constant/polyMesh nor system/blockMeshDict")
	}
}

func (s *Service) Submit(ctx context.Context, manifestPath string) (Result, error) {
	if s.Scheduler == nil {
		return Result{}, fmt.Errorf("scheduler is not configured")
	}
	if s.Store == nil {
		return Result{}, fmt.Errorf("store is not configured")
	}
	s.observe("manifest", StageStarted, manifestPath)
	m, _, err := s.Validate(ctx, manifestPath)
	if err != nil {
		s.observe("manifest", StageFailed, err.Error())
		return Result{}, err
	}
	s.observe("manifest", StageDone, m.Name)
	cacheDir := m.Storage.CacheDir
	if cacheDir == "" {
		cacheDir = filepath.Join(filepath.Dir(manifestPath), ".foam-cache")
	}
	stager, err := stage.New(cacheDir)
	if err != nil {
		return Result{}, err
	}
	staged, err := stager.Stage(m.Case.Path, m.Case.Include...)
	if err != nil {
		s.observe("cache", StageFailed, err.Error())
		return Result{}, fmt.Errorf("stage case: %w", err)
	}
	s.observe("cache", StageDone, staged.Path)
	if s.Metrics != nil {
		if staged.Cached {
			s.Metrics.StageCacheHits.Add(1)
		} else {
			s.Metrics.StageCacheMisses.Add(1)
		}
	}
	id := uuid.NewString()
	if err := s.Store.Create(ctx, store.Job{ID: id, Name: m.Name, State: "STAGED", ManifestPath: manifestPath, CaseHash: staged.Hash, UpdatedAt: time.Now()}); err != nil {
		return Result{}, err
	}
	runDir := m.Storage.RunDir
	if runDir == "" {
		baseDir, _ := filepath.Abs(filepath.Dir(manifestPath))
		runDir = filepath.Join(baseDir, ".foam-runs")
	}
	runPath := filepath.Join(runDir, id)
	if err := stager.Materialize(staged.Path, runPath); err != nil {
		s.observe("run dir", StageFailed, err.Error())
		return Result{}, err
	}
	s.observe("run dir", StageDone, runPath)
	// Persist locations so operators can find logs without the submit stdout.
	_ = s.Store.UpdatePaths(ctx, id, runPath, staged.Path)
	script, err := checkpointScript(m, runPath)
	if err != nil {
		return Result{}, err
	}
	extra := map[string]any{"exclusive": true, "hint": "nomultithread"}
	if m.Solver.CheckpointSeconds > 0 {
		extra["signal"] = fmt.Sprintf("B:USR1@%d", m.Solver.CheckpointSeconds)
	}
	for k, v := range manifest.SbatchExtraVars(m.Resources.SbatchExtra) {
		extra[k] = v
	}
	spec := slurmrest.JobSpec{
		Name: m.Name, Partition: m.Resources.Partition, Account: m.Resources.Account,
		Dir:    runPath,
		Stdout: filepath.Join(runPath, "slurm-%j.out"),
		Stderr: filepath.Join(runPath, "slurm-%j.err"),
		Nodes:  m.Resources.Nodes, TasksPerNode: m.Resources.TasksPerNode,
		TimeLimitMin: m.Resources.TimeLimitMinutes,
		Requeue:      m.Solver.CheckpointSeconds > 0,
		Script:       script, Extra: extra,
	}
	jobID, err := s.Scheduler.Submit(ctx, spec)
	if err != nil {
		s.observe("submitted", StageFailed, err.Error())
		_ = s.Store.UpdateState(ctx, id, "FAILED", err.Error(), 0)
		return Result{}, err
	}
	if err := s.Store.UpdateState(ctx, id, "QUEUED", "", jobID); err != nil {
		return Result{}, err
	}
	if s.Metrics != nil {
		s.Metrics.Submissions.Add(1)
	}
	s.observe("submitted", StageDone, fmt.Sprintf("slurm job %d", jobID))
	return Result{ID: id, CaseHash: staged.Hash, CachePath: staged.Path, RunPath: runPath, SlurmID: jobID}, nil
}

// checkpointScript renders the Slurm batch script entirely from the manifest.
// Nothing about the physics is hard-coded: solver executable, solver args,
// file handler, MPI launcher, mesh strategy, post steps, container image,
// OpenFOAM environment, checkpoint interval, and extra SBATCH lines all come
// from manifest fields, so heavySimple (incompressibleFluid via foamRun),
// heavyBuoyant (fluid via foamRun), and any other OpenFOAM project share the
// same code path.
func checkpointScript(m manifest.Manifest, caseDir string) (string, error) {
	if strings.ContainsAny(caseDir, "\r\n") {
		return "", fmt.Errorf("unsafe case path")
	}
	if strings.ContainsAny(m.Name, "\r\n") || strings.ContainsAny(m.Resources.Partition, "\r\n") {
		return "", fmt.Errorf("unsafe job name or partition")
	}
	if strings.ContainsAny(m.Solver.Name, "\r\n") {
		return "", fmt.Errorf("unsafe solver name")
	}
	if strings.ContainsAny(m.Runtime.FoamBashrc, "\r\n") || !filepath.IsAbs(m.Runtime.FoamBashrc) {
		return "", fmt.Errorf("unsafe foam bashrc path")
	}
	for _, a := range m.Solver.Arguments {
		if strings.ContainsAny(a, "\r\n") {
			return "", fmt.Errorf("unsafe solver argument")
		}
	}
	var scriptedCmds []string
	scriptedCmds = append(scriptedCmds, m.Mesh.Commands...)
	scriptedCmds = append(scriptedCmds, m.Post.Commands...)
	if m.Build != nil {
		scriptedCmds = append(scriptedCmds, m.Build.Commands...)
	}
	for _, c := range scriptedCmds {
		if strings.ContainsAny(c, "\r\n") {
			return "", fmt.Errorf("unsafe mesh/post/build command")
		}
	}
	for _, a := range m.Runtime.MPI.ExtraArgs {
		if strings.ContainsAny(a, "\r\n") {
			return "", fmt.Errorf("unsafe mpi argument")
		}
	}

	ranks := m.Ranks()
	wall := m.WallTime()

	var sb strings.Builder
	sb.WriteString("#!/bin/bash\n")
	sb.WriteString(fmt.Sprintf("#SBATCH --job-name=%s\n", m.Name))
	if strings.TrimSpace(m.Resources.Partition) != "" {
		sb.WriteString(fmt.Sprintf("#SBATCH --partition=%s\n", m.Resources.Partition))
	}
	if strings.TrimSpace(m.Resources.Account) != "" {
		sb.WriteString(fmt.Sprintf("#SBATCH --account=%s\n", m.Resources.Account))
	}
	fmt.Fprintf(&sb, "#SBATCH --nodes=%d\n", m.Resources.Nodes)
	fmt.Fprintf(&sb, "#SBATCH --ntasks-per-node=%d\n", m.Resources.TasksPerNode)
	fmt.Fprintf(&sb, "#SBATCH --ntasks=%d\n", ranks)
	fmt.Fprintf(&sb, "#SBATCH --time=%s\n", wall)
	sb.WriteString("#SBATCH --exclusive\n")
	sb.WriteString("#SBATCH --hint=nomultithread\n")
	if m.Solver.CheckpointSeconds > 0 {
		sb.WriteString("#SBATCH --requeue\n")
		fmt.Fprintf(&sb, "#SBATCH --signal=B:USR1@%d\n", m.Solver.CheckpointSeconds)
	}
	for _, e := range m.Resources.SbatchExtra {
		sb.WriteString("#SBATCH " + e + "\n")
	}
	fmt.Fprintf(&sb, "#SBATCH --output=%s/slurm-%%j.out\n", shellQuote(caseDir))
	fmt.Fprintf(&sb, "#SBATCH --error=%s/slurm-%%j.err\n", shellQuote(caseDir))

	fmt.Fprintf(&sb, "set -euo pipefail\n")
	fmt.Fprintf(&sb, "echo \"=== %s job: %s ===\"\n", brand.Name, m.Name)
	fmt.Fprintf(&sb, "echo \"solver=%s ranks=%d wall=%s mesh=%s launcher=%s fileHandler=%s\"\n",
		m.Solver.Name, ranks, wall, m.Mesh.Strategy, m.Runtime.MPI.Launcher, m.Solver.FileHandler)
	sb.WriteString("date; echo \"nodelist: ${SLURM_NODELIST:-unknown} job: ${SLURM_JOB_ID:-unknown}\"\n")
	sb.WriteString("\n# OpenFOAM environment (per manifest runtime.foamBashrc).\n")
	sb.WriteString("# NOTE: foam bashrc dereferences unset $ZSH_NAME, so drop nounset around it.\n")
	sb.WriteString("set +e +u\n")
	fmt.Fprintf(&sb, "source %s\n", shellQuote(m.Runtime.FoamBashrc))
	sb.WriteString("set -e -u\n")
	sb.WriteString("echo \"OpenFOAM ${WM_PROJECT_VERSION:-unknown} at ${WM_PROJECT_DIR:-unknown}\"\n")
	fmt.Fprintf(&sb, "export OMP_NUM_THREADS=%d\n", m.Runtime.ThreadsPerRank)
	sb.WriteString(fmt.Sprintf("BASHRC=%s\n", shellQuote(m.Runtime.FoamBashrc)))

	// Execution wrappers take a single shell command string so mesh snippets
	// ("snappyHexMesh -overwrite"), dictionary edits, and the solver
	// invocation share one quoting path. The solver name itself is never
	// branched on: it is just another word in the command string.
	container := strings.TrimSpace(m.Solver.Container) != ""
	if container {
		fmt.Fprintf(&sb, "SIF=%s\n", shellQuote(m.Solver.Container))
		fmt.Fprintf(&sb, "BIND=%s\n", shellQuote(caseDir))
		sb.WriteString("foam_serial() { apptainer exec --bind \"$BIND\" \"$SIF\" bash -c \"source $BASHRC && $*\"; }\n")
		if m.Runtime.MPI.Launcher == "srun" {
			flav := ""
			if m.Runtime.MPI.Flavour != "" {
				flav = " --mpi=" + m.Runtime.MPI.Flavour
			}
			extra := ""
			if len(m.Runtime.MPI.ExtraArgs) > 0 {
				extra = " " + strings.Join(quotedAll(m.Runtime.MPI.ExtraArgs), " ")
			}
			fmt.Fprintf(&sb, "foam_mpi()    { srun%s --cpu-bind=cores%s apptainer exec --bind \"$BIND\" \"$SIF\" bash -c \"source $BASHRC && exec $*\"; }\n", flav, extra)
		} else {
			extra := ""
			if len(m.Runtime.MPI.ExtraArgs) > 0 {
				extra = " " + strings.Join(quotedAll(m.Runtime.MPI.ExtraArgs), " ")
			}
			fmt.Fprintf(&sb, "foam_mpi()    { mpirun -np \"${NTASKS:-%d}\"%s apptainer exec --bind \"$BIND\" \"$SIF\" bash -c \"source $BASHRC && exec $*\"; }\n", ranks, extra)
		}
	} else {
		sb.WriteString("foam_serial() { bash -c \"source $BASHRC && $*\"; }\n")
		if m.Runtime.MPI.Launcher == "srun" {
			flav := ""
			if m.Runtime.MPI.Flavour != "" {
				flav = " --mpi=" + m.Runtime.MPI.Flavour
			}
			extra := ""
			if len(m.Runtime.MPI.ExtraArgs) > 0 {
				extra = " " + strings.Join(quotedAll(m.Runtime.MPI.ExtraArgs), " ")
			}
			fmt.Fprintf(&sb, "foam_mpi()    { srun%s --cpu-bind=cores%s bash -c \"source $BASHRC && exec $*\"; }\n", flav, extra)
		} else {
			extra := ""
			if len(m.Runtime.MPI.ExtraArgs) > 0 {
				extra = " " + strings.Join(quotedAll(m.Runtime.MPI.ExtraArgs), " ")
			}
			fmt.Fprintf(&sb, "foam_mpi()    { mpirun -np \"${NTASKS:-%d}\"%s bash -c \"source $BASHRC && exec $*\"; }\n", ranks, extra)
		}
	}
	sb.WriteString("\n")
	fmt.Fprintf(&sb, "cd %s\n", shellQuote(caseDir))
	sb.WriteString("pwd; ls\n")

	// --- build (compile-if-necessary bootstrap) ---
	// A naked project may not have its solver binary installed yet. With a
	// build: section, the job first checks the real compute-node environment
	// and wmake-compiles the project only when the binary is missing.
	// Everything — build included — happens inside the Slurm allocation.
	if m.Build != nil {
		sb.WriteString("\n# --- build (compile if necessary, manifest build.source) ---\n")
		if container {
			sb.WriteString("echo \"ERROR: build.source is not supported with solver.container (sources are not visible in the image)\" >&2\n")
			sb.WriteString("exit 1\n")
		} else {
			fmt.Fprintf(&sb, "if ! command -v %s >/dev/null 2>&1; then\n", m.Solver.Name)
			if len(m.Build.Commands) > 0 {
				fmt.Fprintf(&sb, "  echo \"--- build: %s not on PATH; running build.commands ---\"\n", m.Solver.Name)
				for _, c := range m.Build.Commands {
					fmt.Fprintf(&sb, "  echo \"--- build cmd: %s ---\" >> log.build\n", c)
					fmt.Fprintf(&sb, "  foam_serial %s >> log.build 2>&1 || { echo \"ERROR: build command failed: %s (see log.build)\"; exit 1; }\n", shellSingle(c), c)
				}
			} else {
				fmt.Fprintf(&sb, "  echo \"--- build: %s not on PATH; wmake %s ---\"\n", m.Solver.Name, shellSingle(m.Build.Source))
				fmt.Fprintf(&sb, "  ( cd %s && wmake ) 2>&1 | tee log.build\n", shellSingle(m.Build.Source))
			}
			fmt.Fprintf(&sb, "  command -v %s >/dev/null 2>&1 || { echo \"ERROR: %s still not on PATH after build stage (see log.build)\"; exit 1; }\n", m.Solver.Name, m.Solver.Name)
			sb.WriteString("else\n")
			fmt.Fprintf(&sb, "  echo \"--- build skipped (%s already on PATH) ---\"\n", m.Solver.Name)
			sb.WriteString("fi\n")
		}
	}

	// --- mesh ---
	sb.WriteString("\n# --- mesh (strategy from manifest) ---\n")
	switch m.Mesh.Strategy {
	case "none":
		sb.WriteString("echo \"--- mesh skipped (strategy none) ---\"\n")
	case "blockMesh":
		sb.WriteString("echo \"--- blockMesh (strategy blockMesh) ---\"\n")
		fmt.Fprintf(&sb, "foam_serial %s 2>&1 | tee log.blockMesh\n", shellSingle("blockMesh"))
	case "custom":
		sb.WriteString("if [ ! -f constant/polyMesh/points ]; then\n")
		for _, c := range m.Mesh.Commands {
			fmt.Fprintf(&sb, "  echo \"--- mesh: %s ---\"\n", c)
			fmt.Fprintf(&sb, "  foam_serial %s 2>&1 | tee -a log.mesh\n", shellSingle(c))
		}
		sb.WriteString("else\n")
		sb.WriteString("  echo \"--- mesh skipped (constant/polyMesh exists) ---\"\n")
		sb.WriteString("fi\n")
	default: // auto
		sb.WriteString("if [ ! -f constant/polyMesh/points ]; then\n")
		sb.WriteString("  if [ -f system/blockMeshDict ]; then\n")
		sb.WriteString("    echo \"--- blockMesh (auto) ---\"\n")
		fmt.Fprintf(&sb, "    foam_serial %s 2>&1 | tee log.blockMesh\n", shellSingle("blockMesh"))
		sb.WriteString("  else\n")
		sb.WriteString("    echo \"ERROR: no constant/polyMesh and no system/blockMeshDict (strategy auto)\" >&2\n")
		sb.WriteString("    exit 1\n")
		sb.WriteString("  fi\n")
		sb.WriteString("else\n")
		sb.WriteString("  echo \"--- blockMesh skipped (constant/polyMesh exists) ---\"\n")
		sb.WriteString("fi\n")
	}

	// --- decompose ---
	sb.WriteString("\n# --- decompose (idempotent) ---\n")
	if ranks <= 1 {
		sb.WriteString("echo \"--- decomposePar skipped (serial run, ranks=1) ---\"\n")
	} else {
		sb.WriteString("echo \"--- decomposePar check ---\"\n")
		sb.WriteString("NPROC=$(grep -E \"^numberOfSubdomains\" system/decomposeParDict | grep -o \"[0-9]*\")\n")
		fmt.Fprintf(&sb, "NTASKS=${SLURM_NTASKS:-%d}\n", ranks)
		sb.WriteString("echo \"numberOfSubdomains=$NPROC NTASKS=$NTASKS\"\n")
		sb.WriteString("if [ \"$NPROC\" != \"$NTASKS\" ]; then\n")
		sb.WriteString("  echo \"ERROR: decomposeParDict($NPROC) != ntasks($NTASKS)\" >&2\n")
		sb.WriteString("  exit 1\n")
		sb.WriteString("fi\n")
		sb.WriteString("HAVE_PROC=0\n")
		sb.WriteString("shopt -s nullglob\n")
		sb.WriteString("PROC_DIRS=(processor*)\n")
		sb.WriteString("HAVE_PROC=${#PROC_DIRS[@]}\n")
		sb.WriteString("shopt -u nullglob\n")
		sb.WriteString("if [ \"$HAVE_PROC\" != \"$NTASKS\" ]; then\n")
		sb.WriteString("  echo \"--- decomposePar ($HAVE_PROC -> $NTASKS) ---\"\n")
		sb.WriteString("  rm -rf processor* 2>/dev/null || true\n")
		fmt.Fprintf(&sb, "  foam_serial %s 2>&1 | tee log.decomposePar\n", shellSingle("decomposePar"))
		sb.WriteString("else\n")
		sb.WriteString("  echo \"--- decomposePar skipped ($HAVE_PROC processor dirs) ---\"\n")
		sb.WriteString("fi\n")
	}

	// --- solver ---
	sb.WriteString("\n# --- solver (from manifest solver.name/arguments) ---\n")
	if m.Solver.CheckpointSeconds > 0 {
		sb.WriteString("checkpoint() {\n")
		sb.WriteString("  echo \"USR1 received: requesting writeNow checkpoint\"\n")
		fmt.Fprintf(&sb, "  foam_serial %s || true\n", shellSingle("foamDictionary -entry stopAt -set writeNow system/controlDict"))
		sb.WriteString("  touch .foam-checkpoint-requested\n")
		sb.WriteString("}\n")
		sb.WriteString("trap checkpoint USR1\n")
	}
	sb.WriteString("if [ -f system/controlDict ]; then\n")
	fmt.Fprintf(&sb, "  foam_serial %s || true\n", shellSingle("foamDictionary -entry startFrom -set latestTime system/controlDict"))
	sb.WriteString("fi\n")

	parallelBit := ""
	if ranks > 1 {
		parallelBit = " -parallel"
	}
	fhBit := ""
	switch m.Solver.FileHandler {
	case "collated":
		fhBit = " -fileHandler collated"
	case "uncollated":
		fhBit = " -fileHandler uncollated"
	}
	solverCmd := m.Solver.Name + joinArgs(m.Solver.Arguments) + parallelBit + fhBit
	if ranks <= 1 {
		fmt.Fprintf(&sb, "echo \"--- serial %s%s%s ---\"\n", m.Solver.Name, parallelBit, fhBit)
	} else {
		fmt.Fprintf(&sb, "echo \"--- %s %s%s%s ---\"\n", m.Runtime.MPI.Launcher, m.Solver.Name, parallelBit, fhBit)
	}
	sb.WriteString("set +e\n")
	if ranks <= 1 {
		fmt.Fprintf(&sb, "foam_serial %s > log.solver 2>&1\n", shellSingle(solverCmd))
	} else {
		fmt.Fprintf(&sb, "foam_mpi %s > log.solver 2>&1\n", shellSingle(solverCmd))
	}
	sb.WriteString("SOLVER_RC=$?\n")
	sb.WriteString("set -e\n")
	sb.WriteString("echo \"solver exit: $SOLVER_RC\"\n")
	sb.WriteString("tail -n 20 log.solver || true\n")
	if m.Solver.CheckpointSeconds > 0 {
		sb.WriteString("if [ -f .foam-checkpoint-requested ] && [ -n \"${SLURM_JOB_ID:-}\" ]; then\n")
		sb.WriteString("  echo \"checkpoint requested: requeue $SLURM_JOB_ID\"\n")
		sb.WriteString("  scontrol requeue \"$SLURM_JOB_ID\"\n")
		sb.WriteString("fi\n")
	}

	// --- post ---
	sb.WriteString("\n# --- post ---\n")
	sb.WriteString("if [ \"$SOLVER_RC\" -eq 0 ] && [ ! -f .foam-checkpoint-requested ]; then\n")
	if m.Post.Reconstruct && ranks > 1 {
		sb.WriteString("  echo \"--- reconstructPar ---\"\n")
		fmt.Fprintf(&sb, "  foam_serial %s 2>&1 | tee log.reconstructPar || echo \"reconstructPar warning (check log)\"\n", shellSingle("reconstructPar"))
	}
	for _, c := range m.Post.Commands {
		fmt.Fprintf(&sb, "  echo \"--- post: %s ---\"\n", c)
		fmt.Fprintf(&sb, "  foam_serial %s 2>&1 | tee -a log.post\n", shellSingle(c))
	}
	if !m.Post.Reconstruct && len(m.Post.Commands) == 0 {
		sb.WriteString("  echo \"--- post skipped (no reconstruct/commands) ---\"\n")
	}
	sb.WriteString("else\n")
	sb.WriteString("  echo \"--- post skipped (solver rc=$SOLVER_RC or checkpoint requested) ---\"\n")
	sb.WriteString("fi\n")
	sb.WriteString("exit $SOLVER_RC\n")

	return sb.String(), nil
}

func joinArgs(a []string) string {
	if len(a) == 0 {
		return ""
	}
	return " " + strings.Join(quotedAll(a), " ")
}

func quotedAll(a []string) []string {
	out := make([]string, len(a))
	for i, v := range a {
		out[i] = shellQuote(v)
	}
	return out
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }

// shellSingle wraps a whole shell command string for transport through the
// foam_serial/foam_mpi single-string wrappers. It is the same escaping as
// shellQuote; the split names document intent (single argv word vs whole
// command string).
func shellSingle(s string) string { return shellQuote(s) }
