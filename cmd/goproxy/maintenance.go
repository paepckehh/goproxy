package main

import (
	"context"
	"log/slog"
	"time"

	"paepcke.de/goproxy/internal/history"
	"paepcke.de/goproxy/internal/stats"
)

const (
	// catchUpDelay lets the proxy settle after a start and finish the
	// startup index scan before a missed window runs.
	catchUpDelay = 5 * time.Minute
	// maintenanceTimeout bounds one window; a stuck VACUUM must not keep
	// the goroutine (and shutdown) waiting forever.
	maintenanceTimeout = 30 * time.Minute
)

// runMaintenance owns the nightly 23:00 local-time window. A start after a
// missed window catches up once, then the loop sleeps to the next 23:00.
func runMaintenance(ctx context.Context, hist *history.DB, m *stats.Stats, log *slog.Logger, version string) {
	if hist.MaintenanceDue(time.Now()) {
		log.Info("stats maintenance due: running the missed nightly window",
			"in", catchUpDelay.String())
		timer := time.NewTimer(catchUpDelay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		runMaintenanceNow(ctx, hist, m, log, version)
	}
	for {
		next := history.NextNightly(time.Now())
		log.Info("next stats maintenance", "at", next.In(time.Local).Format(time.RFC3339))
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		runMaintenanceNow(ctx, hist, m, log, version)
	}
}

// runMaintenanceNow runs one window and surfaces the result in the journal
// and the dashboard event feed.
func runMaintenanceNow(ctx context.Context, hist *history.DB, m *stats.Stats, log *slog.Logger, version string) {
	ctx, cancel := context.WithTimeout(ctx, maintenanceTimeout)
	defer cancel()
	rep, err := hist.Nightly(ctx, time.Now(), version)
	if err != nil {
		log.Error("stats maintenance failed", "err", err)
		m.Maintenance("nightly stats maintenance failed: " + err.Error())
		return
	}
	log.Info("stats maintenance complete",
		"summary", rep.Summary(),
		"backup", rep.Backup,
		"backup_bytes", rep.BackupSize,
		"generations", rep.Backups,
		"check_ok", rep.Check.OK)
	m.Maintenance(rep.Summary())
}
