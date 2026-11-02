package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ananymishradev/sluice/internal/preflight"
	"github.com/ananymishradev/sluice/internal/store"
	"github.com/ananymishradev/sluice/internal/telemetry"
)

// No solver name is hard-coded: foamRun with any physics model
// (incompressibleFluid like heavySimple, fluid like heavyBuoyant) and native
// solvers all share one code path.
func TestGenericSolvers(t *testing.T) {
	cases := []struct {
		name        string
		solver      string
		controlDict string
		wantScript  string
	}{
		{"foamRun-incompressibleFluid", "foamRun",
			"application foamRun;\nsolver incompressibleFluid;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"foamRun-fluid", "foamRun",
			"application foamRun;\nsolver fluid;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"foamRun-customModel", "foamRun",
			"application foamRun;\nsolver myCustomModel;\nrunTimeModifiable true;\n",
			"foamRun"},
		{"native-simpleFoam", "simpleFoam",
			"application simpleFoam;\nrunTimeModifiable true;\n",
			"simpleFoam"},
		{"native-custom", "myCustomSolver123",
			"application myCustomSolver123;\nrunTimeModifiable true;\n",
			"myCustomSolver123"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			caseDir := filepath.Join(root, "case")
			writeCase(t, caseDir, tc.controlDict, "2", true)
			mp := filepath.Join(root, "manifest.yaml")
			yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
				"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
				"solver:\n  name: " + tc.solver + "\n  checkpointSeconds: 60\n" +
				"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
			if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
				t.Fatal(err)
			}
			st, err := store.Open(filepath.Join(root, "jobs.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			fake := &fakeScheduler{}
			s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
			if _, err := s.Submit(context.Background(), mp); err != nil {
				t.Fatalf("submit %s: %v", tc.solver, err)
			}
			if !contains(fake.spec.Script, tc.wantScript) {
				t.Fatalf("script missing %q", tc.wantScript)
			}
		})
	}
}

func TestControlDictMismatchWarns(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\nrunTimeModifiable true;\n", "1", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: pimpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	_, report, err := s.Validate(context.Background(), mp)
	if err != nil {
		t.Fatalf("controlDict/solver drift must not fail validation: %v", err)
	}
	found := false
	for _, w := range report.Warnings {
		if w.Severity == "warning" && strings.Contains(w.Message, "application drift") &&
			strings.Contains(w.Message, "simpleFoam") && strings.Contains(w.Message, "pimpleFoam") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected an application-drift warning, got %+v", report.Warnings)
	}
}

func TestControlDictMatchIsSilent(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\nrunTimeModifiable true;\n", "1", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	_, report, err := s.Validate(context.Background(), mp)
	if err != nil {
		t.Fatalf("matching solver must validate: %v", err)
	}
	if len(report.Warnings) != 0 {
		t.Fatalf("matching solver must not warn, got %+v", report.Warnings)
	}
}

func TestCheckpointRequiresRunTimeModifiable(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "1", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 30\n" +
		"solver:\n  name: simpleFoam\n  checkpointSeconds: 60\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	if _, _, err := s.Validate(context.Background(), mp); err == nil {
		t.Fatal("expected runTimeModifiable error")
	}
}

func TestSerialRunSkipsDecompose(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\nstorage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.spec.Script, "serial run, ranks=1") {
		t.Fatal("serial run should skip decomposePar")
	}
	if contains(fake.spec.Script, " -parallel") {
		t.Fatal("serial run must not pass -parallel")
	}
}

func TestSrunAndFileHandlerNone(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n  fileHandler: none\n" +
		"runtime:\n  mpi:\n    launcher: srun\n    flavour: pmix\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if !contains(fake.spec.Script, "srun --mpi=pmix") {
		t.Fatal("expected srun launcher")
	}
	if contains(fake.spec.Script, "-fileHandler") {
		t.Fatal("fileHandler none must omit the flag")
	}
}

