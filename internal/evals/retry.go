package evals

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Retry backoff bounds.
//
// Capped rather than doubling without limit: the useful question during an
// outage is "is quota back yet", and past a minute or two the answer does not
// get more informative by waiting longer — it just spends the budget in fewer,
// coarser samples.
const (
	RetryBase = 15 * time.Second
	RetryCap  = 90 * time.Second
)

// RetryPolicy runs one example, retrying through provider back-pressure.
//
// Both eval tiers need this and for the same reason: a 429 says nothing about
// the agent, but an example that dies on one is dropped from the aggregate, and
// because quota exhaustion arrives in bursts the drops are correlated rather
// than random. A run that silently loses nine examples to one bad minute is not
// a measurement of the agent.
//
// Only rate limiting is retried. A malformed request or a schema violation is a
// real defect and must surface as a failure.
//
// # Patience is wall clock, not attempts
//
// An attempt cap sounds equivalent and is not. With exponential backoff, the
// old "4 retries" was 3m45s of patience; the first full tier-2 run met a quota
// outage lasting over ten minutes, every fixture exhausted its retries, and 6
// of 7 were lost. Wall clock is what the caller actually wants to bound —
// especially on tier 2, where each loss also wastes a cluster build and a fault
// injection that already succeeded.
type RetryPolicy struct {
	// Timeout bounds a single attempt.
	Timeout time.Duration
	// For is the total wall-clock patience across all attempts. Zero means no
	// retrying at all.
	For time.Duration
	// OnRetry, if set, is called before each sleep.
	OnRetry func(wait, budgetLeft time.Duration)
}

// Run executes one example under the policy.
func (p RetryPolicy) Run(ctx context.Context, runner *Runner, id string, ex Example) (Run, error) {
	return p.Do(ctx, func(ctx context.Context) (Run, error) { return runner.Run(ctx, id, ex) })
}

// Do applies the policy to any producer of a Run.
//
// It exists because the bounded pass (internal/bounded) is a second producer
// and needs the same retry semantics, both halves of which cost a live run to
// learn: patience is wall clock rather than an attempt count, because with
// exponential backoff `-retries 4` bought 3m45s against a quota outage lasting
// over an hour and lost six of seven fixtures; and the backoff is jittered,
// because three workers sharing a quota and backing off deterministically
// synchronize and stay synchronized. Reimplementing either at a second call
// site would mean the second producer quietly gets the policy that failed.
func (p RetryPolicy) Do(ctx context.Context, fn func(context.Context) (Run, error)) (Run, error) {
	deadline := time.Now().Add(p.For)
	for attempt := 0; ; attempt++ {
		got, err := func() (Run, error) {
			exCtx, cancel := context.WithTimeout(ctx, p.Timeout)
			defer cancel()
			return fn(exCtx)
		}()
		if err == nil || !IsRateLimit(err) {
			return got, err
		}
		wait := Backoff(attempt)
		if time.Now().Add(wait).After(deadline) {
			return got, fmt.Errorf("rate limited for %s, giving up: %w", p.For, err)
		}
		if p.OnRetry != nil {
			p.OnRetry(wait, time.Until(deadline))
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return Run{}, ctx.Err()
		}
	}
}

// Backoff is capped exponential with +/-25% jitter.
//
// The jitter is not decoration. Concurrent workers that share a quota and back
// off deterministically synchronize and stay synchronized: the first tier-2 run
// shows three fixtures marching through 15s/30s/60s/120s in lockstep, so every
// retry re-collided with the other two. An earlier fix offset each worker by a
// fixed i%7 seconds, which spreads at most 6s across a 120s backoff — enough to
// look addressed, not enough to decorrelate.
// RetryCap bounds the jittered result, so the base it is applied to has to
// leave room for the +25% half of the jitter. Clamping the result instead would
// be simpler and worse: with a base at the cap, half of all draws would land on
// exactly the cap, which is a mass point that re-synchronizes the workers the
// jitter exists to spread.
func Backoff(attempt int) time.Duration {
	base := RetryCap * 4 / 5
	if attempt < 8 { // RetryBase<<8 is past the cap anyway; guard the shift.
		if w := RetryBase << attempt; w < base {
			base = w
		}
	}
	return base - base/4 + time.Duration(rand.Int64N(int64(base)/2))
}

// rateLimitMarkers are matched against the error message because the status is
// wrapped by the workflow, the agent, and the provider before it reaches us,
// with no typed error preserved.
//
// overloaded_error is the one that is easy to miss: Vertex returns it inside a
// streamed body under an HTTP 200, so it fails no status check while still
// meaning "come back later".
var rateLimitMarkers = []string{
	"429",
	"Too Many Requests",
	"RESOURCE_EXHAUSTED",
	"overloaded_error",
	"Overloaded",
}

// IsRateLimit reports whether an error is provider back-pressure.
func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	for _, marker := range rateLimitMarkers {
		if strings.Contains(s, marker) {
			return true
		}
	}
	return false
}
