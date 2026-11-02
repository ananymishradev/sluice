package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/ananymishradev/sluice/internal/brand"
	"github.com/ananymishradev/sluice/internal/service"
	"github.com/ananymishradev/sluice/internal/ui"
	"github.com/spf13/cobra"
)

const (
	exitOK = iota
	exitJob
	exitUsage
	exitEnvironment
)

var (
	outputFormat string
	noColor      bool
)

type cliObserver struct{}

func (cliObserver) Stage(name string, status service.StageStatus, detail string) {
	if status != service.StageDone {
		return
	}
	t := ui.Detect(noColor)
	ui.Progress{Out: os.Stderr, Term: t, Symbol: ui.SymbolsFor(t)}.Timestamped(name, detail)
}

var legacyFlags = map[string]bool{
	"manifest": true, "db": true, "backend": true, "tail": true, "watch": true,
	"latest": true, "id": true, "slurm": true, "state": true, "refresh": true,
	"follow": true, "interval": true, "timeout": true, "addr": true,
	"output": true, "yes": true, "limit": true, "solver": true, "case": true,
	"name": true, "partition": true, "nodes": true, "tasks": true, "time": true,
	"checkpoint": true, "no-color": true,
}

type exitError struct {
	code int
	err  error
}

func (e exitError) Error() string { return e.err.Error() }
func (e exitError) Unwrap() error { return e.err }

func executeRoot() int {
	os.Args = rewriteLegacyArgs(os.Args)
	root := newRoot()
	if err := root.Execute(); err != nil {
		t := ui.Detect(noColor)
		ui.Error(os.Stderr, "command failed", err, "run `"+brand.Name+" --help` to see available commands", t.Color)
		var coded exitError
		if errors.As(err, &coded) {
			return coded.code
		}
		return exitUsage
	}
	return exitOK
}

func rewriteLegacyArgs(args []string) []string {
	out := append([]string(nil), args...)
	changed := false
	for i := 1; i < len(out); i++ {
		if strings.HasPrefix(out[i], "-") && !strings.HasPrefix(out[i], "--") {
			name := strings.TrimPrefix(out[i], "-")
			suffix := ""
			if key, value, ok := strings.Cut(name, "="); ok {
				name = key
				suffix = "=" + value
			}
			if legacyFlags[name] {
				out[i] = "--" + name + suffix
				changed = true
			}
		}
	}
	if changed && isTTY(os.Stderr) {
		fmt.Fprintln(os.Stderr, "note: single-dash long flags are deprecated; use --double-dash flags")
	}
	if len(out) > 1 && (out[1] == "--manifest" || out[1] == "--db" || out[1] == "--backend" || out[1] == "--tail") {
		out = append([]string{out[0], "run"}, out[1:]...)
	}
	return out
}

func isTTY(f *os.File) bool {
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

func newRoot() *cobra.Command {
	root := &cobra.Command{
		Use:           brand.Name,
		Short:         "Run OpenFOAM cases on Slurm",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().StringVarP(&outputFormat, "output", "o", "table", "output format: table or json")
	root.PersistentFlags().BoolVar(&noColor, "no-color", false, "disable terminal colors")
	root.SetHelpFunc(func(cmd *cobra.Command, args []string) {
		t := ui.Detect(noColor)
		if t.TTY {
			fmt.Fprint(os.Stderr, ui.Banner(Version, t.Color))
		}
		_ = cmd.Usage()
	})

	root.AddCommand(
		newValidateCommand(),
		newSubmitCommand(),
		newWatchCommand(),
		newStatusCommand(),
		newCancelCommand(),
		newLogsCommand(),
		newServeCommand(),
		newRunCommand(),
		newListCommand(),
		newDoctorCommand(),
		newInitCommand(),
		newVersionCommand(),
		newUpdateCommand(),
	)
	return root
}

func passthrough(name string, fn func(context.Context, []string) error) *cobra.Command {
	return &cobra.Command{
		Use: name, DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return fn(ctx, args)
		},
	}
}

func newRunCommand() *cobra.Command {
	return &cobra.Command{
		Use: "run", Aliases: []string{"it"}, DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := context.Background()
			return run(ctx, args)
		},
	}
}
