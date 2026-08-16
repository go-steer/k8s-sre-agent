package sre

import (
	"strings"
	"testing"

	"github.com/go-steer/k8s-sre-agent/internal/faults"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// report wraps findings in the minimum valid envelope, so a test can say what
// it is about without restating the consistency rule every time.
func report(findings ...schema.Finding) *schema.HealthReport {
	h := &schema.HealthReport{Summary: "s", Findings: findings}
	h.OverallSeverity = h.DerivedSeverity()
	return h
}

func finding(kind, name, reason string) schema.Finding {
	return schema.Finding{
		Severity: schema.SeverityWarning, Title: "t", Detail: "d",
		Namespace: "ns", Kind: kind, ResourceName: name, Reason: reason,
	}
}

// The recorded defect, hermetically. fault-ledger filed
// (PersistentVolumeClaim, ledger-data, Unschedulable) on four of five live
// runs; a PVC is not unschedulable, so the finding cannot be diffed against
// the next cycle's volume-binding finding.
func TestACrossLayerReasonIsRejected(t *testing.T) {
	problems := crossLayerReasons(report(
		finding("PersistentVolumeClaim", "ledger-data", "Unschedulable"),
	))
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1: %v", len(problems), problems)
	}
	got := problems[0]
	for _, want := range []string{"Unschedulable", "PersistentVolumeClaim", "Pod-level"} {
		if !strings.Contains(got, want) {
			t.Errorf("rejection %q does not mention %q", got, want)
		}
	}

	// The wording is the load-bearing half. The obvious way to satisfy a layer
	// check is to re-file the finding against the pod, which trades this defect
	// for the object-choice instability the finding diff already has to
	// survive. The positive control says the agent's own better answer keeps
	// the PVC finding and gives it the PVC's token, so the message must point
	// at the object it already named and nowhere else.
	if !strings.Contains(got, "this object's own failure mode") {
		t.Errorf("rejection %q does not ask for this object's own failure mode", got)
	}
	if strings.Contains(got, "report the pod") || strings.Contains(got, "the Pod instead") {
		t.Errorf("rejection %q tells the agent to move the finding, which makes the "+
			"object-choice instability worse", got)
	}
}

func TestCrossLayerReasonsAcceptsWhatTheControlPlaneActuallyWrites(t *testing.T) {
	// Every pairing here appeared in a live run or is the token Kubernetes
	// itself writes on that kind. The 17:44 run is the source of the first
	// three: it filed the pod and the PVC as separate findings, gave each its
	// own failure mode, and scored fault_recall 1.00.
	ok := []schema.Finding{
		finding("PersistentVolumeClaim", "ledger-data", "PVCPending"),
		finding("PersistentVolumeClaim", "ledger-data", "VolumeBindingFailed"),
		finding("Pod", "ledger-writer-77cb5bc896-jqk8m", "Unschedulable"),
		finding("Deployment", "ledger-writer", "RolloutIncomplete"),

		// Unmapped kinds pass whatever they carry: v1 covers only the kinds for
		// which the rejection cannot misfire, and Deployment/Unschedulable is
		// the arguable case v2 has to earn separately.
		finding("Deployment", "gemma4-vllm", "Unschedulable"),
		finding("Node", "kind-worker", "OOMKilled"),

		// Unknown tokens pass against any kind. An incomplete table must
		// under-enforce; genericReasons got the opposite wrong twice.
		finding("Service", "antrea", "IntentionalZeroReplicas"),
		finding("PersistentVolumeClaim", "ledger-data", "Pending"),
		finding("ConfigMap", "app-config", "MissingKey"),

		// A missing field is unidentifiedFindings' complaint, not this one.
		finding("PersistentVolumeClaim", "ledger-data", ""),
		finding("", "ledger-data", "Unschedulable"),
	}
	for _, f := range ok {
		if problems := crossLayerReasons(report(f)); len(problems) > 0 {
			t.Errorf("(%s, %s) was rejected: %v", f.Kind, f.Reason, problems)
		}
	}
}

