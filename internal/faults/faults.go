// Package faults defines the cluster faults the live eval tier injects.
//
// # What this tier measures that tier 1 cannot
//
// Tier 1 hands the agent a prose description of a broken cluster and grades
// the narration. That is a real measurement — tool selection and severity
// judgement both show up in it — but it cannot distinguish an agent that
// diagnoses from an agent that paraphrases, because the diagnosis is in the
// prompt.
//
// Here the prompt says only which namespace to look at. The fault is in the
// cluster, not in the text, so a wrong tool choice produces a wrong answer
// instead of a lower coverage score.
//
// # Why the expectations are shaped the way they are
//
// Each fault declares the findings a correct report must contain, keyed on
// Kind + ResourceName + Reason — the machine-stable triple schema.Finding
// exists to carry. Title and Detail are deliberately not graded: they are
// prose, and grading prose needs a judge, which is what tier 1 already has.
//
// Reasons are matched as a set of acceptable tokens rather than one string.
// "ImagePullBackOff" and "ErrImagePull" are the same fault observed a few
// seconds apart, and an agent is not wrong for naming the one kubectl showed
// it.
package faults

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-steer/k8s-sre-agent/internal/kindcluster"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// BaseImage is the workload image every fixture uses.
//
// One image for all fixtures, preloaded into the node with `kind load`, so
// that no fixture depends on registry reachability at eval time — except the
// image-pull fixture, whose whole point is a reference that cannot resolve.
const BaseImage = "busybox:1.36"

// MissingImage is a reference that cannot resolve anywhere. The registry host
// is real so the failure is an authentication/not-found error rather than a
// DNS failure, which is the shape a typo'd tag has in production.
const MissingImage = "ghcr.io/go-steer/no-such-image:v0.0.0-does-not-exist"

// Fault is one injected cluster fault and what a correct report must say
// about it.
type Fault struct {
	// Name identifies the fixture and doubles as its namespace.
	Name string

	// Prompt is what the agent is asked. It must describe the *task*, never
	// the fault: "check namespace X" is the whole point of this tier. A prompt
	// that leaks the diagnosis turns tier 2 back into tier 1.
	Prompt string

	// Manifest is the YAML applied to inject the fault.
	Manifest string

	// Settle are the conditions that must hold before the agent is allowed to
	// look. Injection is not instantaneous — a CrashLoopBackOff takes two
	// restarts and a backoff to exist at all — and an agent that looks too
	// early correctly reports a healthy cluster and is then scored wrong.
	Settle []Condition

	// Want are the findings a correct report must contain.
	Want []Want

	// WantSeverity is the report-level severity a correct report carries.
	// Graded with the same one-directional tolerance tier 1 uses.
	WantSeverity schema.OverallSeverity
}

// Want is one required finding, matched on the machine-stable triple.
type Want struct {
	// Kind is the Kubernetes kind, as schema.Finding spells it.
	Kind string

	// Name is the affected object. Deployment-owned pods get a generated
	// suffix, so this is matched as a prefix — see Want.MatchesName.
	Name string

	// Reasons are the acceptable Reason tokens; any one of them counts. Empty
	// means the reason is not graded and Kind+Name alone must match.
	Reasons []string

	// AlsoAcceptKinds lets a finding about the controller stand in for one
	// about the pod, or vice versa. An agent that reports "Deployment web is
	// unavailable" instead of "Pod web-abc123 is Pending" has found the same
	// fault and named the object an operator would act on.
	AlsoAcceptKinds []string

	// MinSeverity is the least severe this finding may be graded correct at.
	// Empty means severity is not graded for this finding.
	MinSeverity schema.Severity

	// Root marks this Want as the fault's root cause, on a fixture where some
	// other Want is its downstream symptom.
	//
	// Recall alone cannot express the difference. A fixture with a cause and a
	// symptom has two Wants, and an agent that reports only the symptom scores
	// 0.5 — the same as one that reports only the cause, which is a far better
	// answer: the symptom is what an operator sees and the cause is what they
	// have to fix. evals.RootCause grades that separately.
	//
	// Left false wherever the fixture does not grade both halves. On a
	// single-Want fixture — which is every one of the original six, and
	// fault-ledger — marking the one Want root makes root_cause an exact copy of
	// recall, and nine copies of recall would bury the one fixture where the
	// distinction is real. The evaluator skips a fixture that names no root.
	Root bool
}

