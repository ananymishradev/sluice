package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	slurmrest "github.com/ananymishradev/sluice"
	"github.com/ananymishradev/sluice/internal/brand"
	"github.com/ananymishradev/sluice/internal/preflight"
	schedcli "github.com/ananymishradev/sluice/internal/scheduler"
	"github.com/ananymishradev/sluice/internal/service"
	"github.com/ananymishradev/sluice/internal/store"
	"github.com/ananymishradev/sluice/internal/supervisor"
	"github.com/ananymishradev/sluice/internal/telemetry"
	"github.com/spf13/pflag"
)

// Version is set at build time: go build -ldflags "-X main.Version=...".
var Version = "2.0.0"

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	os.Exit(executeRoot())
}

// run is the single command that validates, submits, and watches an
// OpenFOAM case until Slurm reaches a terminal state:
//
//	sluice run -manifest /home/shared/openfoam/heavySimple/manifest.yaml
//
// Defaults are chosen so that running from the case directory needs no flags
// at all (manifest.yaml + sluice.db next to it, CLI backend). It is
// equivalent to validate + submit -watch, with an optional live log tail.
func run(ctx context.Context, args []string) error {
	fs := pflag.NewFlagSet("run", pflag.ExitOnError)
	manifestPath := fs.String("manifest", "manifest.yaml", "path to manifest")
	dbPath := fs.String("db", "", "SQLite database path (default <manifest-dir>/"+brand.DefaultDBFile+")")
	backend := fs.String("backend", "auto", "scheduler backend: auto, rest, cli")
	timeout := fs.Duration("timeout", 5*time.Minute, "submit-phase timeout (0 disables)")
	interval := fs.Duration("interval", 15*time.Second, "watch poll interval (e.g. 10s, 1m)")
	tail := fs.Bool("tail", false, "stream log.solver and slurm-*.out while watching")
	tailInterval := fs.Duration("tail-interval", 5*time.Second, "log tail poll interval")
	dryRun := fs.Bool("dry-run", false, "validate, stage, and render without submitting")
	noWatch := fs.Bool("no-watch", false, "submit and exit without watching")
	output := fs.StringP("output", "o", outputFormat, "output format: table or json")
	plain := fs.Bool("no-color", noColor, "disable terminal colors and live view")
	_ = fs.Parse(args)
	outputFormat = *output
	noColor = *plain

	db := strings.TrimSpace(*dbPath)
	if db == "" {
		var usedLegacy bool
		db, usedLegacy = brand.ResolveDBPath(*manifestPath, "")
		if usedLegacy {
			fmt.Fprintln(os.Stderr, brand.LegacyDBNotice(db))
		}
	}
	if liveRunEnabled() && !*dryRun {
		return runLive(ctx, liveRunOptions{
			Manifest: *manifestPath, DB: db, Backend: *backend,
			Timeout: *timeout, Interval: *interval, Tail: *tail,
			TailInterval: *tailInterval, NoWatch: *noWatch,
		})
	}
	if !liveRunEnabled() {
		var stop context.CancelFunc
		ctx, stop = signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()
	}
	submitCtx := ctx
	if *timeout > 0 {
		var cancel context.CancelFunc
		submitCtx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	// Fail fast on manifest/case problems before touching the database or
	// Slurm, so a typo never creates stray state.
	if *dryRun {
		plan, err := (&service.Service{Preflight: preflight.Runner{}, Observer: cliObserver{}}).Plan(ctx, *manifestPath)
		if err != nil {
			return err
		}
		fmt.Printf("# dry run: nothing was submitted\n")
		fmt.Printf("# cache: %s (%s)\n", plan.Staged.Path, map[bool]string{true: "hit", false: "miss"}[plan.Staged.Cached])
		fmt.Printf("# rendered batch script:\n%s", plan.Script)
		return nil
	}
	scheduler, backendName, err := selectScheduler(*backend)
	if err != nil {
		return err
	}
	log.Printf(brand.Name+" run backend=%s manifest=%s db=%s", backendName, *manifestPath, db)
	vs := service.Service{Preflight: preflight.Runner{}}
	if _, _, err := vs.Validate(ctx, *manifestPath); err != nil {
		return err
	}
	st, err := store.Open(db)
	if err != nil {
		return err
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	s := service.Service{Scheduler: scheduler, Store: st, Preflight: preflight.Runner{}, Metrics: metrics, Observer: cliObserver{}}
	result, err := s.Submit(submitCtx, *manifestPath)
	if err != nil {
		return err
	}
	log.Printf("submitted id=%s slurm=%d run=%s", result.ID, result.SlurmID, result.RunPath)
	log.Printf("solver log: %s", filepath.Join(result.RunPath, "log.solver"))
	log.Printf("slurm log:  %s", filepath.Join(result.RunPath, fmt.Sprintf("slurm-%d.out", result.SlurmID)))
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if *noWatch {
		return nil
	}

	// Watching is decoupled from the submit timeout: Ctrl-C stops polling
	// without losing the job record (Slurm keeps running; re-watch later).
	wctx := context.WithoutCancel(ctx)
	wctx, stop := signal.NotifyContext(wctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *tail {
		go tailRunLogs(wctx, result.RunPath, result.SlurmID, *tailInterval)
	}
	w := supervisor.Supervisor{Scheduler: scheduler, Store: st, Metrics: metrics, PollInterval: *interval}
	if err := w.Watch(wctx, result.ID, result.SlurmID); err != nil {
		return err
	}
	j, err := st.Get(wctx, result.ID)
	if err != nil {
		return err
	}
	log.Printf("terminal state=%s slurm=%d", j.State, j.SlurmID)
	if err := json.NewEncoder(os.Stdout).Encode(j); err != nil {
		return err
	}
	if j.State == "FAILED" || j.State == "CANCELLED" {
		return exitError{code: exitJob, err: fmt.Errorf("job ended in %s", j.State)}
	}
	return nil
}

// tailRunLogs streams new lines from the solver log and the Slurm output
// and error files until ctx is done. Missing files are tolerated (they
// appear once the job starts); offsets reset if a file shrinks.
func tailRunLogs(ctx context.Context, runPath string, slurmID int, interval time.Duration) {
	tailRunLogsTo(ctx, runPath, slurmID, interval, os.Stderr)
}

func tailRunLogsTo(ctx context.Context, runPath string, slurmID int, interval time.Duration, out io.Writer) {
	if interval <= 0 {
		interval = 5 * time.Second
	}
	files := []struct {
		label string
		path  string
	}{{"solver", filepath.Join(runPath, "log.solver")},
		{"slurm-out", filepath.Join(runPath, fmt.Sprintf("slurm-%d.out", slurmID))},
		{"slurm-err", filepath.Join(runPath, fmt.Sprintf("slurm-%d.err", slurmID))}}
	offsets := make([]int64, len(files))
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			for i, f := range files {
				n, err := tailFromTo(f.path, offsets[i], f.label, out)
				if err == nil {
					offsets[i] = n
				}
			}
		}
	}
}

func tailFrom(path string, offset int64, label string) (int64, error) {
	return tailFromTo(path, offset, label, os.Stderr)
}

func tailFromTo(path string, offset int64, label string, out io.Writer) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return offset, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return offset, err
	}
	if st.Size() < offset {
		offset = 0
	}
	if st.Size() == offset {
		return offset, nil
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return offset, err
	}
	data, err := io.ReadAll(io.LimitReader(f, 4<<20+1))
	if err != nil {
		return offset, err
	}
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		fmt.Fprintf(out, "[%s] %s", label, line)
		if !strings.HasSuffix(line, "\n") {
			fmt.Fprintln(out)
		}
	}
	return offset + int64(len(data)), nil
}