func TestCrossLayerReasonsRejectsEveryImpossiblePairing(t *testing.T) {
	bad := []schema.Finding{
		// A Service cannot crash, be scheduled, or be rolled out.
		finding("Service", "frontend", "CrashLoopBackOff"),
		finding("Service", "frontend", "FailedScheduling"),
		finding("svc", "frontend", "ProgressDeadlineExceeded"),
		// ConfigMap and Secret have no status subresource at all, so nothing
		// writes any of this on them.
		finding("ConfigMap", "app-config", "OOMKilled"),
		finding("Secret", "db-creds", "ImagePullBackOff"),
		// A volume does not run containers and is not scheduled.
		finding("PersistentVolumeClaim", "ledger-data", "FailedMount"),
		finding("PersistentVolume", "pvc-9c1f", "Evicted"),
		finding("pvc", "ledger-data", "BackoffLimitExceeded"),
		// Spelling is not the contract: the token is compared normalized, so a
		// separator or a case change does not slip a borrow through.
		finding("PersistentVolumeClaim", "ledger-data", "unschedulable"),
		finding("PersistentVolumeClaim", "ledger-data", "UNSCHEDULABLE"),
		finding("PersistentVolumeClaim", "ledger-data", "crash-loop-back-off"),
	}
	for _, f := range bad {
		if problems := crossLayerReasons(report(f)); len(problems) != 1 {
			t.Errorf("(%s, %s) was accepted", f.Kind, f.Reason)
		}
	}
}

// Substring matching is how internal/evals resolved `Failed` to `FailedMount`
// and `Error` to `ImagePullError`, twice in one session. Membership here is on
// the exact normalized token, so a longer token that merely contains a mapped
// one is unknown rather than misclassified.
func TestReasonMembershipIsExactNotSubstring(t *testing.T) {
	for _, reason := range []string{"UnschedulableAfterAll", "PreUnschedulable", "NotEvicted"} {
		if problems := crossLayerReasons(report(finding("PersistentVolumeClaim", "d", reason))); len(problems) > 0 {
			t.Errorf("%q was resolved to a mapped token by substring: %v", reason, problems)
		}
	}
}

// The three shapes measured against the real cluster, plus the well-formed
// answer the agent gave for the same class of finding on the next run.
func TestUnnamedResourcesRejectsWhatCannotBeFingerprinted(t *testing.T) {
	fourteen := "adservice,cartservice,checkoutservice,currencyservice,emailservice," +
		"frontend,loadgenerator,paymentservice,productcatalogservice,recommendationservice," +
		"redis-cart,shippingservice,gemma4-vllm,gemma4-proxy"

	for _, tc := range []struct {
		name string
		want bool // want a rejection
		val  string
	}{
		{"comma-joined deployments", true, fourteen},
		{"prose in place of a name", true, "(unspecified — 1 pod/1 container per top.unlimited scan)"},
		{"slash-joined webhooks", true, "a-webhook / b-webhook / c-webhook"},
		{"namespace-qualified", true, "online-boutique/frontend"},
		{"a target rather than a name", true, "Deployment/online-boutique/frontend"},
		{"a count", true, "3 pods"},
		{"trailing separator", true, "frontend,"},
		{"newline-joined", true, "frontend\nbackend"},

		{"a pod name", false, "emailservice-6597bbfdbb-j5lpq"},
		{"a namespace", false, "online-boutique"},
		{"a node name", false, "gke-simian-test-default-pool-3f2a1b9c-x1yz"},
		{"a dotted webhook name", false, "warden-validating.common-webhooks.networking.gke.io"},
		{"a statefulset pod ordinal", false, "web-0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			problems := unnamedResources(report(finding("Deployment", tc.val, "RolloutIncomplete")))
			if got := len(problems) > 0; got != tc.want {
				t.Errorf("rejected=%v, want %v (value %q, problems %v)", got, tc.want, tc.val, problems)
			}
		})
	}

	// Empty is unidentifiedFindings' complaint. Two checks naming the same
	// field in one message spends a round trip saying it twice.
	if problems := unnamedResources(report(finding("Deployment", "", "RolloutIncomplete"))); len(problems) > 0 {
		t.Errorf("an absent resource_name should be left to unidentifiedFindings: %v", problems)
	}
}

