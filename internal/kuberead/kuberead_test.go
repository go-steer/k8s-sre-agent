package kuberead

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"
)

// fakeRunner stands in for kubectl. Nothing in this package's tests can reach a
// cluster, for the reason kubewrite's fake exists: a test suite that touches a
// machine with 64 kube contexts is a test suite nobody runs.
type fakeRunner struct {
	calls  [][]string
	stdout string
	stderr string
	err    error
}

func (r *fakeRunner) Output(_ context.Context, args ...string) (string, string, error) {
	r.calls = append(r.calls, slices.Clone(args))
	return r.stdout, r.stderr, r.err
}

// runnableTool is ADK's unexported tool.runnableTool, restated so a test can
// invoke a tool the way ADK does — through the JSON-schema decode rather than
// by calling the handler directly.
type runnableTool interface {
	tool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx adkagent.Context, args any) (map[string]any, error)
}

// testCtx is the lenient mock, not StrictContextMock: functiontool.Run reads
// ctx.ToolConfirmation() before it decodes arguments or reaches the handler
// (tool/functiontool/function.go:202), so a mock that panics on every
// un-overridden method never gets as far as the tool under test.
func testCtx(*testing.T) adkagent.Context { return &adkagent.ContextMock{} }

// kit builds the live toolset over a fake runner and returns the one tool.
func kit(t *testing.T, run *fakeRunner, cfg Config) runnableTool {
	t.Helper()
	cfg.Runner = run
	ts, err := Toolset(cfg)
	if err != nil {
		t.Fatalf("Toolset: %v", err)
	}
	tools, err := ts.Tools(nil)
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	if len(tools) != 1 {
		t.Fatalf("toolset exposes %d tools, want 1", len(tools))
	}
	r, ok := tools[0].(runnableTool)
	if !ok {
		t.Fatalf("tool %q is not runnable (%T)", tools[0].Name(), tools[0])
	}
	return r
}

// list invokes the tool and decodes its Listing.
func list(t *testing.T, tl runnableTool, args map[string]any) Listing {
	t.Helper()
	raw, err := tl.Run(testCtx(t), args)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	blob, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Listing
	if err := json.Unmarshal(blob, &out); err != nil {
		t.Fatalf("decode %v: %v", raw, err)
	}
	return out
}

// items wraps object bodies in the v1.List envelope kubectl produces for a
// multi-kind `get`.
func items(bodies ...string) string {
	return `{"apiVersion":"v1","kind":"List","items":[` + strings.Join(bodies, ",") + `]}`
}

func pod(name, phase string, ready bool, restarts int, node string) string {
	return fmt.Sprintf(`{"kind":"Pod","metadata":{"name":%q},
		"spec":{"nodeName":%q,"containers":[{"name":"app"}]},
		"status":{"phase":%q,"containerStatuses":[{"ready":%t,"restartCount":%d}]}}`,
		name, node, phase, ready, restarts)
}

