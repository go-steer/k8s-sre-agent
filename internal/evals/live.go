// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package evals

import (
	"fmt"
	"sort"
	"strings"

	"github.com/go-steer/k8s-sre-agent/internal/faults"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// LiveEvaluator grades a run against an injected fault rather than against a
// prose ground truth.
//
// A separate interface from Evaluator because the reference is a different
// kind of thing. Tier 1's ground truth is a sentence a human wrote; tier 2's
// is the state of a cluster we broke on purpose. Forcing both through one
// interface would mean synthesizing a fake Example for every fixture, and the
// fake would then be the thing under test.
type LiveEvaluator interface {
	Name() string
	Score(f faults.Fault, run Run) Score
}

// DefaultLiveEvaluators are the deterministic tier-2 evaluators.
func DefaultLiveEvaluators() []LiveEvaluator {
	return []LiveEvaluator{FaultRecall{}, HallucinatedFault{}, FaultSeverity{}, RootCause{}}
}

// FaultRecall is the fraction of a fixture's expected findings the agent
// actually reported.
//
// This is the number tier 2 exists to produce. Tier 1 cannot measure it: its
// scenarios state the fault in the prompt, so "did the agent find it" is not a
// question the dataset can pose. Here the fault is only in the cluster, and a
// miss means the agent looked in the wrong place or read the wrong thing.
type FaultRecall struct{}

func (FaultRecall) Name() string { return "fault_recall" }

func (FaultRecall) Score(f faults.Fault, run Run) Score {
	if len(f.Want) == 0 {
		return Score{
			Name:    "fault_recall",
			Skipped: true,
			Comment: "healthy fixture; nothing to recall",
		}
	}
	if run.Health == nil {
		return Score{
			Name:    "fault_recall",
			Value:   0,
			Comment: "agent produced no structured report",
		}
	}

	var found, missed []string
	for _, w := range f.Want {
		label := fmt.Sprintf("%s/%s", w.Kind, w.Name)
		if matchesAny(f, w, run.Health.Findings) {
			found = append(found, label)
		} else {
			missed = append(missed, label)
		}
	}
	sort.Strings(missed)

	comment := fmt.Sprintf("found %d/%d", len(found), len(f.Want))
	if len(missed) > 0 {
		comment += ", missed=" + strings.Join(missed, ",")
	}
	return Score{
		Name:    "fault_recall",
		Value:   float64(len(found)) / float64(len(f.Want)),
		Comment: comment,
	}
}

// HallucinatedFault checks that the agent did not report a failure mode the
// cluster does not have.
//
// # Why this is not a general precision metric
//
// The obvious counterpart to recall is "what fraction of findings were
// expected", and it would be the wrong metric here. The fixture manifests are
// minimal: no liveness probes, no PodDisruptionBudgets, no resource limits on
// the deliberately-broken workloads. An agent that notes those is *correct*,
// and a precision score would mark it down for thoroughness — which would push
// us to prompt the agent to say less, the opposite of what we want.
//
// So only one class of finding is penalized: claiming one of the concrete
// failure modes the fixture set knows how to inject, in a namespace where that
// mode was not injected. Calling a Pending pod a CrashLoopBackOff is a
// misdiagnosis. Noting that it also has no resource limits is not.
//
// This is what makes the healthy fixture cost something, and it doubles as a
// misdiagnosis check on the faulty ones.
type HallucinatedFault struct{}

func (HallucinatedFault) Name() string { return "hallucinated_fault" }

// failureFamilies groups the reason tokens that name the same concrete
// failure mode. Only reasons in this table can be hallucinated — a reason
// outside it is a configuration advisory, which is not this evaluator's
// business.
//
// Deliberately excluded: "Pending", "Failed", "Error", "Unhealthy". They are
// real reasons the fixtures accept for recall, but they are too generic to
// attribute to one failure mode, and calling them hallucinations would punish
// an agent for using the vocabulary kubectl handed it.
//
// Membership has one requirement: the token must assert a *cause*. Ask what
// else the control plane writes it for. "Unschedulable" is written for unbound
// volumes, taints and node selectors alike, so it names no cause and lives in
// genericReasons; "NotReady" is written on pods, containers and nodes alike, so
// it names a state and not a node, and only NodeNotReady/NodeUnreachable name
// their own subject.
//
// There used to be a second requirement — that a token not be a bare substring
// of a plausible unrelated reason — and it is gone because familyOf no longer
// matches substrings. It was never a property of the vocabulary; it was a
// property of the matcher, and it was wrong four times before it was enforced
// once. What is *not* gone is the judgement that put "NotReady" and
// "DeadlineExceeded" outside their families in the first place: both fail
// requirement 1 on their own terms, so exact matching is not an invitation to
// put them back. An agent that writes the bare "DeadlineExceeded" about a
// stalled rollout is still not making a claim about a Job.
var failureFamilies = map[string][]string{
	"image-pull": {"ImagePullBackOff", "ErrImagePull", "InvalidImageName", "ImagePullError"},
	"crash-loop": {"CrashLoopBackOff", "CrashLooping"},
	"oom":        {"OOMKilled", "OutOfMemory", "MemoryLimitExceeded"},
	// Named for what it claims, not for the pod condition it used to include:
	// every token here asserts the node ran out of something. "Unschedulable"
	// was in this family and is now generic — see genericReasons.
	"resource-pressure": {"InsufficientCPU", "InsufficientMemory", "InsufficientResources"},
	// "DeadlineExceeded" was here and is gone for the same reason as
	// "NotReady", found the same day by the test that enforces the rule rather
	// than by a live run. A Job that outruns activeDeadlineSeconds really does
	// get that token, so it asserted a cause and read as a legitimate member —
	// but it is a substring of "ProgressDeadlineExceeded", which is what the
	// Deployment controller writes on a rollout that cannot progress. Every
	// stuck rollout tier 3 has produced would have been charged here as an
	// invented job failure. What is lost is small and one-directional: an agent
	// that writes the bare token for a job is no longer *credited* with a
	// concrete claim, which can only fail to charge a misdiagnosis, never
	// invent one. BackoffLimitExceeded and JobFailed still name the family.
	"job-failed":   {"BackoffLimitExceeded", "JobFailed"},
	"no-endpoints": {"NoEndpoints", "SelectorMismatch", "NoMatchingPods", "EmptyEndpoints", "ServiceHasNoEndpoints"},
	// "NotReady" was in this family and is gone. It is a bare substring of
	// "PodsNotReady" and "ContainersNotReady", which MatchesReason therefore
	// resolved here — so an agent reporting a Service whose pods are not ready
	// was charged with inventing a node failure. Kubernetes writes Ready=False
	// on pods, containers and nodes alike, so the bare token names a state and
	// not a node; both survivors name the node. See the substring rule below.
	"node-down":      {"NodeNotReady", "NodeUnreachable"},
	"disk":           {"DiskPressure", "Evicted", "VolumeMountFailed", "FailedMount"},
	"volume-binding": {"Unbound", "UnboundImmediatePersistentVolumeClaims", "VolumeBindingFailed", "ProvisioningFailed", "StorageClassNotFound", "NoStorageClass", "MissingStorageClass"},
}

// genericReasons name *that* something failed without naming *how*.
//
// They are perfectly good words for a report to use — a fixture may list them
// in Want.Reasons and recall accepts them — but they can never be hallucinated,
// because they attribute nothing to attribute wrongly.
//
// This has to be an explicit set rather than mere absence from
// failureFamilies, which is how it was written and is not the same thing.
// MatchesReason is a bidirectional substring match, so the bare token "Failed"
// matched "FailedMount" and resolved to the disk family, and "Error" matched
// "ImagePullError". An agent that used kubectl's own word for a failed Job was
// charged with inventing a disk fault. The rule was documented and unenforced.
//
// Note what this set does *not* cover, which took a third live run to learn:
// it is consulted only for the token the agent wrote, never for the tokens the
// families contain, so under substring matching a bare member of
// failureFamilies was unprotected by it. That is how "NotReady" annexed
// "PodsNotReady". familyOf matches exactly now, so that half of the hazard is
// gone — but this set is not, because the tokens in it are ones an agent
// really does write, and they must resolve to no family when it does.
//
// FailedScheduling and Unschedulable are here for the same reason, and both
// were found by fault-ledger — a pod Pending behind a PVC that will never bind.
// The scheduler writes both for unbound volumes, taints, node selectors and
// genuine resource shortage alike, so they say scheduling failed and not why.
//
// Unschedulable arrived a fixture later than FailedScheduling and only because
// a live run produced it. This file previously argued that the unschedulable
// family could keep Unschedulable because, unlike FailedScheduling, it "names a
// cause". It does not: it is the reason string on the PodScheduled=False
// condition, and on fault-ledger the scheduler wrote it with the message "pod
// has unbound immediate PersistentVolumeClaims". An agent that reported the PVC
// with that literal token scored hallucinated_fault 0.000 for a statement the
// cluster itself had made. What is left in the family — InsufficientCPU and
// friends — asserts a shortage, which is a claim that can be false.
//
// Matched exactly, on the normalized token, never as a substring: "FailedMount"
// must stay a disk claim even though "Failed" is generic.
var genericReasons = map[string]bool{
	"pending":          true,
	"failed":           true,
	"error":            true,
	"unhealthy":        true,
	"failedscheduling": true,
	"unschedulable":    true,
}

// normReason folds a reason the way faults.Want.MatchesReason does, so that the
// generic-token check and the family match agree on what one token is.
func normReason(s string) string {
	return strings.ToLower(strings.NewReplacer("_", "", "-", "", " ", "").Replace(strings.TrimSpace(s)))
}

// familyByReason inverts failureFamilies onto the normalized token, which is
// what makes familyOf a lookup rather than a scan. Built once, and a duplicate
// token across two families is a programming error the init check catches
// rather than a silent dependence on map order.
var familyByReason = invertFamilies(failureFamilies)

func invertFamilies(families map[string][]string) map[string]string {
	out := make(map[string]string)
	for family, reasons := range families {
		for _, r := range reasons {
			norm := normReason(r)
			if prev, dup := out[norm]; dup {
				panic(fmt.Sprintf("evals: reason %q is in both %q and %q", r, prev, family))
			}
			out[norm] = family
		}
	}
	return out
}

// familyOf returns the failure family a reason belongs to, or "".
//
// The match is on the exact normalized token — case, underscores, hyphens and
// spaces folded, and nothing else. It used to run faults.Want.MatchesReason
// over each family in turn, which matches substrings in *both* directions, and
// that was the wrong matcher here for a reason worth keeping:
//
// MatchesReason is right for a fixture's Want, where the fixture author lists
// the spellings of one answer and "CrashLoop" and "CrashLoopBackOff" are the
// same answer. It is wrong for this function, whose argument is whatever the
// agent chose to write. A bare family member silently annexed every longer
// token containing it, so an honest report was scored as an invention:
// "Failed" claimed FailedMount for the disk family, "Error" claimed
// ImagePullError, "NotReady" claimed PodsNotReady, "DeadlineExceeded" claimed
// ProgressDeadlineExceeded. Three of those four cost a live run to find.
//
// The change is one-directional in the direction that matters. A token that no
// longer resolves is a concrete claim no longer *credited*, which can only fail
// to charge a misdiagnosis — never invent one. That is the same argument the
// DeadlineExceeded removal made, applied to the matcher instead of to one row.
//
// It is *not* one-directional in the other use of this function.
// HallucinatedFault.Score calls familyOf twice in opposite directions: once
// over a fixture's own Want.Reasons to learn which families were injected, and
// once over the agent's findings. A Want reason that stops resolving drops a
// family out of `injected`, and then a correct report about the fixture's own
// fault is charged as invention. Every current fixture still names its family
// exactly, and TestEveryFixtureStillNamesItsInjectedFamily is what keeps that
// true as fixtures are added.
func familyOf(reason string) string {
	norm := normReason(reason)
	if norm == "" || genericReasons[norm] {
		return ""
	}
	return familyByReason[norm]
}

func (HallucinatedFault) Score(f faults.Fault, run Run) Score {
	if run.Health == nil {
		return Score{
			Name:    "hallucinated_fault",
			Value:   0,
			Comment: "agent produced no structured report",
		}
	}

	// The families this fixture actually injected, taken from its own Wants so
	// that adding a fixture cannot forget to update a second table.
	injected := map[string]bool{}
	for _, w := range f.Want {
		for _, r := range w.Reasons {
			if fam := familyOf(r); fam != "" {
				injected[fam] = true
			}
		}
	}

	var claimed, bogus []string
	for _, finding := range run.Health.Findings {
		// Only failures are graded. Info-level notes are observations, and the
		// agent is asked to report observations.
		if !finding.Severity.Overall().AtLeast(schema.OverallWarning) {
			continue
		}
		if finding.Namespace != "" && finding.Namespace != f.Namespace() {
			// A finding about another namespace is out of scope for this
			// fixture; the prompt named one namespace, and cluster-wide notes
			// are not what is being graded.
			continue
		}
		fam := familyOf(finding.Reason)
		if fam == "" {
			continue
		}
		claimed = append(claimed, fam)
		if !injected[fam] {
			bogus = append(bogus, fmt.Sprintf("%s(%s/%s)", finding.Reason, finding.Kind, finding.ResourceName))
		}
	}

	if len(claimed) == 0 {
		return Score{
			Name:    "hallucinated_fault",
			Value:   1,
			Comment: "claimed no concrete failure mode",
		}
	}
	v := 1 - float64(len(bogus))/float64(len(claimed))
	comment := fmt.Sprintf("%d/%d failure claims are real", len(claimed)-len(bogus), len(claimed))
	if len(bogus) > 0 {
		comment += ", invented=" + strings.Join(bogus, ",")
	}
	return Score{Name: "hallucinated_fault", Value: v, Comment: comment}
}

// FaultSeverity grades the report-level severity by distance, the same way
// tier 1's SeverityCalibration does.
//
// Distance rather than exact match for the reason established in tier 1: the
// observed misses are one-directional, and exact match scores a systematically
// hot agent the same as a confused one. The direction is in the comment
// because the two call for opposite fixes.
type FaultSeverity struct{}

func (FaultSeverity) Name() string { return "fault_severity" }

func (FaultSeverity) Score(f faults.Fault, run Run) Score {
	if f.WantSeverity == "" {
		return Score{Name: "fault_severity", Skipped: true, Comment: "fixture declares no expected severity"}
	}
	if run.Health == nil {
		return Score{
			Name:    "fault_severity",
			Value:   0,
			Comment: "agent produced no structured report",
		}
	}
	got := run.Health.OverallSeverity
	if !got.Valid() {
		return Score{
			Name:    "fault_severity",
			Value:   0,
			Comment: fmt.Sprintf("invalid severity %q; expected %s", got, f.WantSeverity),
		}
	}

	delta := severityRank[got] - severityRank[f.WantSeverity]
	dist := delta
	if dist < 0 {
		dist = -dist
	}
	dir := "exact"
	switch {
	case delta > 0:
		dir = fmt.Sprintf("%d too high", delta)
	case delta < 0:
		dir = fmt.Sprintf("%d too low", -delta)
	}
	return Score{
		Name:    "fault_severity",
		Value:   1 - float64(dist)/3,
		Comment: fmt.Sprintf("actual=%s expected=%s (%s)", got, f.WantSeverity, dir),
	}
}

// RootCause is the fraction of a fixture's root-cause findings the agent
// reported, on the fixtures where a cause and its downstream symptom are both
// expected.
//
// # Why recall cannot express this
//
// On a fixture with a cause and a symptom, fault_recall gives 0.5 to an agent
// that reported only the symptom and 0.5 to one that reported only the cause.
// Those are not the same answer. The symptom is what the operator already saw —
// it is why they opened the incident — and the cause is the thing they have to
// change. An agent that names the Service with no ready endpoints has told them
// nothing they did not know; one that names the crash-looping pod behind it has
// done the job.
//
// The metric is deliberately not a penalty for reporting the symptom too.
// Reporting both is the best answer: the symptom is how the finding is
// recognized and the cause is how it is fixed. Only the cause's absence costs
// anything.
//
// Skipped on every fixture that marks no root, which is all the single-fault
// ones. Marking their one Want root would make this a second copy of recall on
// six of ten fixtures and drown the two where the distinction is real.
type RootCause struct{}

func (RootCause) Name() string { return "root_cause" }

func (RootCause) Score(f faults.Fault, run Run) Score {
	var roots []faults.Want
	for _, w := range f.Want {
		if w.Root {
			roots = append(roots, w)
		}
	}
	if len(roots) == 0 {
		return Score{
			Name:    "root_cause",
			Skipped: true,
			Comment: "fixture marks no root cause",
		}
	}
	if run.Health == nil {
		return Score{
			Name:    "root_cause",
			Value:   0,
			Comment: "agent produced no structured report",
		}
	}

	var found, missed []string
	for _, w := range roots {
		label := fmt.Sprintf("%s/%s", w.Kind, w.Name)
		if matchesAny(f, w, run.Health.Findings) {
			found = append(found, label)
		} else {
			missed = append(missed, label)
		}
	}
	sort.Strings(missed)

	comment := fmt.Sprintf("named %d/%d root causes", len(found), len(roots))
	if len(missed) > 0 {
		comment += ", missed=" + strings.Join(missed, ",")
	}
	return Score{
		Name:    "root_cause",
		Value:   float64(len(found)) / float64(len(roots)),
		Comment: comment,
	}
}

// matchesAny reports whether any finding satisfies w, with the fixture's
// namespace as an extra constraint.
//
// Namespace is checked here rather than in faults.Want because a Want
// describes an object within its fixture and would otherwise have to repeat
// the namespace on every entry. An empty namespace on the finding is accepted:
// the agent was asked about one namespace, so omitting it is terse, not wrong.
func matchesAny(f faults.Fault, w faults.Want, findings []schema.Finding) bool {
	for _, finding := range findings {
		if finding.Namespace != "" && finding.Namespace != f.Namespace() {
			continue
		}
		if w.Matches(finding) {
			return true
		}
	}
	return false
}

// ScoreLive runs every default live evaluator against one fixture.
func ScoreLive(f faults.Fault, run Run) []Score {
	evaluators := DefaultLiveEvaluators()
	out := make([]Score, 0, len(evaluators))
	for _, e := range evaluators {
		out = append(out, e.Score(f, run))
	}
	return out
}
