// Package ratelimit is a small token bucket. All agents share one bucket per
// LLM provider, so three agents can never exceed the free-tier requests/minute.
package ratelimit

import (
	"context"
	"sync"
	"time"
)

type Limiter struct {
	mu     sync.Mutex
	tokens float64
	burst  float64
	rate   float64 // tokens per second
	last   time.Time
}

// New allows `perMinute` requests per minute with a small burst.
func New(perMinute int) *Limiter {
	if perMinute < 1 {
		perMinute = 1
	}
	burst := float64(perMinute) / 6
	if burst < 1 {
		burst = 1
	}
	return &Limiter{tokens: burst, burst: burst, rate: float64(perMinute) / 60, last: time.Now()}
}

// Wait blocks until a token is available or ctx is done. It returns how long it waited.
func (l *Limiter) Wait(ctx context.Context) (time.Duration, error) {
	start := time.Now()
	for {
		l.mu.Lock()
		now := time.Now()
		l.tokens += now.Sub(l.last).Seconds() * l.rate
		if l.tokens > l.burst {
			l.tokens = l.burst
		}
		l.last = now
		if l.tokens >= 1 {
			l.tokens--
			l.mu.Unlock()
			return time.Since(start), nil
		}
		need := time.Duration((1 - l.tokens) / l.rate * float64(time.Second))
		l.mu.Unlock()

		select {
		case <-ctx.Done():
			return time.Since(start), ctx.Err()
		case <-time.After(need):
		}
	}
}