// TestItListsTheNamespaceFaultBadselectorFailedOn is the regression this whole
// package exists for.
//
// The fixture is that fault verbatim: a Service whose selector matches nothing,
// in front of a Deployment that is perfectly healthy. Every health scan
// correctly reports the namespace clean and names no objects, so the agent
// spent fifteen to nineteen calls guessing `api`, `web`, `app`, `backend` — and
// the Deployment is called `frontend`. What this asserts is not that the tool
// diagnoses the fault (it must not; see the package doc) but that one call
// hands the agent the two names it needs and the one number that should make it
// look closer.
func TestItListsTheNamespaceFaultBadselectorFailedOn(t *testing.T) {
	run := &fakeRunner{stdout: items(
		`{"kind":"Deployment","metadata":{"name":"frontend"},
		  "spec":{"replicas":2},"status":{"readyReplicas":2}}`,
		pod("frontend-6d4f-abc12", "Running", true, 0, "node-1"),
		pod("frontend-6d4f-def34", "Running", true, 0, "node-2"),
		`{"kind":"Service","metadata":{"name":"frontend"},
		  "spec":{"type":"ClusterIP","ports":[{"port":80,"protocol":"TCP"}]}}`,
		`{"kind":"Endpoints","metadata":{"name":"frontend"},"subsets":[]}`,
	)}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "fault-badselector"})

	if got.Error != "" {
		t.Fatalf("Error = %q, want none", got.Error)
	}
	if got.Count != 5 {
		t.Errorf("Count = %d, want 5", got.Count)
	}
	for _, want := range []string{
		// The target form the other read tools take, verbatim. This is the
		// payload: the agent could not name `frontend` before.
		"Deployment/fault-badselector/frontend ready=2/2",
		"Service/fault-badselector/frontend type=ClusterIP ports=80/TCP",
		// Zero endpoints behind a Service with two ready Pods is the thread the
		// agent should pull, and it is a field `kubectl get endpoints` prints —
		// so reporting it is inventory, not diagnosis.
		"Endpoints/fault-badselector/frontend addresses=0",
		"Pod/fault-badselector/frontend-6d4f-abc12 phase=Running ready=1/1 restarts=0 node=node-1",
	} {
		if !strings.Contains(got.Objects, want) {
			t.Errorf("listing does not contain %q:\n%s", want, got.Objects)
		}
	}
}

// TestItDoesNotDiagnose. The complement of the test above, and the more
// important half: a tool built to make one fixture pass measures the fixture.
// The selector is what makes fault-badselector broken, and finding it is
// k8s_state_edges's job.
func TestItDoesNotDiagnose(t *testing.T) {
	run := &fakeRunner{stdout: items(
		`{"kind":"Service","metadata":{"name":"frontend"},
		  "spec":{"type":"ClusterIP","selector":{"app":"frontend-v2"},
		  "ports":[{"port":80}]}}`,
	)}
	// The namespace here is deliberately not "fault-badselector": the fixture's
	// own name contains the word this test forbids.
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "shop"})
	for _, forbidden := range []string{"selector", "frontend-v2", "mismatch"} {
		if strings.Contains(strings.ToLower(got.Objects+got.Note), forbidden) {
			t.Errorf("listing volunteers %q; it is an inventory, not a diagnosis:\n%s", forbidden, got.Objects)
		}
	}
}

// TestSecretValuesNeverAppear. lookout's checks are secret-safe by design and
// one careless line here would undo that for the whole read path — an
// enumeration tool sees every Secret in the namespace and its output goes
// straight into a model's context and from there into a health report.
func TestSecretValuesNeverAppear(t *testing.T) {
	const plaintext = "hunter2-do-not-print"
	encoded := base64.StdEncoding.EncodeToString([]byte(plaintext))
	run := &fakeRunner{stdout: items(
		fmt.Sprintf(`{"kind":"Secret","metadata":{"name":"db-creds"},"type":"Opaque",
		  "data":{"password":%q,"username":%q}}`, encoded, encoded),
	)}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "prod"})

	whole := got.Objects + got.Note + got.Error
	if strings.Contains(whole, plaintext) || strings.Contains(whole, encoded) {
		t.Fatalf("a Secret value reached the listing:\n%s", whole)
	}
	if want := "Secret/prod/db-creds type=Opaque keys=2"; !strings.Contains(got.Objects, want) {
		t.Errorf("listing does not contain %q:\n%s", want, got.Objects)
	}
}

