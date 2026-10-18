package files

import (
	"context"
	"log/slog"
	"time"
)

const (
	// sweepBatch caps one pass, so a backlog is cleared over several runs
	// instead of one long transaction.
	sweepBatch = 100
	// sweepEvery is how often the sweep runs.
	sweepEvery = time.Hour
)

// Sweeper deletes files whose message never arrived: someone picked a file,
// then closed the tab. Without it the store fills with bytes nobody can reach.
type Sweeper struct {
	svc       *Service
	orphanAge time.Duration
	log       *slog.Logger
}

func NewSweeper(svc *Service, orphanAge time.Duration, log *slog.Logger) *Sweeper {
	return &Sweeper{svc: svc, orphanAge: orphanAge, log: log}
}

// Run sweeps every hour until the context ends. It is meant to be started in a
// goroutine at boot; river takes this over when background jobs arrive (PRD 11.2).
func (s *Sweeper) Run(ctx context.Context) {
	ticker := time.NewTicker(sweepEvery)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.sweep(ctx)
		}
	}
}

func (s *Sweeper) sweep(ctx context.Context) {
	removed, err := s.svc.SweepOrphans(ctx, s.orphanAge, sweepBatch)
	if err != nil {
		s.log.ErrorContext(ctx, "sweep orphan attachments",
			slog.Int("removed", removed), slog.Any("error", err))
		return
	}
	if removed > 0 {
		s.log.InfoContext(ctx, "swept orphan attachments", slog.Int("removed", removed))
	}
}
