package scheduler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-sre-agent/internal/bounded"
	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

var epoch = time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)

// fakeEscalator records what the trigger asked for and never calls a model.
type fakeEscalator struct {
	seen []string
	err  error
}

func (f *fakeEscalator) Escalate(_ context.Context, ns string, _ []Transition) (*schema.HealthReport, error) {
	f.seen = append(f.seen, ns)
	if f.err != nil {
		return nil, f.err
	}
	return &schema.HealthReport{OverallSeverity: schema.OverallOK, Summary: "checked " + ns}, nil
}

type fakeNotifier struct {
	got []Digest
	err error
}

func (f *fakeNotifier) Notify(_ context.Context, d Digest) error {
	f.got = append(f.got, d)
	return f.err
}

// nilModel satisfies Config.Model without ever being called: the two calls into
// internal/bounded are replaced wholesale in these tests.
type nilModel struct{}

func (nilModel) Name() string { return "none" }
func (nilModel) GenerateContent(context.Context, *adkmodel.LLMRequest, bool) iter.Seq2[*adkmodel.LLMResponse, error] {
	panic("the model must not be reached in a hermetic scheduler test")
}

// harness builds a Scheduler whose collection and analysis are canned, so a
// whole cycle runs with no cluster, no lookout binary and no Vertex call.
func harness(t *testing.T, cfg Config, transitions string) (*Scheduler, *fakeEscalator, *fakeNotifier) {
	t.Helper()
	esc := &fakeEscalator{}
	note := &fakeNotifier{}
	if cfg.Cluster == "" {
		cfg.Cluster = "test"
	}
	if cfg.Store == "" {
		cfg.Store = filepath.Join(t.TempDir(), "store.db")
	}
	cfg.Model = nilModel{}
	cfg.Interval = time.Minute
	cfg.Escalator = esc
	cfg.Notifier = note
	// Respected when the caller set one, so a test can advance the clock across
	// two schedulers sharing a store.
	if cfg.Now == nil {
		cfg.Now = func() time.Time { return epoch }
	}

	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.collect = func(context.Context, lookout.Config, []string) bounded.Snapshot {
		return bounded.Snapshot{Health: "health.category status=degraded"}
	}
	s.check = func(context.Context, bounded.Config) (bounded.Result, error) {
		return bounded.Result{Report: &schema.HealthReport{
			OverallSeverity: schema.OverallOK, Summary: "bounded view",
		}}, nil
	}
	// The diff is the one step a hermetic test cannot run, so it is scripted at
	// the same seam the real one writes through.
	s.diffOverride = func(context.Context, string) ([]Transition, error) {
		return parseTransitions(transitions), nil
	}
	return s, esc, note
}

func line(class, ns, kind, name, reason, sev string) string {
	return fmt.Sprintf(`{"transition":%q,"namespace":%q,"kind_of_object":%q,"name":%q,"reason":%q,"severity":%q}`,
		class, ns, kind, name, reason, sev)
}

// Only new and escalated trigger a run. The other three classes are the whole
// reason a differ is worth having: `ongoing` is the steady state and escalating
// it would re-run the agent on the same fault every cycle forever, `resolved`
// closes, and `suppressed` is an operator ack that re-escalation would defeat.
func TestOnlyNewAndEscalatedTransitionsEscalate(t *testing.T) {
	s, esc, _ := harness(t, Config{}, strings.Join([]string{
		line("new", "shop", "Pod", "api-1", "CrashLoopBackOff", "critical"),
		line("escalated", "billing", "Pod", "led-1", "OOMKilled", "critical"),
		line("ongoing", "cache", "Pod", "c-1", "CrashLoopBackOff", "critical"),
		line("resolved", "web", "Pod", "w-1", "ImagePullBackOff", "warning"),
		line("suppressed", "batch", "Job", "j-1", "BackoffLimitExceeded", "warning"),
	}, "\n"))

	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{"shop", "billing"}
	if got := esc.seen; len(got) != 2 || !contains(got, "shop") || !contains(got, "billing") {
		t.Errorf("escalated %v, want exactly %v", got, want)
	}
	for _, quiet := range []string{"cache", "web", "batch"} {
		if contains(esc.seen, quiet) {
			t.Errorf("escalated %q, whose transition is not actionable", quiet)
		}
	}
}