// TestEveryLineOpensWithATargetTheOtherToolsAccept. The format is the contract:
// the point of enumerating is that the names come back in the exact shape
// k8s_state_edges, k8s_resource_spec and k8s_triage_workload demand, so the
// agent can copy one across without composing it.
func TestEveryLineOpensWithATargetTheOtherToolsAccept(t *testing.T) {
	run := &fakeRunner{stdout: items(
		`{"kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":1},"status":{"readyReplicas":0}}`,
		pod("web-1", "Pending", false, 0, ""),
		`{"kind":"ServiceAccount","metadata":{"name":"default"}}`,
		`{"kind":"ConfigMap","metadata":{"name":"app-config"},"data":{"LOG_LEVEL":"debug"}}`,
	)}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "shop"})

	for _, line := range strings.Split(got.Objects, "\n") {
		target, _, _ := strings.Cut(line, " ")
		parts := strings.Split(target, "/")
		if len(parts) != 3 {
			t.Errorf("line %q does not open with <Kind>/<namespace>/<name>", line)
			continue
		}
		if parts[1] != "shop" {
			t.Errorf("line %q names namespace %q, want \"shop\"", line, parts[1])
		}
		if parts[0] == "" || parts[2] == "" {
			t.Errorf("line %q has an empty kind or name", line)
		}
	}
	// An object with no interesting status is its name alone — existence is the
	// fact for a ServiceAccount, and a trailing empty status would be noise.
	if !strings.Contains(got.Objects, "ServiceAccount/shop/default\n") &&
		!strings.HasSuffix(got.Objects, "ServiceAccount/shop/default") {
		t.Errorf("ServiceAccount line carries a status it should not:\n%s", got.Objects)
	}
	if want := "Pod/shop/web-1 phase=Pending ready=0/1 restarts=0"; !strings.Contains(got.Objects, want) {
		// A Pending pod has no node and no waiting reason, and printing `node=`
		// empty on every one of them is paid for by the token.
		t.Errorf("listing does not contain %q:\n%s", want, got.Objects)
	}
}

// TestTruncationIsAnnounced. A truncated inventory that does not say it is
// truncated is worse than no inventory, because the absence of an object reads
// as evidence that it does not exist.
func TestTruncationIsAnnounced(t *testing.T) {
	bodies := make([]string, 0, 10)
	for i := range 10 {
		bodies = append(bodies, pod(fmt.Sprintf("web-%d", i), "Running", true, 0, "node-1"))
	}
	run := &fakeRunner{stdout: items(bodies...)}
	got := list(t, kit(t, run, Config{MaxObjects: 3}), map[string]any{"namespace": "shop"})

	if n := len(strings.Split(got.Objects, "\n")); n != 3 {
		t.Errorf("listed %d objects, want 3", n)
	}
	if got.Count != 10 {
		t.Errorf("Count = %d, want the true total 10", got.Count)
	}
	if !strings.Contains(got.Note, "truncated") || !strings.Contains(got.Note, "10") {
		t.Errorf("Note = %q, want it to say 3 of 10 were shown", got.Note)
	}
}

// TestARefusalIsAResultNotAnError, for the reason kubewrite returns refusals
// the same way: a tool error reads to a model as an accident worth retrying,
// and "that is not a namespace name" is a fact it should report instead.
func TestARefusalIsAResultNotAnError(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"empty namespace", map[string]any{"namespace": ""}},
		{"flag as namespace", map[string]any{"namespace": "--all-namespaces"}},
		{"uppercase namespace", map[string]any{"namespace": "Prod"}},
		{"path as namespace", map[string]any{"namespace": "../kube-system"}},
		{"flag as kind", map[string]any{"namespace": "shop", "kinds": []any{"--all"}}},
		{"path as kind", map[string]any{"namespace": "shop", "kinds": []any{"pods", "a/b"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &fakeRunner{stdout: items()}
			got := list(t, kit(t, run, Config{}), tc.args)
			if got.Error == "" {
				t.Errorf("accepted %v; want a refusal", tc.args)
			}
			if len(run.calls) != 0 {
				t.Errorf("kubectl ran anyway: %v", run.calls)
			}
		})
	}
}