// Condition is a poll that must pass before the fault counts as injected.
type Condition struct {
	// Describe is what is being waited for, used in timeout messages.
	Describe string
	// Args are passed to kubectl.
	Args []string
	// Contains must appear in the output. Ignored when Empty is set.
	Contains string
	// Empty requires the output to be blank instead. Needed for the faults
	// whose signature is an *absence* — a Service with no endpoints has no
	// string to match on, and `Contains: ""` would be a condition that passes
	// unconditionally while looking like a check.
	Empty bool
}

// satisfied reports whether a poll's output meets the condition.
func (c Condition) satisfied(out string) bool {
	if c.Empty {
		return strings.TrimSpace(out) == ""
	}
	return strings.Contains(out, c.Contains)
}

// Namespace is the namespace a fault is injected into. One namespace per
// fault: the faults are independent incidents, and sharing a namespace would
// let one fixture's broken pod count as another's finding.
func (f Fault) Namespace() string { return f.Name }

// MatchesName reports whether a reported resource name identifies this Want's
// object.
//
// Prefix matching in one direction only: a Want naming the Deployment "web"
// is satisfied by a finding about pod "web-5d8f9c-x2k9", because that pod is
// the deployment's. The reverse is not accepted — a Want naming a specific
// pod is not satisfied by a finding about some other pod that happens to
// share a prefix.
func (w Want) MatchesName(got string) bool {
	got = strings.TrimSpace(got)
	if got == "" {
		return false
	}
	if got == w.Name {
		return true
	}
	return strings.HasPrefix(got, w.Name+"-")
}

// MatchesKind reports whether a reported kind is one this Want accepts.
// Compared case-insensitively: the schema asks for "Pod", and a model that
// writes "pod" has not made a diagnostic error.
func (w Want) MatchesKind(got string) bool {
	if strings.EqualFold(got, w.Kind) {
		return true
	}
	for _, k := range w.AlsoAcceptKinds {
		if strings.EqualFold(got, k) {
			return true
		}
	}
	return false
}

// MatchesReason reports whether a reported reason is acceptable.
//
// Substring, case-insensitively, in both directions: models write
// "CrashLoopBackOff", "CrashLoop", and "crash_loop_backoff" for the same
// state, and the reason field is graded to check that the agent identified
// the failure mode, not that it matched our spelling of it.
func (w Want) MatchesReason(got string) bool {
	if len(w.Reasons) == 0 {
		return true
	}
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(s))
	}
	g := norm(got)
	if g == "" {
		return false
	}
	for _, r := range w.Reasons {
		if n := norm(r); strings.Contains(g, n) || strings.Contains(n, g) {
			return true
		}
	}
	return false
}

// Matches reports whether a finding satisfies this Want.
func (w Want) Matches(f schema.Finding) bool {
	if !w.MatchesKind(f.Kind) || !w.MatchesName(f.ResourceName) {
		return false
	}
	if !w.MatchesReason(f.Reason) {
		return false
	}
	if w.MinSeverity != "" && !f.Severity.Overall().AtLeast(w.MinSeverity.Overall()) {
		return false
	}
	return true
}

// Inject applies the fault and waits for it to actually manifest.
func (f Fault) Inject(ctx context.Context, c *kindcluster.Cluster, timeout time.Duration) error {
	ns := fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: %s\n", f.Namespace())
	if out, err := c.Apply(ctx, ns); err != nil {
		return fmt.Errorf("faults: create namespace %s: %w\n%s", f.Namespace(), err, out)
	}
	if out, err := c.Apply(ctx, f.Manifest); err != nil {
		return fmt.Errorf("faults: apply %s: %w\n%s", f.Name, err, out)
	}
	return f.Settled(ctx, c, timeout)
}

// Settled blocks until every Settle condition holds, or timeout elapses.
//
// A timeout here is an infrastructure failure, not an agent failure, and the
// caller must report it as such — scoring an agent against a fault that never
// materialized measures nothing.
func (f Fault) Settled(ctx context.Context, c *kindcluster.Cluster, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for _, cond := range f.Settle {
		if err := waitFor(ctx, c, cond, deadline); err != nil {
			return fmt.Errorf("faults: %s: %w", f.Name, err)
		}
	}
	return nil
}

func waitFor(ctx context.Context, c *kindcluster.Cluster, cond Condition, deadline time.Time) error {
	var last string
	for {
		out, err := c.Kubectl(ctx, cond.Args...)
		if err == nil && cond.satisfied(out) {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = strings.TrimSpace(out)
		}
		if time.Now().After(deadline) {
			want := fmt.Sprintf("%q in output", cond.Contains)
			if cond.Empty {
				want = "empty output"
			}
			return fmt.Errorf("timed out waiting for %s (want %s, last saw %q)",
				cond.Describe, want, truncate(last, 200))
		}
		select {
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
