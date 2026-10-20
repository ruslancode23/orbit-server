// Package ratelimit is a per-account token bucket held in the process. It costs
// nothing to consult and needs no shared storage: one instance serves one
// process, and the limit is about how often one account may start something,
// not about the total load of the cluster.
package ratelimit

import (
	"math"
	"sync"
	"time"

	"github.com/google/uuid"
)

// idleFor is when a full bucket is forgotten: the account has not spent a token
// for long enough that its map entry is not worth keeping.
const idleFor = 10 * time.Minute

// Limiter hands out tokens to accounts. A nil *Limiter allows everything, so a
// caller that does not need a limit can leave the field unset.
type Limiter struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	now       func() time.Time
	buckets   map[uuid.UUID]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New builds a limiter that allows burst operations at once and then refills
// one token every minute/burst. A limit of zero or less disables it, which is
// how an operator turns the guard off.
func New(perMinute int) *Limiter {
	if perMinute <= 0 {
		return nil
	}
	return &Limiter{
		perSecond: float64(perMinute) / 60,
		burst:     float64(perMinute),
		now:       time.Now,
		buckets:   make(map[uuid.UUID]*bucket),
	}
}

// NewDaily builds a limiter that allows perDay operations at once and then
// refills one token every 24h/perDay. It is a daily budget rather than a
// calendar-day counter: a burst spends the budget early and pays for it with a
// longer wait, which is exactly what Retry-After needs to say.
func NewDaily(perDay int) *Limiter {
	if perDay <= 0 {
		return nil
	}
	return &Limiter{
		perSecond: float64(perDay) / (24 * 60 * 60),
		burst:     float64(perDay),
		now:       time.Now,
		buckets:   make(map[uuid.UUID]*bucket),
	}
}

// Allow takes one token for the account. When there is none left it reports how
// long the caller should wait before asking again; the HTTP layer puts that in
// Retry-After.
func (l *Limiter) Allow(userID uuid.UUID) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[userID]
	if !ok {
		if len(l.buckets) > 1024 {
			l.forgetIdle(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[userID] = b
	}

	// Refill by elapsed time, capped at the burst: an idle bucket does not grow
	// beyond what one moment is allowed to spend.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.perSecond)
		b.last = now
	}
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	wait := time.Duration(math.Ceil((1-b.tokens)/l.perSecond)) * time.Second
	if wait < time.Second {
		wait = time.Second
	}
	return false, wait
}

// forgetIdle drops the buckets that have been full for idleFor; it runs only
// when the map has grown, so an ordinary process sweeps rarely.
func (l *Limiter) forgetIdle(now time.Time) {
	for id, b := range l.buckets {
		if now.Sub(b.last) >= idleFor {
			delete(l.buckets, id)
		}
	}
}