// Two transitions in one namespace are one incident to investigate. The agent
// is asked about a namespace, so running it twice pays twice for the same
// assessment.
func TestOneNamespaceIsOneEscalation(t *testing.T) {
	s, esc, _ := harness(t, Config{}, strings.Join([]string{
		line("new", "shop", "Pod", "api-1", "CrashLoopBackOff", "critical"),
		line("new", "shop", "Pod", "api-2", "CrashLoopBackOff", "critical"),
		line("new", "shop", "Service", "api", "NoEndpoints", "warning"),
	}, "\n"))

	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 {
		t.Fatalf("escalated %v, want one run for the one namespace", esc.seen)
	}
	// And it is told all three, because which objects changed is the context
	// the agent would otherwise have to rediscover.
	if n := len(d.Escalations[0].Why); n != 3 {
		t.Errorf("the escalation carries %d transitions, want 3", n)
	}
}

// The cap exists because the first cycle against a cluster with five broken
// namespaces would otherwise start five full-agent runs at once. What it must
// not do is truncate quietly: a cycle that escalated two of five and said
// nothing about the rest reads exactly like a cycle where two things changed.
func TestTheEscalationCapDropsTheLeastSevereAndSaysSo(t *testing.T) {
	s, esc, _ := harness(t, Config{MaxEscalations: 2}, strings.Join([]string{
		line("new", "low", "Pod", "a-1", "Unschedulable", "info"),
		line("new", "mid", "Pod", "b-1", "OOMKilled", "warning"),
		line("new", "high", "Pod", "c-1", "CrashLoopBackOff", "critical"),
	}, "\n"))

	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 2 {
		t.Fatalf("escalated %v, want 2 under the cap", esc.seen)
	}
	// Worst first: the cap drops the least severe rather than whatever the scan
	// emitted last.
	if !contains(esc.seen, "high") || !contains(esc.seen, "mid") {
		t.Errorf("escalated %v, want the two most severe namespaces", esc.seen)
	}
	if len(d.Dropped) != 1 || d.Dropped[0].Namespace != "low" {
		t.Errorf("Dropped = %v, want the one refused namespace named", d.Dropped)
	}
}

// The floor is the only path for the absence class, and that is measured: the
// bounded pass reported `ok` with no findings on both fault-badselector and
// fault-invoicing, so those namespaces produce no transition, ever. It must run
// whatever the diff said — including on a completely quiet cycle.
func TestTheFloorSweepsEveryNamespaceWithNoTransitions(t *testing.T) {
	s, esc, note := harness(t, Config{
		Namespaces: []string{"shop", "billing"},
		Floor:      24 * time.Hour,
	}, "")

	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 2 || !contains(esc.seen, "shop") || !contains(esc.seen, "billing") {
		t.Errorf("the floor swept %v, want both namespaces", esc.seen)
	}
	if d.Trigger != "floor" {
		t.Errorf("Trigger = %q, want floor", d.Trigger)
	}
	if len(note.got) != 1 {
		t.Errorf("the floor produced %d digests, want 1", len(note.got))
	}
}

// It runs on the first cycle rather than waiting out the interval. A scheduler
// that has just started knows nothing about this cluster, and prod-checkout was
// broken the same way for twelve days without producing a transition — waiting
// a day to find that is the failure the floor exists to prevent.
func TestTheFloorRunsImmediatelyOnAColdStart(t *testing.T) {
	s, esc, _ := harness(t, Config{Namespaces: []string{"shop"}, Floor: 24 * time.Hour}, "")
	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 {
		t.Fatalf("the floor did not run on the first cycle: %v", esc.seen)
	}
	// And not again on the next cycle, or it stops being a floor and becomes
	// the every-cycle full agent this whole design exists to avoid.
	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 {
		t.Errorf("the floor swept again within its interval: %v", esc.seen)
	}
}