// The rejection has to echo the field back — the model is holding several
// findings and has to know which — but a fourteen-name join quoted in full
// would be most of the message.
func TestTheRejectionEchoesALongValueTruncated(t *testing.T) {
	long := strings.Repeat("service-name,", 20)
	problems := unnamedResources(report(finding("Deployment", long, "RolloutIncomplete")))
	if len(problems) != 1 {
		t.Fatalf("got %d problems, want 1", len(problems))
	}
	got := problems[0]
	if strings.Contains(got, long) {
		t.Error("the whole offending value was echoed into the error")
	}
	if !strings.Contains(got, "service-name,service-name") {
		t.Errorf("the error does not show the model which value is meant: %q", got)
	}
	if !strings.Contains(got, "one finding per object") {
		t.Errorf("the error does not say what to do instead: %q", got)
	}
}

// Both checks travel through the submission path, because that is the only
// place they can affect a run — and both must be retryable errors the model can
// act on rather than Go errors the harness discovers later.
func TestSubmitHealthReportRejectsUnusableIdentityFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		f    map[string]any
		want string
	}{{
		name: "a borrowed reason",
		f: map[string]any{
			"severity": "critical", "title": "PVC ledger-data stuck Pending",
			"detail": "referenced StorageClass fast-ssd-encrypted does not exist",
			"kind":   "PersistentVolumeClaim", "resource_name": "ledger-data",
			"reason": "Unschedulable",
		},
		want: "this object's own failure mode",
	}, {
		name: "a list of names",
		f: map[string]any{
			"severity": "warning", "title": "GitOps drift on every Deployment",
			"detail": "a second field manager is writing to all of them",
			"kind":   "Deployment", "resource_name": "frontend,cartservice,adservice",
			"reason": "FieldManagerConflict",
		},
		want: "one finding per object",
	}} {
		t.Run(tc.name, func(t *testing.T) {
			out := runReportTool(t, map[string]any{
				"overall_severity": tc.f["severity"],
				"summary":          "a summary",
				"findings":         []any{tc.f},
			})
			errText, ok := out["error"].(string)
			if !ok {
				t.Fatalf("the report was accepted: %v", out)
			}
			if !strings.Contains(errText, tc.want) {
				t.Errorf("error %q does not mention %q", errText, tc.want)
			}
			if !strings.Contains(errText, ReportToolName) {
				t.Errorf("rejection %q does not tell the model to call %s again", errText, ReportToolName)
			}
		})
	}
}

// The defect the second signal was built for, hermetically. Measured on the
// 2026-08-14 21:30 tier-2 run: fault-sessions scored fault_recall 0.50 with
// `missed=Service/session-store` while having filed that exact Service, because
// the reason it gave named the pods behind it. The agent coined the token, so
// reasonLayers could not contain it and never will.
func TestACoinedReasonThatNamesAnotherObjectIsRejected(t *testing.T) {
	problems := crossLayerReasons(report(finding("Service", "session-store", "PodsNotReady")))
	if len(problems) != 1 {
		t.Fatalf("crossLayerReasons = %v, want the one rejection", problems)
	}
	// The remedy has to be "describe this object", never "move the finding to
	// the pod" — re-filing trades this defect for the object-choice instability
	// the finding diff already has to survive.
	if strings.Contains(problems[0], "Pod/") || strings.Contains(strings.ToLower(problems[0]), "file it against") {
		t.Errorf("the rejection tells the model to re-file against the pod: %q", problems[0])
	}
	if !strings.Contains(problems[0], "Name this object's own failure mode") {
		t.Errorf("the rejection does not say what to do instead: %q", problems[0])
	}
}

func TestTheLeadingNounRuleRejectsBorrowsAndNothingElse(t *testing.T) {
	bad := []schema.Finding{
		// The subject is another layer's object and it is the first word.
		finding("Service", "session-store", "PodsNotReady"),
		finding("Service", "session-store", "PodsCrashLooping"),
		finding("Service", "frontend", "ContainersNotReady"),
		finding("PersistentVolumeClaim", "ledger-data", "PodUnschedulable"),
		finding("PersistentVolumeClaim", "ledger-data", "PodPending"),
		finding("Secret", "db-creds", "PodsNotReady"),
		finding("ConfigMap", "app-config", "DeploymentRolloutStuck"),
		finding("Ingress", "web", "NodeNotReady"),
		// Spelling is not the contract here either.
		finding("Service", "session-store", "pods_not_ready"),
		finding("svc", "session-store", "POD-NOT-READY"),
	}
	for _, f := range bad {
		if problems := crossLayerReasons(report(f)); len(problems) != 1 {
			t.Errorf("(%s, %s) was accepted", f.Kind, f.Reason)
		}
	}
}