// TestKubectlFailureIsReportedNotThrown. Same reasoning, one layer out: the
// namespace being unreachable is something the agent should say in its report.
func TestKubectlFailureIsReportedNotThrown(t *testing.T) {
	run := &fakeRunner{
		stderr: "Error from server (Forbidden): pods is forbidden\nUser \"eval\" cannot list resource",
		err:    fmt.Errorf("exit status 1"),
	}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "prod"})
	if got.Error == "" {
		t.Fatal("a kubectl failure produced no Error")
	}
	if !strings.Contains(got.Error, "Forbidden") {
		t.Errorf("Error = %q; it should carry kubectl's own words", got.Error)
	}
	if strings.Contains(got.Error, "\n") {
		t.Errorf("Error spans lines: %q", got.Error)
	}
}

// TestAPartialListingKeepsWhatItGotAndSaysWhatItMissed.
//
// kubectl lists most kinds and is refused one — an RBAC role without secrets is
// the usual shape — printing the refusal on stderr, the rest on stdout, and
// exiting non-zero. Dropping the whole listing would be wrong, and keeping it
// silently would be worse: the difference between "there is no Secret here" and
// "I was not allowed to look" is the difference between a finding and a blind
// spot.
func TestAPartialListingKeepsWhatItGotAndSaysWhatItMissed(t *testing.T) {
	run := &fakeRunner{
		stdout: items(pod("web-1", "Running", true, 0, "node-1")),
		stderr: "Error from server (Forbidden): secrets is forbidden",
		err:    fmt.Errorf("exit status 1"),
	}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "prod"})
	if got.Error != "" {
		t.Errorf("Error = %q; a partial listing is not a failed one", got.Error)
	}
	if !strings.Contains(got.Objects, "Pod/prod/web-1") {
		t.Errorf("threw away the objects it did get:\n%s", got.Objects)
	}
	if !strings.Contains(got.Note, "Forbidden") {
		t.Errorf("Note = %q, want it to carry the refusal", got.Note)
	}
}

// TestAnEmptyListingSaysWhyItMightBeEmpty. "No objects" and "no such namespace"
// are different facts and kubectl reports them identically, so the note has to
// carry the ambiguity rather than let the agent resolve it silently.
func TestAnEmptyListingSaysWhyItMightBeEmpty(t *testing.T) {
	got := list(t, kit(t, &fakeRunner{stdout: ""}, Config{}), map[string]any{"namespace": "gone"})
	if got.Error != "" {
		t.Errorf("Error = %q; an empty namespace is not an error", got.Error)
	}
	if got.Count != 0 || got.Objects != "" {
		t.Errorf("Count = %d, Objects = %q, want empty", got.Count, got.Objects)
	}
	if !strings.Contains(got.Note, "does not exist") {
		t.Errorf("Note = %q, want it to raise the possibility the namespace is absent", got.Note)
	}
}

// TestOneCallEnumeratesEverything. The whole default set goes in a single
// kubectl invocation: eighteen subprocesses would be eighteen round trips of
// latency on the tool call that now precedes every namespace investigation.
func TestOneCallEnumeratesEverything(t *testing.T) {
	run := &fakeRunner{stdout: items()}
	list(t, kit(t, run, Config{}), map[string]any{"namespace": "shop"})

	if len(run.calls) != 1 {
		t.Fatalf("ran %d kubectl invocations, want 1: %v", len(run.calls), run.calls)
	}
	got := run.calls[0]
	want := []string{"get", strings.Join(defaultKinds, ","), "-n", "shop", "-o", "json", "--ignore-not-found"}
	if !slices.Equal(got, want) {
		t.Errorf("args:\n got %v\nwant %v", got, want)
	}
	// ReplicaSets are out of the default set on purpose: a namespace accumulates
	// one per Deployment revision and they would routinely be the bulk of a
	// truncated listing.
	if slices.Contains(defaultKinds, "replicasets") {
		t.Error("replicasets are in the default set; they will crowd out the workloads")
	}
}

