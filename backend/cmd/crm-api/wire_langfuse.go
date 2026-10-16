package main

import (
	"time"

	"personal-crm/backend/internal/config"
	"personal-crm/backend/internal/judgment"
	"personal-crm/backend/internal/langfuseexport"
	"personal-crm/backend/internal/scorecard"

	"github.com/riverqueue/river"
)

// langfuseExportInterval is how often the langfuse_export_nightly periodic job
// runs.
const langfuseExportInterval = 24 * time.Hour

// registerLangfuseExport registers the langfuse_export_nightly periodic job
// (and its worker), which sends the judgment store's content-free records to
// the Langfuse extraction project. Unconditional: with the extraction
// project's settings unset, each run sends nothing and logs so. Called from
// run() AND from buildWireChainForGolden (golden-list pinned).
func registerLangfuseExport(reg *riverRegistrar, cfg *config.Config, judgments *judgment.Store) {
	exporter := langfuseexport.New(cfg.LangfuseExtraction, judgments, scorecard.NewReader(judgments))
	addWorker(reg, langfuseexport.NewWorker(exporter))
	reg.addPeriodic(langfuseexport.NightlyArgs{}.Kind(), river.NewPeriodicJob(
		river.PeriodicInterval(langfuseExportInterval),
		func() (river.JobArgs, *river.InsertOpts) { return langfuseexport.NightlyArgs{}, nil },
		&river.PeriodicJobOpts{RunOnStart: true},
	))
}