// A floor with nothing to sweep is a scheduler that silently cannot see a whole
// class of fault, so it is refused at construction rather than logged.
func TestConfigRefusesAFloorWithNoNamespaces(t *testing.T) {
	_, err := New(Config{
		Cluster: "c", Store: "s", Model: nilModel{}, Interval: time.Minute,
		Escalator: &fakeEscalator{}, Floor: 24 * time.Hour,
	})
	if err == nil {
		t.Fatal("a floor that sweeps nothing was accepted")
	}
	if !strings.Contains(err.Error(), "Namespaces") {
		t.Errorf("the error does not name the missing field: %v", err)
	}
}

// A quiet cycle says nothing, which is the point of a differ — but a channel
// that has been silent for an hour should prove the scheduler is alive rather
// than that the cluster is healthy. Those are the two readings an operator
// cannot tell apart, and the heartbeat is what separates them.
func TestAQuietCycleIsSilentUntilTheHeartbeatIsDue(t *testing.T) {
	s, _, note := harness(t, Config{Heartbeat: 3}, "")
	for i := 0; i < 2; i++ {
		if _, err := s.Cycle(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if len(note.got) != 0 {
		t.Fatalf("a quiet cycle notified: %v", note.got)
	}
	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(note.got) != 1 {
		t.Fatalf("the heartbeat did not fire after 3 quiet cycles: %d digests", len(note.got))
	}
	if note.got[0].Trigger != "heartbeat" {
		t.Errorf("Trigger = %q, want heartbeat", note.got[0].Trigger)
	}
}

// A cycle whose collection half-failed still reports, and the digest says which
// half is missing. The alternative is a scheduler that goes quiet exactly when
// the cluster becomes unreachable, which an operator reads as good news.
func TestAPartialCollectionStillProducesADigest(t *testing.T) {
	s, _, note := harness(t, Config{}, "")
	s.collect = func(context.Context, lookout.Config, []string) bounded.Snapshot {
		return bounded.Snapshot{
			Health: "health.category status=healthy",
			Errors: []string{"lookout triage delta: exit status 1"},
		}
	}
	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(note.got) != 1 {
		t.Fatalf("a degraded cycle notified %d times, want 1", len(note.got))
	}
	if len(d.CollectErrors) == 0 || !strings.Contains(d.CollectErrors[0], "exit status 1") {
		t.Errorf("the digest does not carry the collection failure: %v", d.CollectErrors)
	}
}

// A collection that produced nothing at all is an error, not an "ok" report.
// This is the tier-3 lesson at scheduler scale: with no telemetry the honest
// answer is "unchecked", and a cycle that emits a clean report from no evidence
// poisons the diff with a full set of resolutions.
func TestAnEmptyCollectionFailsTheCycleRatherThanReportingHealthy(t *testing.T) {
	s, _, _ := harness(t, Config{}, "")
	s.collect = func(context.Context, lookout.Config, []string) bounded.Snapshot {
		return bounded.Snapshot{Errors: []string{"lookout health: exit status 1"}}
	}
	if _, err := s.Cycle(context.Background()); err == nil {
		t.Fatal("a cycle with no cluster data succeeded")
	}
}

// An escalation that fails does not take the cycle down, and the failure is in
// the digest. One broken namespace must not cost the report on the other ten.
func TestAFailedEscalationIsRecordedNotFatal(t *testing.T) {
	s, esc, note := harness(t, Config{}, line("new", "shop", "Pod", "a-1", "CrashLoopBackOff", "critical"))
	esc.err = errors.New("vertex overloaded")

	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatalf("a failed escalation failed the cycle: %v", err)
	}
	if len(d.Escalations) != 1 || d.Escalations[0].Err == nil {
		t.Errorf("the escalation error was swallowed: %+v", d.Escalations)
	}
	if len(note.got) != 1 {
		t.Error("no digest was sent for a cycle whose escalation failed")
	}
}

// The store advances before the notifier runs, so a notification failure cannot
// make the next cycle re-report everything as new. Upstream's rule, and the
// trade it names is real: a notification lost after the state advanced is lost,
// which is better than a duplicate storm every cycle until the channel is back.
//
// Gated on the lookout binary and nothing else — the differ is a pure function
// of (report, store), needs no cluster, and creates its own SQLite file, which
// makes this the one integration test here that costs nothing to run.
func TestTheDiffAdvancesEvenWhenNotificationFails(t *testing.T) {
	bin := os.Getenv(lookout.EnvBinary)
	if bin == "" {
		t.Skipf("set %s to run the real diff pipeline", lookout.EnvBinary)
	}
	store := filepath.Join(t.TempDir(), "store.db")
	report := "kind=pod severity=critical namespace=shop kind_of_object=Pod " +
		"name=api-abc123 reason=CrashLoopBackOff message=x\nscanned=1 findings=1 elapsed=1ms\n"

	esc := &fakeEscalator{}
	note := &fakeNotifier{err: errors.New("slack is down")}
	s, err := New(Config{
		Cluster: "test", Store: store, Model: nilModel{}, Interval: time.Minute,
		Escalator: esc, Notifier: note, Now: func() time.Time { return epoch },
		Lookout: lookout.Config{Binary: bin, Kubeconfig: "unused", Context: "unused"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The diff runs for real; only the scans around it are canned, because they
	// are the half that needs a cluster.
	s.collect = func(context.Context, lookout.Config, []string) bounded.Snapshot {
		return bounded.Snapshot{Health: report}
	}
	s.check = func(context.Context, bounded.Config) (bounded.Result, error) {
		return bounded.Result{Report: &schema.HealthReport{
			OverallSeverity: schema.OverallOK, Summary: "s",
		}}, nil
	}
	// Scan verifies the kubeconfig pin before spawning, which this test has no
	// cluster for; the diff subcommand touches no cluster, so the pin is
	// bypassed here and only here.
	s.diffOverride = func(ctx context.Context, rep string) ([]Transition, error) {
		out, err := rawDiff(ctx, bin, store, "test", rep)
		if err != nil {
			return nil, err
		}
		return parseTransitions(out), nil
	}

	first, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Transitions) != 1 || first.Transitions[0].Class != "new" {
		t.Fatalf("first cycle transitions = %+v, want one new", first.Transitions)
	}
	if len(note.got) != 1 {
		t.Fatal("the notifier was not called")
	}

	// Second cycle, same report, notifier still failing: the subject must be
	// ongoing. If the state had not advanced it would be new again, and an
	// operator would be paged for the same fault every cycle.
	second, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Transitions) != 1 || second.Transitions[0].Class != "ongoing" {
		t.Fatalf("second cycle transitions = %+v, want one ongoing — the store did not advance "+
			"through a failed notification", second.Transitions)
	}
	if len(esc.seen) != 1 {
		t.Errorf("escalated %v; an ongoing subject must not re-escalate", esc.seen)
	}
}

func TestParseTransitionsSkipsTheSummaryLine(t *testing.T) {
	out := line("new", "shop", "Pod", "a-1", "CrashLoopBackOff", "critical") + "\n" +
		`{"scanned":1,"findings":1,"elapsed":"29ms"}` + "\n" +
		"not json at all\n"
	got := parseTransitions(out)
	if len(got) != 1 || got[0].Class != "new" {
		t.Errorf("parseTransitions = %+v, want the one transition", got)
	}
}

func TestValidateRejectsAConfigThatWouldRunButNotWork(t *testing.T) {
	base := Config{Cluster: "c", Store: "s", Model: nilModel{}, Interval: time.Minute, Escalator: &fakeEscalator{}}
	for _, tc := range []struct {
		name string
		bad  func(*Config)
		want string
	}{
		{"no cluster label", func(c *Config) { c.Cluster = "" }, "Cluster"},
		{"no store", func(c *Config) { c.Store = "" }, "Store"},
		{"no model", func(c *Config) { c.Model = nil }, "Model"},
		{"no interval", func(c *Config) { c.Interval = 0 }, "Interval"},
		{"no escalator", func(c *Config) { c.Escalator = nil }, "Escalator"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.bad(&cfg)
			err := cfg.Validate()
			if err == nil {
				t.Fatalf("accepted a config with no %s", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// rawDiff runs `lookout findings diff` without the kubeconfig pin.
//
// lookout.Pipe requires a verified Kubeconfig and Context, which is right for
// every other subcommand and unnecessary for this one: the diff reads a report
// on stdin and a SQLite file on disk and never contacts a cluster. Production
// goes through Pipe and keeps the pin, because a scheduler has both anyway;
// this bypass exists so the test can exercise the real differ with no cluster
// at all, which is what makes it free to run.
func rawDiff(ctx context.Context, bin, store, cluster, report string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, "findings", "diff",
		"--report=-", "--store="+store, "--cluster="+cluster, "--format=json")
	cmd.Stdin = strings.NewReader(report)
	cmd.Stderr = os.Stderr
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return out.String(), err
}

// Both of these were found by running the loop against a real cluster rather
// than by reading it, which is why they are here as a matched pair.

// A cluster-scoped transition names no namespace, and the agent is asked about
// one. The first live cycle escalated a Node finding and spent 29 seconds
// asking for an assessment of the namespace "". It still reaches the digest —
// it is a real finding, it just cannot select a target.
func TestAClusterScopedTransitionIsRecordedNotEscalated(t *testing.T) {
	s, esc, _ := harness(t, Config{}, strings.Join([]string{
		line("new", "", "Node", "kind-worker", "NodeNotReady", "critical"),
		line("new", "shop", "Pod", "api-1", "CrashLoopBackOff", "critical"),
	}, "\n"))

	d, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 || esc.seen[0] != "shop" {
		t.Errorf("escalated %v, want only the namespaced transition", esc.seen)
	}
	if len(d.Unscoped) != 1 || d.Unscoped[0].Kind != "Node" {
		t.Errorf("Unscoped = %v, want the Node finding recorded rather than dropped", d.Unscoped)
	}
	// And it still counts as something an operator should see, or a cycle whose
	// only news is a dead node would go out as silence.
	if !d.Changed() {
		t.Error("a cycle whose only transition is cluster-scoped reported no change")
	}
}

// The floor and the trigger overlap whenever a namespace both changed and is on
// the floor's list. The first live cycle assessed `shop` twice in one pass —
// two full agent runs, same namespace, same evidence — which is the same waste
// that makes two transitions in one namespace a single escalation.
func TestTheFloorSkipsWhatThisCycleAlreadyEscalated(t *testing.T) {
	s, esc, _ := harness(t, Config{
		Namespaces: []string{"shop", "billing"},
		Floor:      24 * time.Hour,
	}, line("new", "shop", "Pod", "api-1", "CrashLoopBackOff", "critical"))

	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 2 {
		t.Fatalf("escalated %v, want one triggered and one swept — shop must not run twice", esc.seen)
	}
	if !contains(esc.seen, "shop") || !contains(esc.seen, "billing") {
		t.Errorf("escalated %v, want shop (triggered) and billing (floor)", esc.seen)
	}
}

// The third thing running it against a real cluster found. `lookout health`
// emits one `kind=health.category` line per scorecard category alongside its
// object findings, and the differ keys subjects on an object — so feeding the
// scorecard in stores subjects that identify nothing and report `ongoing` in
// every digest forever. The first live cycle carried two.
func TestScorecardLinesAreNotDiffedAsObjects(t *testing.T) {
	report := strings.Join([]string{
		`kind=health.category severity=info category=nodes status=healthy`,
		`kind=health.category severity=critical category=crashloops status=degraded total=1`,
		`kind=pod.imagepull severity=critical namespace=shop kind_of_object=Pod name=api-1 reason=ImagePullBackOff`,
		`scanned=2 findings=2 elapsed=5ms`,
	}, "\n")

	got := objectFindings(report)
	if strings.Contains(got, "health.category") {
		t.Errorf("a scorecard line survived into the diff input:\n%s", got)
	}
	if !strings.Contains(got, "name=api-1") {
		t.Errorf("the object finding was dropped:\n%s", got)
	}
	// lookout's contract: a stream without a summary line is void, so dropping
	// it would make the differ refuse the report rather than read an empty one.
	if !strings.Contains(got, "scanned=2") {
		t.Errorf("the mandatory summary line was dropped:\n%s", got)
	}
}

// A cluster-scoped object has no namespace, and an empty middle segment reads
// like a bug in the tool to whoever is looking at the digest.
func TestTargetOmitsAnEmptyNamespace(t *testing.T) {
	cluster := Transition{Kind: "ValidatingWebhookConfiguration", Name: "warden-validating"}
	if got := cluster.Target(); got != "ValidatingWebhookConfiguration/warden-validating" {
		t.Errorf("Target() = %q, want no empty segment", got)
	}
	scoped := Transition{Kind: "Pod", Namespace: "shop", Name: "api-1"}
	if got := scoped.Target(); got != "Pod/shop/api-1" {
		t.Errorf("Target() = %q", got)
	}
}

// "Has just started" must mean the cluster is new to us, not the process.
//
// Found by running the demo: every `sre-monitor -once` is a fresh process, so
// every one of them read a zero lastFloor and swept every configured namespace
// at full-agent price whatever the differ had said. Two costs, and the second
// is the worse one — a demo whose floor assesses the namespace on every
// invocation can never show the trigger declining to escalate an unchanged
// fault, which is the entire argument for putting a differ in front of an
// agent.
func TestTheFloorSurvivesAProcessRestart(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store.db")
	cfg := func(now time.Time) Config {
		return Config{
			Store: store, Namespaces: []string{"shop"}, Floor: 24 * time.Hour,
			Now: func() time.Time { return now },
		}
	}

	first, esc, _ := harness(t, cfg(epoch), "")
	if _, err := first.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 {
		t.Fatalf("the cold start did not sweep: %v", esc.seen)
	}

	// A second process over the same store, a minute later.
	second, esc, _ := harness(t, cfg(epoch.Add(time.Minute)), "")
	d, err := second.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 0 {
		t.Errorf("a restarted scheduler swept again inside the floor interval: %v", esc.seen)
	}
	if d.Trigger == "floor" {
		t.Error("Trigger = floor on a cycle that did not sweep")
	}

	// And a third once the interval really has passed, or the mark has turned
	// the floor off rather than spaced it out.
	third, esc, _ := harness(t, cfg(epoch.Add(25*time.Hour)), "")
	if _, err := third.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(esc.seen) != 1 {
		t.Errorf("the floor did not sweep after its interval elapsed: %v", esc.seen)
	}
}

// Every way of failing to read the mark has to sweep. An unreadable mark costs
// one sweep; a mark wrongly trusted costs the absence class, which is the fault
// that produces no transition and is therefore invisible to everything else in
// this loop.
func TestAnUnreadableFloorMarkSweeps(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"garbage", "not a timestamp\n"},
		{"empty", ""},
		{"in the future", epoch.Add(72 * time.Hour).Format(time.RFC3339)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := filepath.Join(t.TempDir(), "store.db")
			if err := os.WriteFile(floorMark(store), []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			s, esc, _ := harness(t, Config{
				Store: store, Namespaces: []string{"shop"}, Floor: 24 * time.Hour,
			}, "")
			if _, err := s.Cycle(context.Background()); err != nil {
				t.Fatal(err)
			}
			if len(esc.seen) != 1 {
				t.Errorf("a %s mark suppressed the floor: %v", tc.name, esc.seen)
			}
		})
	}
}

// The mark goes beside the store, not inside it: the store is lookout's SQLite
// database with lookout's schema, and a second writer with its own table in
// someone else's file is a migration hazard for one timestamp. What that buys
// is that the two share a lifetime — deleting the store resets the floor, which
// is right, because a fresh store makes every subject new.
func TestTheFloorMarkLivesBesideTheStore(t *testing.T) {
	store := filepath.Join(t.TempDir(), "store.db")
	s, _, _ := harness(t, Config{
		Store: store, Namespaces: []string{"shop"}, Floor: 24 * time.Hour,
	}, "")
	if _, err := s.Cycle(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(floorMark(store), store) {
		t.Fatalf("floor mark %q is not beside the store %q", floorMark(store), store)
	}
	b, err := os.ReadFile(floorMark(store))
	if err != nil {
		t.Fatalf("no floor mark written after a sweep: %v", err)
	}
	if got := strings.TrimSpace(string(b)); got != epoch.UTC().Format(time.RFC3339) {
		t.Errorf("mark = %q, want the sweep time", got)
	}
}