// TestKindsNarrowsTheListing, including the escape hatch for the kinds the
// default set leaves out.
func TestKindsNarrowsTheListing(t *testing.T) {
	run := &fakeRunner{stdout: items()}
	list(t, kit(t, run, Config{}), map[string]any{
		"namespace": "shop", "kinds": []any{"ReplicaSets", " pods "},
	})
	if len(run.calls) != 1 {
		t.Fatalf("ran %d invocations, want 1", len(run.calls))
	}
	// Normalised, not rejected: a model that writes "ReplicaSets" means
	// replicasets, and kubectl resolves singular, plural and short names itself
	// rather than this package carrying that table.
	if got := run.calls[0][1]; got != "replicasets,pods" {
		t.Errorf("kinds = %q, want %q", got, "replicasets,pods")
	}
}

// TestASingleKindListStillNamesItsKind. `kubectl get pods -o json` returns a
// PodList whose items carry no kind of their own, while `get a,b -o json`
// returns a v1.List whose items do. Both have to produce a usable target, and
// the single-kind shape is exactly what a caller passing one entry in `kinds`
// gets back.
func TestASingleKindListStillNamesItsKind(t *testing.T) {
	run := &fakeRunner{stdout: `{"apiVersion":"v1","kind":"PodList","items":[
		{"metadata":{"name":"web-1"},"spec":{"containers":[{"name":"app"}]},
		 "status":{"phase":"Running","containerStatuses":[{"ready":true,"restartCount":0}]}}]}`}
	got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "shop", "kinds": []any{"pods"}})
	if want := "Pod/shop/web-1 phase=Running"; !strings.Contains(got.Objects, want) {
		t.Errorf("listing does not contain %q:\n%s", want, got.Objects)
	}
}

