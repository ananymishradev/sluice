# Changelog

## 2.0.0 - 2026-11-02

- Preflight and staging accept an `initial/` directory next to `0/` and
  `0.orig/` (laserbeamFoam's tutorial convention). Its dictionaries are
  scanned like `0/` for blocked directives.
- A controlDict `application` value that disagrees with the manifest solver
  is now a surfaceable warning instead of a hard validation error. The
  manifest solver stays authoritative; `foamRun` model validation remains
  strict.
- Optional `build.source` and `build.commands` stages compile a missing
  custom solver on the compute node before meshing, with output in
  `log.build`.
- Relative case, cache, run, and build-source paths resolve against the
  manifest directory.
- Manifest `apiVersion` is `sluice/v1alpha1`; `sluice init` writes it.
- Env vars use the `SLUICE_*` names (e.g. `SLUICE_API_TOKEN`). Values are
  never logged.
- DB default file is `sluice.db` next to the manifest. User data is never
  moved, renamed, copied, or deleted automatically.
- New runs write `sluice.sbatch`; existing run directories are untouched.
- Prometheus metrics use the `sluice_*` prefix with no dual emission.
  Point dashboards and alerts at those names.
- The `it` alias for `run` stays. `.foam-cache/` and `.foam-runs/` keep
  their names. The installer never deletes other copies it finds.