// selectScheduler picks the Slurm backend. "auto" prefers slurmrestd when the
// REST env is fully set and falls back to sbatch/squeue CLI otherwise, which
// is the common case on clusters without slurmrestd.
//
// Token handling is production-minded: SLURM_JWT_FILE (refreshed on every
// request, e.g. `scontrol token lifespan=... > file`) wins over the static
// SLURM_JWT value, and SLURM_SOCKET selects Unix-socket transport.
func selectScheduler(backend string) (slurmrest.Scheduler, string, error) {
	baseURL := os.Getenv("SLURM_REST_URL")
	apiVersion := os.Getenv("SLURM_API_VERSION")
	user := os.Getenv("SLURM_USER")
	socket := os.Getenv("SLURM_SOCKET")
	tokenFn := tokenProvider()
	_, hasStatic := os.LookupEnv("SLURM_JWT")
	_, hasFile := os.LookupEnv("SLURM_JWT_FILE")
	restReady := apiVersion != "" && user != "" && (baseURL != "" || socket != "") && (hasStatic || hasFile)
	switch backend {
	case "rest":
		if !restReady {
			return nil, "", fmt.Errorf("backend=rest requires SLURM_API_VERSION, SLURM_USER, SLURM_JWT or SLURM_JWT_FILE, and SLURM_REST_URL or SLURM_SOCKET")
		}
		c, err := slurmrest.NewClient(slurmrest.Config{
			BaseURL: baseURL, SocketPath: socket, APIVersion: apiVersion, User: user,
			Token: tokenFn,
		})
		if err != nil {
			return nil, "", err
		}
		return c, "rest", nil
	case "cli":
		return &schedcli.CliScheduler{}, "cli", nil
	case "auto", "":
		if restReady {
			c, err := slurmrest.NewClient(slurmrest.Config{
				BaseURL: baseURL, SocketPath: socket, APIVersion: apiVersion, User: user,
				Token: tokenFn,
			})
			if err != nil {
				return nil, "", err
			}
			return c, "rest", nil
		}
		return &schedcli.CliScheduler{}, "cli", nil
	default:
		return nil, "", fmt.Errorf("unknown backend %q (want auto, rest, or cli)", backend)
	}
}