// TestStatusLinesPerKind covers the formatter table. The rule for what belongs
// in one is mechanical — the fields `kubectl get <kind>` prints in its own
// default table — which is what keeps this an inventory and settles the awkward
// cases by rule rather than by taste.
func TestStatusLinesPerKind(t *testing.T) {
	cases := []struct{ name, body, want string }{
		{"deployment mid-rollout",
			`{"kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":3},"status":{"readyReplicas":1}}`,
			"Deployment/ns/web ready=1/3"},
		{"deployment scaled to zero",
			`{"kind":"Deployment","metadata":{"name":"web"},"spec":{"replicas":0},"status":{}}`,
			"Deployment/ns/web ready=0/0"},
		{"statefulset",
			`{"kind":"StatefulSet","metadata":{"name":"db"},"spec":{"replicas":3},"status":{"readyReplicas":3}}`,
			"StatefulSet/ns/db ready=3/3"},
		{"daemonset",
			`{"kind":"DaemonSet","metadata":{"name":"agent"},"status":{"numberReady":2,"desiredNumberScheduled":3}}`,
			"DaemonSet/ns/agent ready=2/3"},
		{"crashlooping pod",
			`{"kind":"Pod","metadata":{"name":"api-1"},"spec":{"nodeName":"n1","containers":[{"name":"app"}]},
			  "status":{"phase":"Running","containerStatuses":[
			    {"ready":false,"restartCount":7,"state":{"waiting":{"reason":"CrashLoopBackOff"}}}]}}`,
			"Pod/ns/api-1 phase=Running ready=0/1 restarts=7 state=CrashLoopBackOff node=n1"},
		{"unscheduled pod has no containerStatuses",
			`{"kind":"Pod","metadata":{"name":"api-2"},"spec":{"containers":[{"name":"app"},{"name":"sidecar"}]},
			  "status":{"phase":"Pending"}}`,
			"Pod/ns/api-2 phase=Pending ready=0/2 restarts=0"},
		{"failed job",
			`{"kind":"Job","metadata":{"name":"nightly"},"spec":{"completions":1},"status":{"failed":4}}`,
			"Job/ns/nightly completions=0/1 failed=4"},
		{"job defaults to one completion",
			`{"kind":"Job","metadata":{"name":"once"},"spec":{},"status":{"succeeded":1}}`,
			"Job/ns/once completions=1/1"},
		{"suspended cronjob quotes its schedule",
			`{"kind":"CronJob","metadata":{"name":"report"},"spec":{"schedule":"*/5 * * * *","suspend":true},"status":{}}`,
			`CronJob/ns/report schedule="*/5 * * * *" suspend=true active=0`},
		{"multi-port service",
			`{"kind":"Service","metadata":{"name":"web"},"spec":{"type":"NodePort",
			  "ports":[{"port":80},{"port":443,"protocol":"TCP"}]}}`,
			"Service/ns/web type=NodePort ports=80/TCP,443/TCP"},
		{"endpoints across subsets",
			`{"kind":"Endpoints","metadata":{"name":"web"},"subsets":[
			  {"addresses":[{"ip":"10.0.0.1"},{"ip":"10.0.0.2"}]},{"addresses":[{"ip":"10.0.0.3"}]}]}`,
			"Endpoints/ns/web addresses=3"},
		{"ingress",
			`{"kind":"Ingress","metadata":{"name":"public"},"spec":{"ingressClassName":"nginx",
			  "rules":[{"host":"shop.example.com"},{"host":"www.example.com"}]}}`,
			"Ingress/ns/public class=nginx hosts=shop.example.com,www.example.com"},
		{"configmap counts binary keys too",
			`{"kind":"ConfigMap","metadata":{"name":"cfg"},"data":{"a":"1","b":"2"},"binaryData":{"c":"AA=="}}`,
			"ConfigMap/ns/cfg keys=3"},
		{"pending pvc",
			`{"kind":"PersistentVolumeClaim","metadata":{"name":"data"},
			  "spec":{"storageClassName":"standard"},"status":{"phase":"Pending"}}`,
			"PersistentVolumeClaim/ns/data phase=Pending class=standard"},
		{"bound pvc",
			`{"kind":"PersistentVolumeClaim","metadata":{"name":"data"},"spec":{"storageClassName":"standard"},
			  "status":{"phase":"Bound","capacity":{"storage":"20Gi"}}}`,
			"PersistentVolumeClaim/ns/data phase=Bound capacity=20Gi class=standard"},
		{"hpa",
			`{"kind":"HorizontalPodAutoscaler","metadata":{"name":"web"},
			  "spec":{"minReplicas":2,"maxReplicas":10,"scaleTargetRef":{"kind":"Deployment","name":"web"}},
			  "status":{"currentReplicas":10}}`,
			"HorizontalPodAutoscaler/ns/web replicas=10 min=2 max=10 target=Deployment/web"},
		{"blocking pdb",
			`{"kind":"PodDisruptionBudget","metadata":{"name":"web"},
			  "status":{"disruptionsAllowed":0,"currentHealthy":1,"desiredHealthy":2}}`,
			"PodDisruptionBudget/ns/web allowed_disruptions=0 healthy=1/2"},
		{"a kind with no formatter is its name alone",
			`{"kind":"NetworkPolicy","metadata":{"name":"deny-all"},"spec":{"podSelector":{}}}`,
			"NetworkPolicy/ns/deny-all"},
		{"an unknown kind still produces a target",
			`{"kind":"Rollout","metadata":{"name":"canary"},"spec":{"replicas":3}}`,
			"Rollout/ns/canary"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := &fakeRunner{stdout: items(tc.body)}
			got := list(t, kit(t, run, Config{}), map[string]any{"namespace": "ns"})
			if got.Objects != tc.want {
				t.Errorf("\n got %q\nwant %q", got.Objects, tc.want)
			}
		})
	}
}

// TestMalformedOutputIsAResult. kubectl printing something unparseable is a
// fact about the cluster access, not a crash.
func TestMalformedOutputIsAResult(t *testing.T) {
	got := list(t, kit(t, &fakeRunner{stdout: "not json"}, Config{}), map[string]any{"namespace": "shop"})
	if got.Error == "" {
		t.Fatal("unparseable output produced no Error")
	}
}

