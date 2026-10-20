package secrets

import (
	"context"
	"log/slog"
	"time"
)

const (
	// sweepEvery is how often expired envelopes are removed. A message that was
	// never picked up should not outlive its week by much.
	sweepEvery = 5 * time.Minute
)

// Sweeper removes envelopes whose deadline passed before anyone collected them.
// Delivery deletes an envelope; this is what covers the recipient who never
// came back.
type Sweeper struct {
	svc *Service
	log *slog.Logger
}

func NewSweeper(svc *Service, log *slog.Logger) *Sweeper {
	return &Sweeper{svc: svc, log: log}
}

// Run sweeps until the context ends. Meant to be started in a goroutine at boot.
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
	removed, err := s.svc.SweepExpired(ctx)
	if err != nil {
		s.log.ErrorContext(ctx, "sweep expired envelopes", slog.Any("error", err))
		return
	}
	if removed > 0 {
		s.log.InfoContext(ctx, "swept expired envelopes", slog.Int64("removed", removed))
	}
}