// tokenProvider returns a refreshable JWT supplier. The file is re-read on
// every request so rotated tokens (scontrol token) are picked up without a
// restart; the static env value is the fallback for short-lived setups.
func tokenProvider() func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		if p := strings.TrimSpace(os.Getenv("SLURM_JWT_FILE")); p != "" {
			raw, err := os.ReadFile(p)
			if err != nil {
				return "", fmt.Errorf("read SLURM_JWT_FILE: %w", err)
			}
			if tok := strings.TrimSpace(string(raw)); tok != "" {
				return tok, nil
			}
			return "", fmt.Errorf("SLURM_JWT_FILE %q is empty", p)
		}
		if tok := strings.TrimSpace(os.Getenv("SLURM_JWT")); tok != "" {
			return tok, nil
		}
		return "", fmt.Errorf("no Slurm token: set SLURM_JWT_FILE or SLURM_JWT")
	}
}

// logs resolves a job's log files from the database so no ID ever has to
// be pasted into tail -f by hand:
//
//	sluice logs -manifest <case>/manifest.yaml -latest          # stream newest job
//	sluice logs -manifest <case>/manifest.yaml -id <local-id>   # stream one job
//	tail -f $(sluice logs -manifest <case>/manifest.yaml -latest -follow=false)
//
// logPaths returns every log address for a job: the solver log plus the
// Slurm output and error files Slurm was told to write.
func logPaths(j store.Job) []string {
	return []string{
		filepath.Join(j.RunPath, "log.solver"),
		filepath.Join(j.RunPath, fmt.Sprintf("slurm-%d.out", j.SlurmID)),
		filepath.Join(j.RunPath, fmt.Sprintf("slurm-%d.err", j.SlurmID)),
	}
}

func serveWithConfig(ctx context.Context, addr, dbPath string, _ io.Writer) error {
	st, err := store.Open(dbPath)
	if err != nil {
		return err
	}
	defer st.Close()
	metrics := &telemetry.Metrics{}
	apiToken := strings.TrimSpace(brand.Getenv("API_TOKEN"))
	mux := http.NewServeMux()
	mux.Handle("/metrics", metrics.Handler())
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if _, err := st.List(r.Context()); err != nil {
			http.Error(w, "db not ready", http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ready\n"))
	})
	mux.HandleFunc("/jobs", func(w http.ResponseWriter, r *http.Request) {
		if apiToken != "" && r.Header.Get("Authorization") != "Bearer "+apiToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		jobs, err := st.ListByState(r.Context(), strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("state"))))
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		if jobs == nil {
			jobs = []store.Job{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jobs)
	})
	mux.HandleFunc("/version", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"version": Version})
	})
	srv := &http.Server{
		Addr: addr, Handler: withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	log.Printf(brand.Name+" listening on %s version=%s", addr, Version)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" -manifest manifest.yaml [-db "+brand.DefaultDBFile+"] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" it -manifest manifest.yaml [-db "+brand.DefaultDBFile+"] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" run -manifest manifest.yaml [-db "+brand.DefaultDBFile+"] [-backend auto|rest|cli] [-tail]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" validate -manifest manifest.yaml")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" submit -manifest manifest.yaml [-db "+brand.DefaultDBFile+"] [-backend auto|rest|cli] [-watch]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" watch -db "+brand.DefaultDBFile+" -id <job-id> -slurm <slurm-id>")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" status -db "+brand.DefaultDBFile+" -id <job-id> [-refresh] [-slurm <id>]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" cancel -db "+brand.DefaultDBFile+" -id <job-id> [-slurm <id>]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" list -db "+brand.DefaultDBFile+" [-state DONE]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" logs -manifest manifest.yaml [-id <job-id> | -latest] [-follow=false]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" init -o manifest.yaml -solver foamRun -case ./case")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" serve [-addr :8080] [-db "+brand.DefaultDBFile+"]")
	fmt.Fprintln(os.Stderr, "  "+brand.Name+" version")
}