// TestOfflineDeclaresTheSameToolAsLive.
//
// Tier 1 runs against the offline surface, so if the two declarations drift the
// eval measures a tool the agent does not have. Both are built by listTool for
// that reason; this asserts the constructor is actually shared rather than
// merely intended to be.
func TestOfflineDeclaresTheSameToolAsLive(t *testing.T) {
	live := kit(t, &fakeRunner{}, Config{})
	off, err := Offline("no cluster").Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(off) != 1 {
		t.Fatalf("offline toolset exposes %d tools, want 1", len(off))
	}
	offline, ok := off[0].(runnableTool)
	if !ok {
		t.Fatalf("offline tool is not runnable (%T)", off[0])
	}

	a, _ := json.Marshal(live.Declaration())
	b, _ := json.Marshal(offline.Declaration())
	if string(a) != string(b) {
		t.Errorf("declarations differ:\nlive    %s\noffline %s", a, b)
	}
	if live.Name() != ToolName {
		t.Errorf("tool name = %q, want %q — alias.go and the specs key on this", live.Name(), ToolName)
	}
}

// TestOfflineReportsAbsenceAsANoteNotAnError. An offline run is not a
// malfunction, and a model that reads "error" retries — which is the behaviour
// the message exists to prevent.
func TestOfflineReportsAbsenceAsANoteNotAnError(t *testing.T) {
	off, err := Offline("telemetry is unavailable for this run").Tools(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := off[0].(runnableTool).Run(testCtx(t), map[string]any{"namespace": "shop"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	blob, _ := json.Marshal(raw)
	var got Listing
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}
	if got.Error != "" {
		t.Errorf("Error = %q; absence is not a malfunction", got.Error)
	}
	if !strings.Contains(got.Note, "unavailable") {
		t.Errorf("Note = %q, want the caller's message", got.Note)
	}
}

// TestToolsetNameIsWhatSpecsAllowlist. The spec allowlist matches a toolset by
// Name(), so a drift here silently withholds the tool from every specialist
// that asked for it.
func TestToolsetNameIsWhatSpecsAllowlist(t *testing.T) {
	ts, err := Toolset(Config{Runner: &fakeRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	if ts.Name() != ToolsetName {
		t.Errorf("toolset name = %q, want %q", ts.Name(), ToolsetName)
	}
	if Offline("x").Name() != ToolsetName {
		t.Errorf("offline toolset name = %q, want %q", Offline("x").Name(), ToolsetName)
	}
}

// TestTheRealRunnerRefusesToGuess. This machine has 64 kube contexts. The
// kubeconfig check itself lives in internal/kubectl and is covered there; what
// this asserts is that the read path reaches it rather than building a client
// that resolves the ambient current-context.
func TestTheRealRunnerRefusesToGuess(t *testing.T) {
	if _, err := Toolset(Config{Kubeconfig: "/tmp/kubeconfig"}); err == nil {
		t.Error("an empty Context was accepted")
	}
	if _, err := Toolset(Config{Context: "kind-sre-eval-a1"}); err == nil {
		t.Error("an empty Kubeconfig was accepted")
	}
}

// TestTheDescriptionTellsTheModelWhenToReachForIt.
//
// The description is the whole mechanism: a tool the model does not call is
// worth nothing, and the failure it was built for is one where the model had
// already read a clean health scan and concluded there was nothing to look at.
// These three clauses are load-bearing, so they get a test rather than a
// comment.
func TestTheDescriptionTellsTheModelWhenToReachForIt(t *testing.T) {
	d := strings.ToLower(kit(t, &fakeRunner{}, Config{}).Description())
	for _, want := range []string{
		"<kind>/<namespace>/<name>", // the form the other tools take
		"clean",                     // a clean scan is not an empty namespace
		"guess",                     // what it did instead
	} {
		if !strings.Contains(d, want) {
			t.Errorf("description does not mention %q:\n%s", want, d)
		}
	}
}
