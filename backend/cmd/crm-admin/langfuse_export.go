package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"personal-crm/backend/internal/langfuseexport"
)

// langfuseExporter is the narrow interface --langfuse-export needs.
// Production wires it to a *langfuseexport.Exporter.
type langfuseExporter interface {
	Export(ctx context.Context) (langfuseexport.Report, error)
}

// runLangfuseExport sends the decision-model records made since the previous
// export to the Langfuse extraction project and prints the content-free
// counts, or that nothing was sent because the project is not configured.
func runLangfuseExport(ctx context.Context, deps adminDeps) error {
	r, err := deps.langfuseExport.Export(ctx)
	if err != nil {
		return fmt.Errorf("langfuse export: %w", err)
	}
	return writeLangfuseExport(deps.stdout, r)
}

func writeLangfuseExport(w io.Writer, r langfuseexport.Report) error {
	var err error
	if !r.Sent() {
		_, err = fmt.Fprintf(w, "langfuse-export: extraction project not configured (unset: %s); nothing sent\n",
			strings.Join(r.Missing, ", "))
	} else {
		_, err = fmt.Fprintf(w, "langfuse-export: runs=%d runs_already_sent=%d judgments=%d scorecards=%d reviews=%d\n",
			r.Runs, r.Skipped, r.Judgments, r.Scorecards, r.Reviews)
	}
	if err != nil {
		return fmt.Errorf("write langfuse export report: %w", err)
	}
	return nil
}
