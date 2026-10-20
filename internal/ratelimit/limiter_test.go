package ratelimit

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestLimiterAllowsBurstThenRefills(t *testing.T) {
	limiter := New(2)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }
	anna := uuid.New()

	if allowed, _ := limiter.Allow(anna); !allowed {
		t.Fatal("the first call was refused")
	}
	if allowed, _ := limiter.Allow(anna); !allowed {
		t.Fatal("the second call of the burst was refused")
	}
	allowed, retryAfter := limiter.Allow(anna)
	if allowed {
		t.Fatal("the third call passed a burst of two")
	}
	if retryAfter <= 0 || retryAfter > time.Minute {
		t.Errorf("RetryAfter = %v, want a positive wait within a minute", retryAfter)
	}

	// One token refills in half a minute when the limit is two per minute.
	clock = clock.Add(30 * time.Second)
	if allowed, _ := limiter.Allow(anna); !allowed {
		t.Error("no token after half a minute")
	}
}

func TestLimiterCountsEachAccountSeparately(t *testing.T) {
	limiter := New(1)
	anna, bob := uuid.New(), uuid.New()

	if allowed, _ := limiter.Allow(anna); !allowed {
		t.Fatal("Anna's first call was refused")
	}
	if allowed, _ := limiter.Allow(bob); !allowed {
		t.Error("Bob was refused because Anna spent her token")
	}
}

func TestZeroLimitDisablesTheGuard(t *testing.T) {
	limiter := New(0)
	if limiter != nil {
		t.Fatalf("New(0) = %v, want nil", limiter)
	}
	for range 100 {
		if allowed, _ := limiter.Allow(uuid.New()); !allowed {
			t.Fatal("a disabled limiter refused a call")
		}
	}
}

func TestIdleBucketsAreForgotten(t *testing.T) {
	limiter := New(1)
	clock := time.Now()
	limiter.now = func() time.Time { return clock }
	anna := uuid.New()

	if allowed, _ := limiter.Allow(anna); !allowed {
		t.Fatal("the first call was refused")
	}

	// Grow the map past the sweep threshold, then let one account idle.
	for range 1100 {
		limiter.Allow(uuid.New())
	}
	clock = clock.Add(idleFor + time.Minute)
	limiter.mu.Lock()
	limiter.forgetIdle(clock)
	_, kept := limiter.buckets[anna]
	limiter.mu.Unlock()
	if kept {
		t.Error("an idle bucket survived the sweep")
	}
}
