// Package ratelimit is a token bucket per key held in the process. It costs
// nothing to consult and needs no shared storage: one instance serves one
// process, and the limit is about how often one account — or one address, or
// one login name — may do something, not about the total load of the cluster.
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

// Limiter hands out tokens per key: an account id, a client address, a login. A nil *Limiter allows everything, so a
// caller that does not need a limit can leave the field unset.
type Limiter struct {
	mu        sync.Mutex
	perSecond float64
	burst     float64
	now       func() time.Time
	buckets   map[string]*bucket
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
		buckets:   make(map[string]*bucket),
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
		buckets:   make(map[string]*bucket),
	}
}

// NewHourly builds a limiter that allows perHour operations at once and then
// refills one token every hour/perHour. It is the same token bucket as the daily
// one, for a budget that is small but needs a shorter Retry-After.
func NewHourly(perHour int) *Limiter {
	if perHour <= 0 {
		return nil
	}
	return &Limiter{
		perSecond: float64(perHour) / (60 * 60),
		burst:     float64(perHour),
		now:       time.Now,
		buckets:   make(map[string]*bucket),
	}
}

// Allow takes one token for the account. When there is none left it reports how
// long the caller should wait before asking again; the HTTP layer puts that in
// Retry-After.
func (l *Limiter) Allow(userID uuid.UUID) (bool, time.Duration) {
	return l.AllowKey(userID.String())
}

// AllowKey is Allow for any key: a client address or a login name. Keys of
// different kinds belong in different limiters, so they never share a budget.
func (l *Limiter) AllowKey(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refilled(key, l.now())
	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	return false, l.waitFor(b)
}

// Check reports whether a token is there without taking it. It serves a limit
// that only failures spend: the caller checks before the attempt and calls
// AllowKey when the attempt failed, so success costs nothing.
func (l *Limiter) Check(key string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b := l.refilled(key, l.now())
	if b.tokens >= 1 {
		return true, 0
	}
	return false, l.waitFor(b)
}

// refilled returns the bucket of the key with the tokens it earned since it was
// last touched. The caller holds the lock.
func (l *Limiter) refilled(key string, now time.Time) *bucket {
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) > 1024 {
			l.forgetIdle(now)
		}
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}

	// Refill by elapsed time, capped at the burst: an idle bucket does not grow
	// beyond what one moment is allowed to spend.
	if elapsed := now.Sub(b.last).Seconds(); elapsed > 0 {
		b.tokens = math.Min(l.burst, b.tokens+elapsed*l.perSecond)
		b.last = now
	}
	return b
}

// waitFor is how long until the bucket holds one whole token again.
func (l *Limiter) waitFor(b *bucket) time.Duration {
	wait := time.Duration(math.Ceil((1-b.tokens)/l.perSecond)) * time.Second
	if wait < time.Second {
		wait = time.Second
	}
	return wait
}

// forgetIdle drops the buckets that have been full for idleFor; it runs only
// when the map has grown, so an ordinary process sweeps rarely. A bucket that
// has not refilled yet is kept: dropping it would hand the account a fresh
// budget and quietly defeat the limit.
func (l *Limiter) forgetIdle(now time.Time) {
	for id, b := range l.buckets {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed >= idleFor.Seconds() && b.tokens+elapsed*l.perSecond >= l.burst {
			delete(l.buckets, id)
		}
	}
}

// Seconds is a wait as Retry-After states it: whole seconds, rounded up, at
// least one.
func Seconds(wait time.Duration) int {
	seconds := int((wait + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}