func TestCheckpointZeroDisablesSignal(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n  checkpointSeconds: 0\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(root, "jobs.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fake := &fakeScheduler{}
	s := Service{Scheduler: fake, Store: st, Preflight: preflight.Runner{}, Metrics: &telemetry.Metrics{}}
	if _, err := s.Submit(context.Background(), mp); err != nil {
		t.Fatal(err)
	}
	if contains(fake.spec.Script, "--signal") || contains(fake.spec.Script, "scontrol requeue") {
		t.Fatal("checkpoint 0 must disable signal/requeue")
	}
	if _, ok := fake.spec.Extra["signal"]; ok {
		t.Fatal("REST extra must not carry signal when checkpoint is 0")
	}
	if fake.spec.Requeue {
		t.Fatal("JobSpec.Requeue must be false when checkpoint is 0")
	}
}

func solverProject(t *testing.T, root, name string) string {
	t.Helper()
	src := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Join(src, "Make"), 0o750); err != nil {
		t.Fatal(err)
	}
	files := "EXE = " + name + "\n"
	if err := os.WriteFile(filepath.Join(src, "Make", "files"), []byte(files), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "Make", "options"), []byte("EXE_INC =\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return src
}

const noProbeStat = "probe unavailable"

func TestPlanInjectsGuardedBuildStage(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	src := solverProject(t, root, "src")
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n" +
		"build:\n  source: ./src\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}, ProbeSolver: func(context.Context, string, string) (bool, error) {
		return false, nil // binary absent locally: stage must wmake on the node
	}}
	plan, err := s.Plan(context.Background(), mp)
	if err != nil {
		t.Fatal(err)
	}
	script := plan.Script
	if !contains(script, "command -v simpleFoam") {
		t.Fatal("build stage must guard on the manifest solver binary")
	}
	if !contains(script, "( cd ") || !contains(script, "&& wmake )") {
		t.Fatal("build stage must wmake the resolved build.source")
	}
	if !contains(script, src) {
		t.Fatal("build.source must be resolved to an absolute path in the script")
	}
	if !contains(script, "log.build") {
		t.Fatal("build output must be kept in log.build")
	}
	if !contains(script, "still not on PATH after build stage") {
		t.Fatal("build stage must fail fast when wmake did not produce the binary")
	}
	if strings.Index(script, "# --- build") > strings.Index(script, "# --- mesh") {
		t.Fatal("build stage must run before meshing")
	}
}

func TestPlanSkipsBuildStageWithoutSection(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}}
	plan, err := s.Plan(context.Background(), mp)
	if err != nil {
		t.Fatal(err)
	}
	if contains(plan.Script, "wmake") {
		t.Fatal("no build: section means no build stage")
	}
}

func TestPlanBuildStageCommandsPath(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "2", true)
	src := solverProject(t, root, "src")
	mp := filepath.Join(root, "manifest.yaml")
	yml := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 2\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n" +
		"build:\n  commands:\n    - cd " + src + " && ./Allwmake\n" +
		"storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"
	if err := os.WriteFile(mp, []byte(yml), 0o640); err != nil {
		t.Fatal(err)
	}
	s := Service{Preflight: preflight.Runner{}, ProbeSolver: func(context.Context, string, string) (bool, error) {
		return false, nil
	}}
	plan, err := s.Plan(context.Background(), mp)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(plan.Script, "running build.commands") {
		t.Fatal("commands path must log its intent")
	}
	if !contains(plan.Script, "./Allwmake") {
		t.Fatal("bootstrap command must appear verbatim")
	}
	if contains(plan.Script, "&& wmake") || contains(plan.Script, "&& wmake ") {
		t.Fatal("commands path must not silently wmake")
	}
	if !contains(plan.Script, "log.build") {
		t.Fatal("bootstrap output must accumulate in log.build")
	}
}

func TestValidateNotesBuildStageDecision(t *testing.T) {
	root := t.TempDir()
	caseDir := filepath.Join(root, "case")
	writeCase(t, caseDir, "application simpleFoam;\n", "1", true)
	src := solverProject(t, root, "src")
	base := "apiVersion: sluice/v1alpha1\nname: test\ncase:\n  path: ./case\n" +
		"resources:\n  nodes: 1\n  tasksPerNode: 1\n  timeLimitMinutes: 5\n" +
		"solver:\n  name: simpleFoam\n"
	build := "build:\n  source: " + src + "\n"
	storage := "storage:\n  cacheDir: ./cache\n  runDir: ./runs\n"

	cases := []struct {
		name     string
		yml      string
		probe    func(context.Context, string, string) (bool, error)
		wantNote string
	}{
		{"binary-present-build-skips", base + build + storage, func(context.Context, string, string) (bool, error) { return true, nil }, "build stage will be skipped"},
		{"binary-absent-build-wmakes", base + build + storage, func(context.Context, string, string) (bool, error) { return false, nil }, "build stage will wmake"},
		{"no-build-and-no-binary", base + storage, func(context.Context, string, string) (bool, error) { return false, nil }, "no build: section"},
		{"probe-unavailable", base + build + storage, func(context.Context, string, string) (bool, error) { return false, errors.New(noProbeStat) }, "cannot probe"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mp := filepath.Join(root, tc.name+".yaml")
			if err := os.WriteFile(mp, []byte(tc.yml), 0o640); err != nil {
				t.Fatal(err)
			}
			s := Service{Preflight: preflight.Runner{}, ProbeSolver: tc.probe}
			_, report, err := s.Validate(context.Background(), mp)
			if err != nil {
				t.Fatalf("validate %s: %v", tc.name, err)
			}
			found := false
			for _, n := range report.Notes {
				if strings.Contains(n.Message, tc.wantNote) {
					found = true
				}
			}
			if !found {
				t.Fatalf("note %q missing in %+v", tc.wantNote, report.Notes)
			}
		})
	}
}