// The guard that matters more than the rule. Every over-rejection this repo has
// shipped — genericReasons twice, familyOf four times — was an honest token
// scored as a lie, and a new refusal is the most likely place to do it again.
//
// Every entry is a reason a recorded run produced or a fixture accepts, paired
// with a kind it would plausibly be filed on. The rule must let all of it
// through.
func TestTheLeadingNounRuleAcceptsHonestVocabulary(t *testing.T) {
	ok := []schema.Finding{
		// The distinction the whole cut rests on: these name other layers and
		// are about the object filing them. Both are fixture-accepted answers —
		// fault-badselector and cascade would stop scoring if either were
		// refused.
		finding("Service", "frontend", "NoMatchingPods"),
		finding("Service", "session-store", "NoReadyEndpoints"),
		finding("Service", "frontend", "ServiceHasNoEndpoints"),
		finding("Service", "frontend", "SelectorMismatch"),
		finding("Service", "session-store", "NoHealthyBackends"),
		finding("Service", "antrea", "IntentionalZeroReplicas"),

		// A reference failure is this object's own problem, however it is
		// spelled and whichever kind it names.
		finding("PersistentVolumeClaim", "ledger-data", "StorageClassNotFound"),
		finding("PersistentVolumeClaim", "ledger-data", "MissingStorageClass"),
		finding("Ingress", "web", "ServiceNotFound"),
		finding("Ingress", "web", "SecretNotFound"),
		finding("ConfigMap", "app-config", "ConfigMapNotFound"),
		// The near-miss the corpus turned up: opens with "pod", is about a
		// policy object, and the reference-failure suffix is what saves it.
		finding("Service", "frontend", "PodDisruptionBudgetMissing"),
		finding("Service", "frontend", "NoPodDisruptionBudget"),

		// A noun that names this object's own layer is self-description.
		finding("PersistentVolumeClaim", "ledger-data", "PVCPending"),
		finding("PersistentVolumeClaim", "ledger-data", "VolumeBindingFailed"),
		finding("Ingress", "web", "ServiceUnavailable"),
		finding("Service", "session-store", "EndpointsNotReady"),

		// Coined tokens that name no kind at all pass, which is the property
		// that keeps the polarity open-world. These five are fault-invoicing's
		// accepted answers; a Pod is unmapped today, but they must also survive
		// on a kind that is mapped, or widening resourceLayers later would
		// silently break the fixture.
		finding("Service", "invoice-reconciler", "ConnectionRefused"),
		finding("Service", "invoice-reconciler", "ConnectionFailure"),
		finding("Service", "invoice-reconciler", "Unreachable"),
		finding("Service", "invoice-reconciler", "DependencyFailure"),
		finding("Service", "invoice-reconciler", "DependencyMissing"),

		// Advisory vocabulary from recorded runs.
		finding("Secret", "db-creds", "UnusedSecret"),
		finding("ConfigMap", "app-config", "MissingKey"),
		finding("Service", "frontend", "SlowWebhookRisk"),
		finding("Service", "frontend", "SingleReplicaNoPDB"),
	}
	for _, f := range ok {
		if problems := crossLayerReasons(report(f)); len(problems) > 0 {
			t.Errorf("(%s, %s) was rejected — an honest reason refused at submission "+
				"costs a round trip and, three times over, the run: %v", f.Kind, f.Reason, problems)
		}
	}
}

// A bare kind noun is not a borrow claim. It identifies no failure mode either,
// but that is a different complaint and this check is not calibrated to make
// it — firing here would produce a rejection whose remedy the message does not
// describe.
func TestABareKindNounIsNotRejectedHere(t *testing.T) {
	for _, reason := range []string{"Pod", "Pods", "Node", "Service"} {
		if problems := crossLayerReasons(report(finding("PersistentVolumeClaim", "d", reason))); len(problems) > 0 {
			t.Errorf("bare %q was rejected by the layer check: %v", reason, problems)
		}
	}
}

