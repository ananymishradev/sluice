<div align="center">

<img src="sluice-logo.png" alt="Sluice logo" width="600">

[![LICENSE](https://img.shields.io/github/license/ananymishradev/sluice.svg)](LICENSE)
[![build status](https://github.com/ananymishradev/sluice/actions/workflows/go.yaml/badge.svg)](https://github.com/ananymishradev/sluice/actions/workflows/go.yaml)
[![Releases](https://img.shields.io/github/v/release/ananymishradev/sluice)](https://github.com/ananymishradev/sluice/releases/latest)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/ananymishradev/sluice/badge)](https://scorecard.dev/viewer/?uri=github.com/ananymishradev/sluice)
[![Go Version](https://img.shields.io/github/go-mod/go-version/ananymishradev/sluice)](go.mod)

# Sluice

**Run OpenFOAM cases on cluster of CPUs and find out your broken case before the queue does**

</div>
Sluice is a small Go tool that sits between OpenFOAM and Slurm. It checks a
case before submission, caches it, gives each run its own directory, tracks the
job in SQLite, and follows it to the end. Slurm still owns scheduling and
OpenFOAM still runs the solver.

## How it works

The workflow is simple:

```text
manifest.yaml
    │
    ▼
strict manifest validation
    │
    ▼
case preflight
    │
    ▼
SHA-256 cache and isolated run directory
    │
    ▼
SQLite job record
    │
    ▼
slurmrestd submission
    │
    ▼
OpenFOAM solver with checkpoint/requeue handling
    │
    ▼
Slurm state supervision and Prometheus metrics
```

## What it does for you

- Strict YAML manifests using `sluice/v1alpha1`
- OpenFOAM case checks for `system`, `constant`, and `0`
- Protection against symlinks that escape the case directory
- Rejection of code-loading directives such as:
  `#codeStream`, `#calc`, `codedFixedValue`, `codedMixed`, and `libs`.
- Optional `foamDictionary` and `checkMesh` checks
- SHA-256 case caching with reuse for identical inputs
- Isolated run directories, so checkpoint edits never change the cache
- SQLite job state with WAL mode
- Slurm REST and command-line submission, lookup, and cancellation
- `USR1` checkpoint handling with `scontrol requeue`
- Prometheus-compatible `/metrics`
- Health and read-only job listing endpoints
- Unit and integration-style tests for the local workflow

## Before you start

- Go 1.26 or newer, installed system-wide so `go` is on the default `PATH`:
  ```bash
  sudo rm -rf /usr/local/go
  sudo tar -xzf go1.26.0.linux-amd64.tar.gz -C /usr/local
  sudo ln -sf /usr/local/go/bin/go /usr/local/bin/go
  go version  # go version go1.26.0 linux/amd64
  ```
- OpenFOAM available on the execution host or inside the configured runtime
  (this cluster: `/opt/openfoam12/etc/bashrc`, identical on every node).
- Slurm with `sbatch`/`squeue`/`scontrol` on `PATH` (CLI backend, works
  without further setup), and/or `slurmrestd` with a valid REST API version
  (such as `v0.0.43`) plus a Slurm JWT for the REST backend.
- `mpirun` (OpenMPI) and the selected OpenFOAM tools available to jobs.

The repository also includes `openfoam.def` for building an Apptainer image.
Check container and MPI compatibility with your cluster before using it.

## Build and run from source

If you are developing Sluice, start in the repository checkout and run:

```bash
go mod download
go test ./...
```

Build the CLI into `bin/sluice` and check that it starts:

```bash
mkdir -p bin
CGO_ENABLED=0 go build -trimpath \
  -ldflags "-s -w -X main.Version=$(git rev-parse --short HEAD)" \
  -o bin/sluice ./cmd/sluice
./bin/sluice version
```

Run a case with the binary you just built:

```bash
./bin/sluice run --manifest /path/to/manifest.yaml
```

Or start the HTTP service:

```bash
./bin/sluice serve --addr :8080 --db sluice.db
```

For a one-off run without producing a binary, use `go run`:

```bash
go run ./cmd/sluice run --manifest /path/to/manifest.yaml
```

## Install and uninstall

The easiest way to install the command is to use the included script. By
default it builds `sluice` and places it in `~/.local/bin`:

```bash
./install.sh
sluice version
```

If `~/.local/bin` is not on your `PATH`, add it for the current shell:

```bash
export PATH="$HOME/.local/bin:$PATH"
sluice version
```

To install somewhere else, choose a prefix or an exact binary directory:

```bash
./install.sh --prefix "$HOME/.local"
./install.sh --bindir "$HOME/bin"
```

To install for all users, use the system option. It normally requires `sudo`:

```bash
sudo ./install.sh --system
```

Remove the copy installed in the default location:

```bash
./uninstall.sh
```

If you have more than one copy installed, remove copies found in the common
locations with:

```bash
./uninstall.sh --all
```

These scripts remove only the `sluice` executable. They do not remove your
SQLite databases, `.foam-cache` directories, or `.foam-runs` directories.

Update an installed copy to the latest published release on Linux or macOS:

```bash
sluice update
```

The updater verifies the release binary's SHA-256 checksum and atomically
replaces the current executable. It needs write permission to that executable's
directory; use the installer with appropriate privileges for a system-wide
copy. Unreleased branches are not available through this command.

If you do not want to install anything, use the source directly:

```bash
go run ./cmd/sluice version
go run ./cmd/sluice run --manifest /path/to/manifest.yaml
```

The commands in the rest of this guide assume `sluice` is on your `PATH`.

## Run a case

```bash
sluice run --manifest manifest.yaml
```

`it` is a short alias for `run`. These three forms are identical:

```bash
sluice --manifest manifest.yaml
sluice it --manifest manifest.yaml
sluice run --manifest manifest.yaml
```

Use `--dry-run` to validate and stage a case and print the batch script without
creating a run directory, writing a database record, or submitting anything to
Slurm:

```bash
sluice run --manifest manifest.yaml --dry-run
```

Use `--no-watch` when you want to submit the job and return immediately:

```bash
sluice run --manifest manifest.yaml --no-watch
```

## Check the environment

Run `doctor` before sending a job to the queue. It checks the Slurm and MPI
commands and verifies that the database directory is writable:

```bash
sluice doctor
```

Use JSON output in scripts:

```bash
sluice doctor --output json
```

Doctor exits with status `0` when there are no failures and `3` when the
environment has a failure.

## List jobs

Show recent jobs in a readable table:

```bash
sluice list --db sluice.db
sluice list --db sluice.db --state RUNNING --limit 10
```

For scripts, JSON is written only to standard output:

```bash
sluice list --db sluice.db --output json | jq
```

Human-readable status and error messages go to standard error, so they do not
corrupt the JSON stream.

## Exit codes

| Code | Meaning |
|---:|---|
| `0` | Command completed successfully |
| `1` | Job or run failed |
| `2` | Invalid usage or validation error |
| `3` | Environment problem, such as missing Slurm tools |

## Where to look in the code

```text
cmd/sluice/
  main.go
  CLI commands and the local HTTP service

internal/manifest/
  Manifest types, strict YAML loading, validation, example manifest

internal/preflight/
  OpenFOAM case layout, symlink, directive, and tool checks

internal/stage/
  Content hashing, cache population, and isolated run materialization

internal/store/
  SQLite database schema and job persistence

internal/service/
  End-to-end validation, staging, persistence, and scheduler submission

internal/supervisor/
  Slurm state polling and local state reconciliation

internal/telemetry/
  Prometheus text exposition

client.go
  slurmrestd client and Scheduler interface

solver.sbatch.tmpl
  Standalone Slurm batch template

openfoam.def
  Apptainer definition
```

## Configure a case with a manifest

No solver, mesh tool, MPI launcher, or file handler is hard-coded. The same
schema runs heavySimple (`foamRun` + `incompressibleFluid`), heavyBuoyant
(`foamRun` + `fluid`), any stock solver (`simpleFoam`, `pimpleFoam`,
`buoyantPimpleFoam`, ...), and any custom-compiled solver: `solver.name` is
an arbitrary bare executable, and the batch script is rendered entirely from
manifest fields. Create a starter manifest with:

```bash
sluice init --output manifest.yaml --solver foamRun --case ./case
```

Full schema (`examples/generic.yaml`):

```yaml
apiVersion: sluice/v1alpha1
name: cavity

case:
  path: ./case
  include: []            # extra top-level inputs to stage (e.g. ["Allrun"])

resources:
  partition: ""         # empty = cluster default
  account: ""
  nodes: 1
  tasksPerNode: 8
  timeLimitMinutes: 60
  sbatchExtra: []       # e.g. ["--gres=gpu:1"], appended to SBATCH + REST Extra

solver:
  name: foamRun         # any executable: foamRun, simpleFoam, myCustomSolver, ...
  arguments: []
  fileHandler: collated # collated, uncollated, none
  checkpointSeconds: 600 # USR1 writeNow N s before walltime; 0 disables
  # container: /images/openfoam.sif

mesh:
  strategy: auto        # auto, blockMesh, custom, none
  # commands: ["snappyHexMesh -overwrite"]

build:
  # Choose one: wmake a project root, or run custom build commands.
  # source: ./solver
  # commands: ["cd /shared/project && ./Allwmake"]

post:
  reconstruct: false
  # commands: ["foamPostProcess -func sample"]

runtime:
  foamBashrc: /opt/openfoam12/etc/bashrc  # or $FOAM_BASHRC; auto-detected
  threadsPerRank: 1
  mpi:
    launcher: mpirun    # mpirun or srun
    flavour: ""         # srun --mpi value; mpirun ignores it

storage:
  cacheDir: ./.foam-cache
  runDir: ./.foam-runs
```

Paths are resolved relative to the manifest file. The case directory must
contain:

```text
case/
├── 0/            # or 0.orig, or initial/ (any of the three satisfies preflight)
├── constant/
└── system/
```

For custom solvers, an optional `build` section runs only when the solver is
not already available in the OpenFOAM environment. `build.source` must point
to a `wmake` project containing `Make/files`; `build.commands` runs custom
single-line commands instead. Choose exactly one. The source tree must be
visible at the same path on compute nodes. The generated job logs build
output to `log.build`, then verifies the solver is available before meshing.
This bootstrap is unavailable with `solver.container`.

Important fields:

| Field | Meaning |
|---|---|
| `apiVersion` | Must be `sluice/v1alpha1`. |
| `name` | Slurm job name and local job name. |
| `case.path` | OpenFOAM case directory. |
| `case.include` | Extra top-level case files/dirs to stage and hash. |
| `resources.partition` | Slurm partition (empty = default). |
| `resources.account` | Slurm accounting account. |
| `resources.nodes` | Number of nodes. |
| `resources.tasksPerNode` | MPI tasks per node. |
| `resources.timeLimitMinutes` | Slurm time limit. |
| `resources.sbatchExtra` | Raw `--key[=value]` SBATCH lines (never repeats managed opts). |
| `solver.name` | Any solver executable, for example `foamRun` or `myCustomSolver`. |
| `solver.arguments` | Additional solver arguments. |
| `solver.fileHandler` | `collated`, `uncollated`, or `none`. |
| `solver.checkpointSeconds` | USR1 lead time; `0` disables checkpoint/requeue. |
| `build.source` / `build.commands` | Optional compute-node build step; use exactly one. |
| `mesh.strategy` | `auto`, `blockMesh`, `custom`, or `none`. |
| `post.reconstruct` | Run `reconstructPar` after a clean parallel solve. |
| `runtime.foamBashrc` | OpenFOAM env; manifest > `$FOAM_BASHRC` > auto-detect. |
| `runtime.mpi.launcher` | `mpirun` or `srun`. |
| `storage.cacheDir` | Content-addressed cache root. |
| `storage.runDir` | Per-job working directory root. |

Validation also checks the case against the manifest generically: a
`controlDict` `application` mismatch is reported as a warning because the
manifest solver is authoritative; `foamRun` still requires a `solver <model>;`
physics entry. `decomposeParDict` must match `nodes × tasksPerNode` for
parallel runs, `checkpointSeconds` requires `runTimeModifiable true`, and
`mesh.strategy` must have a mesh source.

### Add extra files to the OpenFOAM case

Sluice always stages the standard OpenFOAM inputs (`0/`, `0.orig/` or
`initial/` — the laserbeamFoam convention, `constant/`, and `system/`).
Other files at the top level of the case are ignored unless you list them
in `case.include`.

Use `case.include` for files or directories that your case needs, such as
`Allrun`, `Allclean`, custom scripts, or additional configuration:

```yaml
case:
  path: ./case
  include:
    - Allrun
    - Allclean
    - scripts
```

The entries are relative to `case.path`, but they must refer to an additional
top-level name. For example, `scripts` includes the whole `scripts/` directory.
The standard initial-fields directory (`0/`, `0.orig/`, or `initial/`),
`constant/`, and `system/` are already included and must not be listed again.
Included files are copied into the cached case,
included in its content hash, and available in the isolated run directory on
the compute node.

Keep generated output and runner state out of `case.include`. The tool rejects
`.git`, `.foam-cache`, `.foam-runs`, `processor*`, logs, and SQLite database
files. After changing an included file, submit the job again so Sluice
creates or reuses the correct cache entry.

## Check your setup

Format and test the project:

```bash
go test ./...
go vet ./...
go test -race ./...
```

Validate a case before submitting:

```bash
sluice validate --manifest manifest.yaml
```

The command prints JSON containing the parsed manifest, preflight report, and
an error when validation fails.

## Run an OpenFOAM case

```bash
sluice run --manifest /home/shared/openfoam/heavySimple/manifest.yaml
sluice run --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
```

That one command validates the manifest and case, stages it into the
content-addressed cache, materializes an isolated run directory, submits to
Slurm, and watches until DONE/FAILED/CANCELLED — printing the local ID, Slurm
ID, run directory, and both log paths. Defaults: `-db` is
`<manifest-dir>/sluice.db`, `-backend auto`. Add `-tail` to stream
`log.solver` and `slurm-<id>.out` while watching:

```bash
sluice run --manifest /home/shared/openfoam/heavySimple/manifest.yaml --tail
sluice run --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml --tail
```

`submit`/`watch`/`status`/`cancel` below are the same pipeline split into
steps for scripting; `run` is just `validate + submit -watch` in one call.

## Submit a job step by step

Pick a backend first (see Scheduler backends below). For the REST backend,
set the Slurm connection values (`SLURM_JWT_FILE` is preferred over
`SLURM_JWT` because it is re-read on every request, so rotated tokens are
picked up without a restart; `SLURM_SOCKET` selects Unix-socket transport):

```bash
export SLURM_REST_URL=http://slurmrestd.example:6820
export SLURM_API_VERSION=v0.0.43
export SLURM_USER="$USER"
export SLURM_JWT_FILE="$HOME/.slurm/token"  # or SLURM_JWT for short-lived setups
```

Submit (CLI backend shown; needs nothing but Slurm on `PATH`):

```bash
sluice submit \
  -manifest manifest.yaml \
  -db sluice.db \
  -backend cli
```

Submit and keep polling Slurm until the job reaches a terminal state:

```bash
sluice submit \
  -manifest manifest.yaml \
  -db sluice.db \
  -backend cli \
  -watch
```

The command returns a local job ID, case hash, run path, cache path, and Slurm
job ID.

Reconcile the state of an already-submitted job (updates the DB record to
DONE/FAILED/CANCELLED once Slurm reaches a terminal state):

```bash
sluice watch \
  -db sluice.db \
  -id <local-job-id> \
  -slurm <slurm-job-id>
```

One-shot refresh, cancel, and listing:

```bash
sluice status --db sluice.db --id <local-job-id> --refresh
sluice cancel --db sluice.db --id <local-job-id>
sluice list --db sluice.db --state RUNNING
```

## Choose a scheduler backend

`submit`, `watch`, `status`, and `cancel` accept `-backend auto|rest|cli`
(default `auto`).

- `rest` talks to `slurmrestd` and needs `SLURM_API_VERSION`, `SLURM_USER`,
  `SLURM_JWT` or `SLURM_JWT_FILE`, and `SLURM_REST_URL` or `SLURM_SOCKET`.
- `cli` shells out to `sbatch`/`squeue`/`scontrol`/`sacct`/`scancel` and needs no
  extra configuration. `sacct` is the last-resort state source for finished
  jobs once `slurmctld` forgets them (unavailable when accounting storage is
  disabled, in which case the job is recorded `UNKNOWN` with a bounded retry
  budget).
- `auto` prefers `rest` when the REST variables are all set and falls
  back to `cli` otherwise. Clusters without `slurmrestd` (the common case)
  work out of the box via `cli`.

## Example cases

Both cases run through the identical generic path — the only differences are
the case directory and the physics model declared in `controlDict`
(`incompressibleFluid` vs `fluid`); the runner executable is `foamRun` in
both manifests and is never special-cased:

```bash
sluice validate --manifest /home/shared/openfoam/heavySimple/manifest.yaml
sluice validate --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
sluice run --manifest /home/shared/openfoam/heavySimple/manifest.yaml
sluice run --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml
squeue
tail -f /home/shared/openfoam/heavySimple/.foam-runs/<job-id>/slurm-<slurm-id>.out
```

The generated job script is a full pipeline driven by the manifest: mesh
(`auto` uses `constant/polyMesh` when present, else `blockMesh` when
`system/blockMeshDict` exists; `custom` runs `mesh.commands`; `none` skips)
→ `decomposePar` for parallel runs (rebuilt when the processor count
mismatches `nodes × tasksPerNode`; skipped for serial `ranks=1`) → solver
`<solver.name> <args> [-parallel] [-fileHandler ...]` via `mpirun` or `srun`
with a `USR1` `writeNow` checkpoint trap and `scontrol requeue` when
`checkpointSeconds > 0` → optional `reconstructPar` and `post.commands`.
The manifest's `decomposeParDict` subdomain count is validated against the
requested ranks before submission, so a mismatch fails fast instead of wasting
queue time. Only `0/` (or `0.orig/`, `initial/`), `constant/`, `system/`, plus
explicit `case.include` tops are staged and hashed; docs, scripts, logs, and
`.foam-*` state never affect the case hash.

## Find logs

Every sluice run writes three logs inside its isolated run directory.
Slurm expands `%j` to the Slurm job ID, so the addresses are:

```text
<case>/.foam-runs/<local-job-id>/log.solver      # solver residuals (all ranks)
<case>/.foam-runs/<local-job-id>/slurm-<slurm-id>.out  # Slurm stdout: mesh, decompose, batch echo
<case>/.foam-runs/<local-job-id>/slurm-<slurm-id>.err  # Slurm stderr
```

Concrete addresses:

```text
heavySimple:  /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/slurm-<slurm-id>.out
              /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/slurm-<slurm-id>.err
              /home/shared/openfoam/heavySimple/.foam-runs/<local-job-id>/log.solver
heavyBuoyant: /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/slurm-<slurm-id>.out
              /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/slurm-<slurm-id>.err
              /home/shared/openfoam/heavyBuoyant/.foam-runs/<local-job-id>/log.solver
```

The same run directory also holds `log.blockMesh`, `log.decomposePar`,
`processor*`, and `sluice.sbatch` (the exact script Slurm executed —
resubmit it with `sbatch` to reproduce).

You never need to paste IDs by hand. The `run` command prints all three
paths at submit time, `-tail` streams them live, and `logs` resolves any
past job from the database:

```bash
# stream the newest heavySimple job (no IDs typed):
sluice logs --manifest /home/shared/openfoam/heavySimple/manifest.yaml --latest

# stream the newest heavyBuoyant job:
sluice logs --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml --latest

# stream one specific job:
sluice logs --manifest /home/shared/openfoam/heavySimple/manifest.yaml --id <local-job-id>

# plain tail -f with auto-resolved paths (no IDs typed):
tail -f $(sluice logs --manifest /home/shared/openfoam/heavySimple/manifest.yaml --latest --follow=false)
tail -f $(sluice logs --manifest /home/shared/openfoam/heavyBuoyant/manifest.yaml --latest --follow=false)
```

Other layouts for reference:

- **Direct `sbatch run_openfoam.slurm`:** `/home/shared/openfoam/heavySimple/slurm-<JOBID>.out`
  and `slurm-<JOBID>.err` (absolute paths baked into that script).
- **Standalone `solver.sbatch.tmpl`:** `<caseDir>/slurm-%j.out` / `.err`,
  where `<caseDir>` is the template's `CaseDir` field.

Solver residuals live in `<runDir>/log.solver` regardless of backend, so
`tail -f` that file plus the `slurm-*.out` above for mesh/decompose progress.

JWTs are never logged. Prefer `SLURM_JWT_FILE` over `SLURM_JWT` so the token
is re-read on every request, and do not place long-lived credentials in shell
history, process listings, or shared logs.

## Run the HTTP service

Start the local HTTP service:

```bash
sluice serve \
  -addr :8080 \
  -db sluice.db
```

Available endpoints:

```text
GET /healthz   liveness response
GET /readyz    readiness (checks the database)
GET /metrics   Prometheus text exposition
GET /jobs      persisted jobs as JSON (?state=RUNNING filters)
GET /version   build version
```

Example:

```bash
curl http://localhost:8080/healthz
curl http://localhost:8080/metrics
curl http://localhost:8080/jobs
```

Set `SLUICE_API_TOKEN` to require `Authorization: Bearer <token>` on
`/jobs`. The HTTP service exposes health, metrics, and read-only job listing
only. It does not expose anonymous job submission. Add authentication and
authorization before putting it behind a shared network endpoint.

## Understand the cache and run directories

The cache is keyed by a SHA-256 hash of relative file names and file contents.
An unchanged case reuses the same cache directory:

```text
.foam-cache/
└── <sha256>/
```

Every submission receives its own run directory:

```text
.foam-runs/
└── <local-job-id>/
```

The run directory is a copy of the cached case. This is important because the
checkpoint handler edits `system/controlDict`; changing the cache would make
future jobs non-reproducible.

## Checkpoint and requeue

When `solver.checkpointSeconds > 0`, the generated solver script:

1. Starts the solver under `mpirun` or `srun` (per `runtime.mpi`).
2. Receives `USR1` that many seconds before the Slurm time limit.
3. Sets `stopAt` to `writeNow`.
4. Marks the run as checkpoint-requested.
5. Waits for the solver to exit cleanly.
6. Calls `scontrol requeue` for the current Slurm job.
7. Starts from `latestTime` on the next execution.

When `checkpointSeconds: 0`, the `--signal`/`--requeue` directives, the trap,
and the requeue call are all omitted and the solver runs straight through.

This requires (only when checkpointing is enabled):

- `system/controlDict` to be runtime-modifiable;
- a valid `foamDictionary` command;
- Slurm requeue permission;
- a solver that writes a usable latest time before exiting.

The standalone `solver.sbatch.tmpl` remains available for users who want to
render or submit a batch script outside the Go service.

## Security notes

OpenFOAM dictionaries can load and compile code. A user-supplied case can
therefore become arbitrary code execution if it is run without restrictions.
The default preflight rejects known code-loading directives and symlinks that
escape the case directory.

This is only a first security boundary. A production multi-user deployment
should also:

- authenticate every API caller;
- authorize Slurm accounts, partitions, and filesystem paths;
- run jobs with the submitting user's identity or an explicitly audited
  service identity;
- isolate case data and temporary files by user;
- use a read-only or verified container image;
- apply CPU, memory, disk, and wall-clock limits;
- avoid exposing the SQLite database or cache over shared writable paths;
- log submissions and state transitions without logging JWTs;
- scan archives for path traversal before extraction;
- review any additional OpenFOAM directives introduced by the target version.

## Known limitations

This is a working control-plane foundation, not a complete hosted SaaS product.
The following areas still need deployment-specific work:

- token refresh and secret management;
- HTTP authentication and authorization;
- real API handlers for authenticated submit/cancel operations;
- Postgres or another multi-instance database if SQLite is insufficient;
- transactional outbox/event delivery;
- scheduler reconciliation after service restarts;
- full `foamDictionary` schema validation per solver;
- complete `checkMesh` result parsing and quality thresholds;
- log parsing for residuals, Courant number, and step time;
- Grafana dashboards and alert rules;
- stage-out/result indexing;
- autotuning and scaling benchmarks;
- multi-node container/MPI validation;
- cleanup and retention policies.

## Development notes

The scheduler boundary is the `Scheduler` interface:

```go
type Scheduler interface {
    Submit(context.Context, JobSpec) (int, error)
    State(context.Context, int) ([]string, error)
    Cancel(context.Context, int) error
}
```

This keeps the orchestration layer independent of Slurm REST details and leaves
room for an `sbatch`/`squeue`/`sacct` adapter or another scheduler later.

## License

Sluice is available under the [Apache License 2.0](LICENSE).
