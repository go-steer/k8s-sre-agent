// Package schema defines the structured-output contract shared by the SRE
// agent, the monitoring loop, and the eval harness.
//
// This is a port of the upstream Python project's schemas.py. The field names
// and JSON encoding are kept wire-identical so the same eval dataset scores
// both implementations and a HealthReport produced by either is readable by
// the other.
//
// The load-bearing design point is that a Finding carries machine-stable
// identity fields (Kind / ResourceName / Reason) alongside the human-facing
// Title and Detail. Two consumers depend on that split:
//
//   - The monitoring loop fingerprints findings across runs. Fingerprinting
//     Title does not work — the model rewords it between runs
//     ("CrashLoopBackOff on api-7d9" vs "api-7d9 is crash looping"), which
//     makes one ongoing incident look new every interval.
//   - The eval harness asserts on findings. Grading free text needs an LLM
//     judge; grading Kind+ResourceName+Reason is exact-match arithmetic.
package schema

import (
	"fmt"
	"strings"
)

// Severity is the level a single Finding can carry.
type Severity string

// OverallSeverity is the level a whole HealthReport can carry. It is a
// superset of Severity plus SeverityOK.
//
// Upstream learned this the hard way: the analysis prompt asks for "the
// highest severity among your findings", so every value a Finding can take
// has to be representable here. Omitting "info" made an all-info report
// unanswerable and failed validation for the entire report.
type OverallSeverity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityInfo     Severity = "info"
)

const (
	OverallCritical OverallSeverity = "critical"
	OverallWarning  OverallSeverity = "warning"
	OverallInfo     OverallSeverity = "info"
	OverallOK       OverallSeverity = "ok"
)

// severityRank orders severities for "highest wins" reduction. Higher is worse.
var severityRank = map[OverallSeverity]int{
	OverallOK:       0,
	OverallInfo:     1,
	OverallWarning:  2,
	OverallCritical: 3,
}

// Valid reports whether s is a severity a Finding may carry.
func (s Severity) Valid() bool {
	switch s {
	case SeverityCritical, SeverityWarning, SeverityInfo:
		return true
	}
	return false
}

// Valid reports whether s is a severity a HealthReport may carry.
func (s OverallSeverity) Valid() bool {
	_, ok := severityRank[s]
	return ok
}

// Overall widens a Finding severity to a report severity.
func (s Severity) Overall() OverallSeverity { return OverallSeverity(s) }

// AtLeast reports whether s is as severe as other, or worse.
func (s OverallSeverity) AtLeast(other OverallSeverity) bool {
	return severityRank[s] >= severityRank[other]
}

// ParseSeverity accepts a severity in any case, with or without the
// bracketed form the free-text prompt uses ("[CRITICAL]", "critical",
// "CRITICAL:"). It exists because the two producers disagree: the structured
// path emits lowercase enum values, while the free-text path the upstream
// prompt specifies emits "[CRITICAL]" section headers and the upstream eval
// dataset writes a bare "CRITICAL:" prefix.
func ParseSeverity(s string) (OverallSeverity, bool) {
	t := OverallSeverity(strings.ToLower(strings.Trim(strings.TrimSpace(s), "[]:")))
	if !t.Valid() {
		return "", false
	}
	return t, true
}

// Finding is a single issue or observation about the cluster.
type Finding struct {
	Severity Severity `json:"severity"`
	// Title is a short headline, e.g. "CrashLoopBackOff on api-7d9".
	Title string `json:"title"`
	// Detail is a specific explanation naming resources and the cause.
	Detail string `json:"detail"`
	// Namespace is the Kubernetes namespace, if applicable.
	Namespace string `json:"namespace,omitempty"`
	// Kind is the Kubernetes kind of the affected object, capitalized and
	// singular: Pod, Deployment, StatefulSet, DaemonSet, Node, HPA, Job,
	// CronJob, Service, PersistentVolume, Namespace. Empty only for
	// cluster-wide observations.
	Kind string `json:"kind,omitempty"`
	// ResourceName is the name of the single affected object. A finding
	// covering several objects is emitted once per object rather than
	// listing them here — that is what keeps fingerprints stable.
	ResourceName string `json:"resource_name,omitempty"`
	// Reason is a short stable machine-style cause in CamelCase:
	// CrashLoopBackOff, OOMKilled, ImagePullBackOff, NotReady,
	// HPAAtMaxReplicas, MissingResourceLimits, NoPodDisruptionBudget,
	// LatestImageTag. The same token must be reused for the same class of
	// problem across runs.
	Reason string `json:"reason,omitempty"`
}

// Validate reports the first structural problem with f, or nil.
func (f Finding) Validate() error {
	if !f.Severity.Valid() {
		return fmt.Errorf("finding %q: invalid severity %q", f.Title, f.Severity)
	}
	if strings.TrimSpace(f.Title) == "" {
		return fmt.Errorf("finding: empty title")
	}
	return nil
}

// HealthReport is a structured cluster health report.
type HealthReport struct {
	// OverallSeverity is the highest severity across all findings, or "ok".
	OverallSeverity OverallSeverity `json:"overall_severity"`
	// Summary is a one- or two-sentence overall summary.
	Summary string `json:"summary"`
	// Findings lists all issues found, most severe first.
	Findings []Finding `json:"findings"`
	// RecommendedActions lists concrete, ordered next steps.
	RecommendedActions []string `json:"recommended_actions,omitempty"`
}

// HasIssues reports whether the report is at warning level or worse.
func (r HealthReport) HasIssues() bool {
	return r.OverallSeverity == OverallCritical || r.OverallSeverity == OverallWarning
}

// DerivedSeverity is the highest severity among r's findings, or "ok" when
// there are none. Use it to check a model-reported OverallSeverity rather
// than to silently overwrite one: a model that reports an OverallSeverity
// inconsistent with its own findings is a signal worth surfacing.
func (r HealthReport) DerivedSeverity() OverallSeverity {
	worst := OverallOK
	for _, f := range r.Findings {
		if s := f.Severity.Overall(); s.AtLeast(worst) {
			worst = s
		}
	}
	return worst
}

// Validate reports the first structural problem with r, or nil.
func (r HealthReport) Validate() error {
	if !r.OverallSeverity.Valid() {
		return fmt.Errorf("report: invalid overall_severity %q", r.OverallSeverity)
	}
	for i, f := range r.Findings {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("findings[%d]: %w", i, err)
		}
	}
	if len(r.Findings) == 0 && r.OverallSeverity != OverallOK {
		return fmt.Errorf("report: overall_severity %q with no findings", r.OverallSeverity)
	}
	return nil
}
