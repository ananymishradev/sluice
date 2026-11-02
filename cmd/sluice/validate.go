package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/ananymishradev/sluice/internal/brand"
	"github.com/ananymishradev/sluice/internal/preflight"
	"github.com/ananymishradev/sluice/internal/service"
	"github.com/ananymishradev/sluice/internal/ui"
	"github.com/spf13/cobra"
)

type validateConfig struct {
	Manifest string
	Output   string
}

func newValidateCommand() *cobra.Command {
	cfg := validateConfig{}
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a manifest and its OpenFOAM case",
		Long:  "Validate the manifest, case layout, links, directives, and optional OpenFOAM checks before submission.",
		Example: "  " + brand.Name + " validate --manifest manifest.yaml\n" +
			"  " + brand.Name + " validate --manifest examples/generic.yaml --output json\n" +
			"  " + brand.Name + " validate --manifest manifest.yaml --no-color",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runValidate(cmd.Context(), cfg, cmd.OutOrStdout(), cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVarP(&cfg.Manifest, "manifest", "m", "manifest.yaml", "path to manifest")
	cmd.Flags().StringVarP(&cfg.Output, "output", "o", outputFormat, "output format: table or json")
	_ = cmd.RegisterFlagCompletionFunc("manifest", cobra.FixedCompletions([]string{"manifest.yaml", "examples/generic.yaml"}, cobra.ShellCompDirectiveDefault))
	return cmd
}

func runValidate(ctx context.Context, cfg validateConfig, stdout, stderr io.Writer) error {
	s := service.Service{Preflight: preflight.Runner{}}
	m, report, err := s.Validate(ctx, cfg.Manifest)
	out := struct {
		Manifest any    `json:"manifest"`
		Report   any    `json:"report"`
		Error    string `json:"error,omitempty"`
	}{m, report, ""}
	if err != nil {
		out.Error = err.Error()
	}
	if strings.EqualFold(cfg.Output, "json") {
		b, marshalErr := json.MarshalIndent(out, "", "  ")
		if marshalErr != nil {
			return marshalErr
		}
		_, _ = fmt.Fprintln(stdout, string(b))
		if err != nil {
			return exitError{code: exitUsage, err: fmt.Errorf("invalid: %w", err)}
		}
		return nil
	}

	t := ui.Detect(noColor)
	symbols := ui.SymbolsFor(t)
	if err != nil {
		ui.Error(stderr, "validation failed", err, "fix the manifest or case findings, then run validate again", t.Color)
		return exitError{code: exitUsage, err: fmt.Errorf("invalid: %w", err)}
	}
	name := m.Name
	if name == "" {
		name = filepath.Base(m.Case.Path)
	}
	fmt.Fprintf(stderr, "%s manifest     %s · %s\n", symbols.OK, name, m.APIVersion)
	fmt.Fprintf(stderr, "%s layout       0/, 0.orig/, or initial/ · constant/ · system/\n", symbols.OK)
	fmt.Fprintf(stderr, "%s symlinks     no escaping symlinks\n", symbols.OK)
	fmt.Fprintf(stderr, "%s directives   no blocked code-loading directives\n", symbols.OK)
	for _, note := range report.Notes {
		fmt.Fprintf(stderr, "%s %s: %s\n", symbols.OK, note.Path, note.Message)
	}
	if len(report.Warnings) == 0 {
		fmt.Fprintf(stderr, "%s checks       no warnings\n", symbols.OK)
	} else {
		fmt.Fprintf(stderr, "%s checks       %d warning(s)\n", symbols.Warn, len(report.Warnings))
		for _, finding := range report.Warnings {
			fmt.Fprintf(stderr, "  %s %s: %s\n", symbols.Warn, finding.Path, finding.Message)
		}
	}
	return nil
}