// The strongest available statement that the check cannot break the suite: no
// answer any tier-2 fixture accepts may be refused at submission. A fixture
// grades a report the agent has to get *through* this gate first, so a rule
// that rejects a fixture's own right answer makes that fixture unscoreable
// while looking like a model failure.
func TestNoFixtureAnswerIsRefusedAtSubmission(t *testing.T) {
	for _, fx := range faults.All() {
		for _, w := range fx.Want {
			kinds := append([]string{w.Kind}, w.AlsoAcceptKinds...)
			for _, kind := range kinds {
				for _, reason := range w.Reasons {
					f := finding(kind, w.Name, reason)
					if problems := crossLayerReasons(report(f)); len(problems) > 0 {
						t.Errorf("%s: (%s, %s) is an accepted answer and would be rejected "+
							"at submission: %v", fx.Name, kind, reason, problems)
					}
				}
			}
		}
	}
}

// The recorded vocabulary, swept against every kind the check is armed for.
//
// These are the seventeen distinct reason tokens four saved tier-2 runs
// produced, counted before the transcripts aged out of /tmp. Sweeping each
// against all six mapped kinds is stronger than replaying the actual pairings:
// it asks not "did this rule fire on a recorded report" but "could it, on any
// object one of these tokens could be filed against".
//
// The expected set is written out rather than counted, because the value of
// this test is the disagreement. A token moving between the two halves is
// either a table edit somebody meant or the beginning of the over-rejection
// this whole file is nervous about, and only naming them tells the two apart.
func TestTheRecordedVocabularySweep(t *testing.T) {
	// Tokens that must be refused on a mapped kind, with why. Every one is a
	// workload's failure mode on an object that runs no workload.
	refused := map[string]bool{
		"BackoffLimitExceeded": true, // a Job's, and none of these is a Job
		"CrashLoopBackOff":     true, // a container's
		"ImagePullBackOff":     true, // a container's
		"OOMKilled":            true, // a container's
		"Unschedulable":        true, // a Pod's — the token v1 was built for
		"PodsNotReady":         true, // the coined borrow v2 was built for
		"PVCPending":           true, // a claim's, refused on the four non-volume kinds
	}
	// Everything else a recorded run wrote. None of it may be refused on any
	// mapped kind: these are advisories and coined descriptions, and a
	// rejection here is a round trip spent telling the model its honest answer
	// is wrong.
	accepted := []string{
		"DataDependencyMissing", "ExcessiveRestarts", "MissingPDB", "MissingProbes",
		"NoReadyEndpoints", "NoServiceDefined", "RolloutIncomplete", "SelectorMismatch",
		"SingleReplica", "SuspectImage",
	}

	mapped := []string{"PersistentVolumeClaim", "PersistentVolume", "Service", "Ingress", "ConfigMap", "Secret"}
	for _, reason := range accepted {
		for _, kind := range mapped {
			if problems := crossLayerReasons(report(finding(kind, "x", reason))); len(problems) > 0 {
				t.Errorf("(%s, %s) refused, but a live run wrote this token and it names no "+
					"other object's state: %v", kind, reason, problems)
			}
		}
	}
	for reason := range refused {
		var fired int
		for _, kind := range mapped {
			if problems := crossLayerReasons(report(finding(kind, "x", reason))); len(problems) > 0 {
				fired++
			}
		}
		if fired == 0 {
			t.Errorf("(%s) is refused on no mapped kind; the table that caught it was edited", reason)
		}
	}
	// PVCPending is the one token whose answer depends on the kind, and it is
	// the shape of the whole check: right on a claim, a borrow anywhere else.
	if problems := crossLayerReasons(report(finding("PersistentVolumeClaim", "ledger-data", "PVCPending"))); len(problems) > 0 {
		t.Errorf("PVCPending refused on a claim, which is the answer the 17:44 run got right: %v", problems)
	}
	if problems := crossLayerReasons(report(finding("Service", "session-store", "PVCPending"))); len(problems) != 1 {
		t.Error("PVCPending accepted on a Service")
	}
}
