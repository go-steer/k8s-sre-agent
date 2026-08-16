package evals

import (
	"errors"
	"testing"
	"time"
)

func TestIsRateLimit(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"429", errors.New(`stream: POST ".../v1/messages": 429 Too Many Requests [{`), true},
		{"resource exhausted", errors.New("rpc error: code = RESOURCE_EXHAUSTED"), true},

		// The one that passes every status check: HTTP 200, error in the body.
		{"overloaded under 200", errors.New(
			`stream: POST ".../claude-sonnet-5:streamRawPredict": 200 OK ` +
				`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`), true},

		// Real defects must surface, not spin.
		{"schema violation", errors.New("output did not match schema: missing field severity"), false},
		{"bad request", errors.New(`400 Bad Request: unknown field "tool_choice"`), false},
		{"timeout", errors.New("context deadline exceeded"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRateLimit(tc.err); got != tc.want {
				t.Errorf("IsRateLimit(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// Backoff must stay inside the cap and must actually vary. A deterministic
// backoff is what let three concurrent fixtures retry in lockstep and
// re-collide on every attempt.
func TestBackoffIsCappedAndJittered(t *testing.T) {
	for attempt := 0; attempt < 12; attempt++ {
		seen := map[time.Duration]bool{}
		for i := 0; i < 200; i++ {
			w := Backoff(attempt)
			if w <= 0 {
				t.Fatalf("attempt %d produced a non-positive wait %s", attempt, w)
			}
			if w > RetryCap {
				t.Fatalf("attempt %d produced %s, past the %s cap", attempt, w, RetryCap)
			}
			seen[w] = true
		}
		if len(seen) < 10 {
			t.Errorf("attempt %d produced only %d distinct waits across 200 draws; "+
				"backoff is effectively deterministic and will synchronize concurrent workers",
				attempt, len(seen))
		}
	}
}

// The spread has to be wide enough to actually decorrelate two workers that
// collided on the same quota window. The superseded fix offset workers by a
// fixed i%7 seconds, which is 6s against a 120s backoff.
func TestBackoffSpreadIsWideEnoughToDecorrelate(t *testing.T) {
	const attempt = 4 // past the cap, where the old scheme was weakest
	min, max := time.Hour, time.Duration(0)
	for i := 0; i < 500; i++ {
		w := Backoff(attempt)
		if w < min {
			min = w
		}
		if w > max {
			max = w
		}
	}
	if spread := max - min; spread < RetryCap/4 {
		t.Errorf("spread %s is under a quarter of the %s cap; workers sharing a quota "+
			"window will stay synchronized", spread, RetryCap)
	}
}
