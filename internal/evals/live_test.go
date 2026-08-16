package evals

import (
	"slices"
	"sort"
	"testing"

	"github.com/go-steer/core-sre-agent/internal/faults"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

// perfectRun builds the report a flawless agent would return for a fixture.
// It is the tier-2 analogue of tier 1's ground-truth self-score: if this does
// not hit 1.000, the evaluator is asking for something no correct answer
// provides, which is exactly how upstream's defects survived.
func perfectRun(f faults.Fault) Run {
	report := &schema.HealthReport{
		OverallSeverity: f.WantSeverity,
		Summary:         "reference answer",
	}
	for _, w := range f.Want {
		sev := w.MinSeverity
		if sev == "" {
			sev = schema.SeverityWarning
		}
		reason := ""
		if len(w.Reasons) > 0 {
			reason = w.Reasons[0]
		}
		report.Findings = append(report.Findings, schema.Finding{
			Severity:     sev,
			Title:        "reference finding",
			Detail:       "reference detail",
			Namespace:    f.Namespace(),
			Kind:         w.Kind,
			ResourceName: w.Name,
			Reason:       reason,
		})
	}
	return Run{Health: report}
}

// Ceiling: a correct report must score 1.000 on every live evaluator.
func TestLiveGroundTruthScoresCeiling(t *testing.T) {
	for _, f := range faults.All() {
		t.Run(f.Name, func(t *testing.T) {
			for _, s := range ScoreLive(f, perfectRun(f)) {
				if s.Skipped {
					continue
				}
				if s.Value != 1 {
					t.Errorf("%s = %.3f, want 1.000 (%s)", s.Name, s.Value, s.Comment)
				}
			}
		})
	}
}

// Floor: an agent that reports a clean bill of health for every namespace must
// score near zero on recall. It gets full marks on hallucination — it invented
// nothing — which is correct and is why hallucination is reported separately
// rather than folded into one number.
func TestLiveUselessAgentScoresFloor(t *testing.T) {
	clean := Run{Health: &schema.HealthReport{
		OverallSeverity: schema.OverallOK,
		Summary:         "no issues detected",
	}}

	var recallSum float64
	var recallN int
	for _, f := range faults.All() {
		for _, s := range ScoreLive(f, clean) {
			if s.Skipped || s.Name != "fault_recall" {
				continue
			}
			recallSum += s.Value
			recallN++
		}
	}
	if recallN == 0 {
		t.Fatal("no fixture produced a recall score")
	}
	if mean := recallSum / float64(recallN); mean > 0.25 {
		t.Errorf("an agent that reports nothing scores %.3f recall, want <= 0.25", mean)
	}
}

// The other way to game recall: report every failure mode in every namespace.
// It buys recall, so hallucinated_fault has to make it expensive — otherwise
// the two numbers together are still gameable by one strategy.
func TestCarpetBombingIsPunished(t *testing.T) {
	all := faults.All()

	var recallSum, halluSum float64
	var n int
	for _, f := range all {
		// Every fixture's expected finding, claimed against every fixture.
		bomb := &schema.HealthReport{OverallSeverity: schema.OverallCritical, Summary: "everything is broken"}
		for _, other := range all {
			for _, w := range other.Want {
				reason := ""
				if len(w.Reasons) > 0 {
					reason = w.Reasons[0]
				}
				bomb.Findings = append(bomb.Findings, schema.Finding{
					Severity:     schema.SeverityCritical,
					Title:        "everything",
					Namespace:    f.Namespace(),
					Kind:         w.Kind,
					ResourceName: w.Name,
					Reason:       reason,
				})
			}
		}
		run := Run{Health: bomb}
		for _, s := range ScoreLive(f, run) {
			switch {
			case s.Skipped:
			case s.Name == "fault_recall":
				recallSum += s.Value
				n++
			case s.Name == "hallucinated_fault":
				halluSum += s.Value
			}
		}
	}

	recall := recallSum / float64(n)
	hallucination := halluSum / float64(len(all))
	t.Logf("carpet bombing: recall=%.3f hallucinated_fault=%.3f", recall, hallucination)

	if recall < 0.9 {
		t.Fatalf("carpet bombing scored %.3f recall — the premise of this test no longer holds", recall)
	}
	if hallucination > 0.4 {
		t.Errorf("carpet bombing scores %.3f on hallucinated_fault, want <= 0.4 — "+
			"recall is gameable if inventing faults is cheap", hallucination)
	}
}

// The healthy fixture must punish an invented failure and tolerate an honest
// configuration advisory. If it did the latter, the metric would push us to
// prompt the agent into saying less.
func TestHealthyFixtureSeparatesAdvicefromInvention(t *testing.T) {
	var healthy faults.Fault
	for _, f := range faults.All() {
		if len(f.Want) == 0 {
			healthy = f
		}
	}
	if healthy.Name == "" {
		t.Fatal("no healthy fixture in the set")
	}

	advisory := Run{Health: &schema.HealthReport{
		OverallSeverity: schema.OverallWarning,
		Summary:         "workloads are running; configuration could be tightened",
		Findings: []schema.Finding{{
			Severity: schema.SeverityWarning, Title: "no probes",
			Namespace: healthy.Namespace(), Kind: "Deployment",
			ResourceName: "docs-site", Reason: "MissingProbes",
		}},
	}}
	invention := Run{Health: &schema.HealthReport{
		OverallSeverity: schema.OverallCritical,
		Summary:         "workload is crash looping",
		Findings: []schema.Finding{{
			Severity: schema.SeverityCritical, Title: "crash loop",
			Namespace: healthy.Namespace(), Kind: "Pod",
			ResourceName: "docs-site-abc123", Reason: "CrashLoopBackOff",
		}},
	}}

	if got := (HallucinatedFault{}).Score(healthy, advisory); got.Value != 1 {
		t.Errorf("a configuration advisory scored %.3f (%s); advisories must not count as invention",
			got.Value, got.Comment)
	}
	if got := (HallucinatedFault{}).Score(healthy, invention); got.Value != 0 {
		t.Errorf("an invented crash loop scored %.3f (%s), want 0", got.Value, got.Comment)
	}
}

// Reporting the wrong failure mode for a real fault is a misdiagnosis, not
// noise — the agent looked, and concluded something false.
func TestMisdiagnosisCountsAsHallucination(t *testing.T) {
	fixtures, err := faults.ByName([]string{"fault-unschedulable"})
	if err != nil {
		t.Fatal(err)
	}
	f := fixtures[0]

	run := Run{Health: &schema.HealthReport{
		OverallSeverity: schema.OverallCritical,
		Summary:         "pod is crash looping",
		Findings: []schema.Finding{{
			Severity: schema.SeverityCritical, Title: "crash loop",
			Namespace: f.Namespace(), Kind: "Pod",
			ResourceName: "analytics-etl-abc123", Reason: "CrashLoopBackOff",
		}},
	}}

	if s := (FaultRecall{}).Score(f, run); s.Value != 0 {
		t.Errorf("recall = %.3f for a wrong failure mode, want 0", s.Value)
	}
	if s := (HallucinatedFault{}).Score(f, run); s.Value != 0 {
		t.Errorf("hallucinated_fault = %.3f for a misdiagnosis, want 0 (%s)", s.Value, s.Comment)
	}
}

// The whole reason root_cause exists: on a cause-and-symptom fixture, recall
// cannot tell the two apart. It pays 0.5 for the outage anyone would have
// noticed and 0.5 for the crash loop behind it, and those are not the same
// answer — the symptom is why the incident was opened, the cause is what has to
// be changed. If this test ever passes with root_cause removed, the metric was
// never measuring anything recall did not already.
func TestRootCauseSeparatesCauseFromSymptom(t *testing.T) {
	fixtures, err := faults.ByName([]string{"fault-sessions"})
	if err != nil {
		t.Fatal(err)
	}
	f := fixtures[0]

	finding := func(kind, name, reason string) schema.Finding {
		return schema.Finding{
			Severity: schema.SeverityCritical, Title: "finding", Detail: "detail",
			Namespace: f.Namespace(), Kind: kind, ResourceName: name, Reason: reason,
		}
	}
	run := func(findings ...schema.Finding) Run {
		return Run{Health: &schema.HealthReport{
			OverallSeverity: f.WantSeverity, Summary: "s", Findings: findings,
		}}
	}

	cause := finding("Pod", "session-store-7d9f4-x2k9", "CrashLoopBackOff")
	symptom := finding("Service", "session-store", "NoEndpoints")

	symptomOnly := run(symptom)
	causeOnly := run(cause)

	// The premise: recall is blind here.
	rs := (FaultRecall{}).Score(f, symptomOnly).Value
	rc := (FaultRecall{}).Score(f, causeOnly).Value
	if rs != rc {
		t.Fatalf("recall already separates these (%.3f vs %.3f) — this fixture no longer "+
			"has the shape root_cause was built for", rs, rc)
	}

	if got := (RootCause{}).Score(f, symptomOnly); got.Value != 0 {
		t.Errorf("naming only the symptom scored root_cause %.3f (%s), want 0", got.Value, got.Comment)
	}
	if got := (RootCause{}).Score(f, causeOnly); got.Value != 1 {
		t.Errorf("naming the cause scored root_cause %.3f (%s), want 1", got.Value, got.Comment)
	}

	// Reporting the symptom as well must cost nothing. The best answer names
	// both — the symptom is how an operator recognizes the incident — and a
	// metric that penalized it would be the precision metric this tier
	// deliberately does not have.
	both := run(symptom, cause)
	if got := (RootCause{}).Score(f, both); got.Value != 1 {
		t.Errorf("naming both scored root_cause %.3f (%s), want 1", got.Value, got.Comment)
	}
	if got := (FaultRecall{}).Score(f, both); got.Value != 1 {
		t.Errorf("naming both scored recall %.3f (%s), want 1", got.Value, got.Comment)
	}
}

// On a fixture that grades one finding, root_cause would be a second copy of
// recall. It skips instead, so the reported mean is taken over the fixtures
// where the distinction is real rather than diluted by nine restatements.
func TestRootCauseSkipsFixturesWithNoRoot(t *testing.T) {
	var graded, skipped []string
	for _, f := range faults.All() {
		s := (RootCause{}).Score(f, perfectRun(f))
		if s.Skipped {
			skipped = append(skipped, f.Name)
			continue
		}
		graded = append(graded, f.Name)
		if len(f.Want) < 2 {
			t.Errorf("%s grades root_cause with %d want(s) — that is recall under another name",
				f.Name, len(f.Want))
		}
	}
	if len(graded) == 0 {
		t.Fatalf("no fixture grades root_cause; skipped=%v", skipped)
	}
	t.Logf("root_cause graded on %v, skipped on %d fixtures", graded, len(skipped))
}

// An agent that returns prose instead of a HealthReport must not score as an
// agent that found nothing — the structured contract is the deliverable.
func TestNoStructuredReportScoresZero(t *testing.T) {
	f := faults.All()[0]
	run := Run{Response: "The checkout-api pods are in ImagePullBackOff."}
	for _, s := range ScoreLive(f, run) {
		if s.Skipped {
			continue
		}
		if s.Value != 0 {
			t.Errorf("%s = %.3f without a structured report, want 0", s.Name, s.Value)
		}
	}
}

func TestFamilyOf(t *testing.T) {
	for _, tc := range []struct{ reason, want string }{
		{"CrashLoopBackOff", "crash-loop"},
		{"crash_loop_back_off", "crash-loop"},
		{"OOMKilled", "oom"},
		{"ImagePullBackOff", "image-pull"},
		{"NoEndpoints", "no-endpoints"},
		{"VolumeBindingFailed", "volume-binding"},

		// Advisories and generic tokens are not failure families. If any of
		// these started resolving to one, honest reporting would be scored as
		// invention.
		{"MissingResourceLimits", ""},
		{"NoPodDisruptionBudget", ""},
		{"LatestImageTag", ""},
		{"MissingProbes", ""},
		{"", ""},

		// The generic tokens, pinned because four of them silently were not
		// generic: MatchesReason is a bidirectional substring match, so "Failed"
		// resolved to disk via "FailedMount", "Error" to image-pull via
		// "ImagePullError", and "FailedScheduling" to the resource-pressure
		// family in a namespace whose pod is Pending on a volume. Each one
		// charged an agent with invention for using kubectl's own word.
		//
		// "Unschedulable" is the same token one layer up — the PodScheduled
		// condition's reason, which the scheduler writes for an unbound PVC as
		// readily as for a full node — and a live run on fault-ledger is what
		// showed it. A family may only contain tokens that assert a cause.
		{"Pending", ""},
		{"Failed", ""},
		{"Error", ""},
		{"Unhealthy", ""},
		{"FailedScheduling", ""},
		{"Unschedulable", ""},

		// The shortage claims Unschedulable used to share a family with. These
		// can be false, so they stay gradeable.
		{"InsufficientCPU", "resource-pressure"},
		{"InsufficientMemory", "resource-pressure"},

		// ...and the tokens they were colliding with still resolve. A generic
		// token is excluded by exact match, not by substring, or the fix would
		// have deleted three real families.
		{"FailedMount", "disk"},
		{"ImagePullError", "image-pull"},
		{"JobFailed", "job-failed"},

		// The bare tokens that were annexing honest vocabulary from inside a
		// family, where genericReasons cannot reach them. "NotReady" made every
		// pod- and container-level readiness report a claim about a node;
		// "DeadlineExceeded" made every stuck Deployment rollout a claim about
		// a Job. Both families still resolve from tokens that name their own
		// subject.
		{"PodsNotReady", ""},
		{"ContainersNotReady", ""},
		{"NodeNotReady", "node-down"},
		{"ProgressDeadlineExceeded", ""},
		{"BackoffLimitExceeded", "job-failed"},

		// What exact matching gave up, pinned so the cost is visible rather
		// than assumed. Each of these used to resolve by substring and no
		// longer does. All three are the agent writing an abbreviation or an
		// elaboration of a real token, and losing them can only fail to charge
		// a misdiagnosis, never invent one — which is the trade this matcher
		// makes deliberately. The same substring rule was charging honest
		// reports as inventions in the other direction, four times over.
		{"CrashLoop", ""},
		{"ContainerCrashLoopBackOff", ""},
		{"OOMKilledRepeatedly", ""},
	} {
		if got := familyOf(tc.reason); got != tc.want {
			t.Errorf("familyOf(%q) = %q, want %q", tc.reason, got, tc.want)
		}
	}
}

// TestNoHonestReasonIsAFailureClaim runs the vocabulary an agent actually
// writes past familyOf and requires all of it to resolve to nothing.
//
// It was written to enforce a rule that no longer exists — that no family
// member may be a bare substring of a reason asserting something else — and
// familyOf's exact matching has retired that whole class. What it keeps
// enforcing is the residue, which is not vacuous: a token below could still be
// *added* to a family by someone reading it as a failure mode, and then every
// honest report of it would be scored as an invention. That is the outcome the
// four historical violations produced ("Failed" via FailedMount, "Error" via
// ImagePullError, "NotReady" via PodsNotReady, "DeadlineExceeded" via
// ProgressDeadlineExceeded); only their mechanism has changed.
//
// The corpus is reasons Kubernetes or this agent actually writes that are *not*
// assertions of one of our families. Add to it whenever a live run produces a
// reason we would not want graded as a concrete failure claim; the test is only
// as good as the vocabulary it knows about.
func TestNoHonestReasonIsAFailureClaim(t *testing.T) {
	honest := []string{
		// Workload and pod conditions that describe readiness or progress,
		// not a cause.
		"PodsNotReady", "ContainersNotReady", "ReplicaFailure", "FailedCreate",
		"ProgressDeadlineExceeded", "ContainerCreating", "PodInitializing",
		"Terminating", "Completed", "ScalingReplicaSet", "SuccessfulCreate",
		// Probe and autoscaler vocabulary.
		"ReadinessProbeFailed", "LivenessProbeFailed", "FailedGetResourceMetric",
		"NotTriggerScaleUp",
		// Advisories the agent writes itself. hallucinated_fault must never
		// grade these, or thoroughness becomes a penalty.
		"MissingResourceLimits", "NoPodDisruptionBudget", "LatestImageTag",
		"MissingProbes", "NoNetworkPolicy", "SingleReplicaNoPDB",
		"IntentionalZeroReplicas", "SlowWebhookRisk",
		// Reference failures, which name the missing object rather than a
		// failure mode of the object reporting them.
		"SecretNotFound", "ConfigMapNotFound",
	}
	for _, reason := range honest {
		if fam := familyOf(reason); fam != "" {
			t.Errorf("familyOf(%q) = %q: an honest reason resolves to a failure "+
				"family, so reporting it will be scored as invention.", reason, fam)
		}
	}
}

// TestEveryFixtureStillNamesItsInjectedFamily is the guard on the one direction
// in which exact matching can do harm.
//
// HallucinatedFault.Score runs familyOf over a fixture's own Want.Reasons to
// learn which families it injected. Narrowing the matcher narrows that set too,
// and a fixture whose Want lists only near-miss spellings of its family's
// tokens would inject a fault nothing recognises — at which point a *correct*
// report of that fault is charged as an invention and the fixture's
// hallucinated_fault collapses for a reason that has nothing to do with the
// agent. That failure is silent: the score simply drops.
//
// So the map is pinned rather than derived. A new fixture fails this test until
// somebody writes down what it injects, which is the moment to notice that its
// Wants spell the family's token some other way.
//
// fault-none is absent on purpose and its empty entry says so: it injects
// nothing, which is what makes it the fixture that gives hallucinated_fault
// something to cost.
func TestEveryFixtureStillNamesItsInjectedFamily(t *testing.T) {
	want := map[string][]string{
		"fault-imagepull":     {"image-pull"},
		"fault-crashloop":     {"crash-loop"},
		"fault-oomkill":       {"oom"},
		"fault-unschedulable": {"resource-pressure"},
		"fault-failedjob":     {"job-failed"},
		"fault-badselector":   {"no-endpoints"},
		"fault-none":          nil,
		"fault-storefront":    {"image-pull", "job-failed", "resource-pressure"},
		"fault-sessions":      {"crash-loop", "no-endpoints"},
		"fault-ledger":        {"volume-binding"},
		// Empty for a different reason than fault-none, and the difference is
		// the point of the fixture. fault-none injects nothing; fault-invoicing
		// injects a real fault that Kubernetes has no vocabulary for, so its
		// Want reasons are all coined ones and none resolves to a family. The
		// consequence is that hallucinated_fault treats *every* concrete
		// failure claim in that namespace as an invention — which is correct
		// and is the second thing the fixture measures: an agent that reaches
		// for CrashLoopBackOff or OOMKilled to explain a workload that has
		// never restarted is guessing from a symptom it did not observe.
		"fault-invoicing": nil,
	}

	all := faults.All()
	if len(all) != len(want) {
		t.Errorf("the suite has %d fixtures and this table has %d entries; a fixture "+
			"was added or removed without recording what it injects", len(all), len(want))
	}
	for _, f := range all {
		expected, known := want[f.Name]
		if !known {
			t.Errorf("fixture %q is not in the table: record the families its Wants "+
				"resolve to, and check they are the ones it actually injects", f.Name)
			continue
		}
		seen := map[string]bool{}
		for _, w := range f.Want {
			for _, r := range w.Reasons {
				if fam := familyOf(r); fam != "" {
					seen[fam] = true
				}
			}
		}
		var got []string
		for fam := range seen {
			got = append(got, fam)
		}
		sort.Strings(got)
		if !slices.Equal(got, expected) {
			t.Errorf("fixture %q injects families %v, want %v — a Want reason stopped "+
				"resolving, so a correct report of this fault will be scored as invention",
				f.Name, got, expected)
		}
	}
}
