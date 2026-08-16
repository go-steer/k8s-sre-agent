// Package scheduler is the monitoring loop: a bounded pass every cycle, the
// full agent on what changed, and a slow full-agent floor regardless.
//
// # The three clocks, and why one is not enough
//
// Running the agent on the cycle does not work. The 2026-08-15 tier-2 run cost
// $0.2374 per namespace and tier 3 measured up to 2m44s for one; 288 cycles a
// day over ten namespaces is roughly $683/day and does not fit a five-minute
// interval. So the cycle is internal/bounded — one subagent call over two
// subprocess scans, $0.0046 and single-digit seconds.
//
// But the bounded pass cannot find a fault that is an absence, and that is
// measured rather than assumed: against the eleven tier-2 fixtures it reported
// `ok` with no findings on both fault-badselector and fault-invoicing. Those
// namespaces produce no transition, ever, so an escalation trigger alone would
// never look at them. Neither would it look at tier 3's prod-checkout, which
// had been broken the same way for twelve days.
//
// Hence three clocks:
//
//   - Every cycle: the bounded pass, and the diff of its scan against the last.
//   - On a new or escalated transition: the full agent, scoped to that
//     namespace.
//   - Every Floor interval: the full agent on every configured namespace,
//     whatever the diff said. This is the only path for the absence class and
//     it is not optional; see Config.Floor.
//
// # What the diff keys on, which is not our report
//
// `lookout findings diff` is the differ and we do not write one. It is keyed on
// <cluster>/<namespace>/<kind>/<normalized-name>/<canonical-reason>, carries
// first_seen across runs, and classifies each subject new|ongoing|escalated|
// resolved|suppressed.
//
// It is fed the *collector's* findings, not the model's HealthReport, and that
// is a deliberate reversal of what the design note in AGENTS.md originally
// assumed. The bounded pass wrote RolloutIncomplete, ExcessiveRestarts and
// CrashLoopBackOff for closely related conditions inside a single run —
// model-chosen tokens are not a stable diff key. lookout derives its reason
// mechanically from object status, so the same fault produces the same subject
// key every cycle.
//
// One consequence worth stating plainly: this takes the escalation trigger off
// task #16's critical path. #16 is the *agent's* object-choice instability, and
// the trigger never sees the agent's findings. The model's report is what a
// human reads, not what the diff compares.
//
// # No writes
//
// Nothing here grants Config.Writes and the Escalator is expected not to
// either. approval.Prompt reads a terminal and approves on "y"; on a 3am cycle
// there is nobody at it, and pointing a scheduler at ApproveAllUnattended would
// convert the structural HITL guarantee into a rubber stamp. The scheduled path
// is detect-and-report until asynchronous approval exists.
package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	adkmodel "google.golang.org/adk/v2/model"

	"github.com/go-steer/core-sre-agent/internal/bounded"
	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/schema"
)

// Transition is one subject's change since the previous cycle, as
// `lookout findings diff` classified it.
type Transition struct {
	Class        string `json:"transition"`
	Namespace    string `json:"namespace"`
	Kind         string `json:"kind_of_object"`
	Name         string `json:"name"`
	Reason       string `json:"reason"`
	Severity     string `json:"severity"`
	PrevSeverity string `json:"prev_severity"`
	FirstSeen    string `json:"first_seen"`
	// SubjectKey is lookout's own instance-grain key. It is currently
	// unreadable — the binary emits "[REDACTED]" for it in both wire formats,
	// which looks like its secret-safety pass firing on a field that is an
	// identifier rather than a value. Recorded rather than relied on: `lookout
	// findings ack` wants this key, so an operator ack surface is blocked on it
	// and Escalations key on (namespace, kind, name, reason) instead.
	SubjectKey string `json:"subject_key"`
}

// Actionable reports whether a transition is one the trigger escalates on.
//
// new and escalated only. `ongoing` is the steady state and escalating it would
// re-run the agent on the same fault every cycle forever; `resolved` closes;
// `suppressed` is an operator ack and re-escalating it would defeat the ack.
func (t Transition) Actionable() bool {
	return t.Class == "new" || t.Class == "escalated"
}

// Target is the object the transition is about, for logs and digests.
//
// The namespace segment is dropped when there is none, so a cluster-scoped
// object reads `ValidatingWebhookConfiguration/warden-validating…` rather than
// carrying an empty segment as `Kind//name`. Cosmetic, but these go in front of
// an operator at 3am and a stray `//` reads like a bug in the tool.
func (t Transition) Target() string {
	if t.Namespace == "" {
		return t.Kind + "/" + t.Name
	}
	return fmt.Sprintf("%s/%s/%s", t.Kind, t.Namespace, t.Name)
}

// Escalation is one full-agent run the cycle triggered.
type Escalation struct {
	Namespace string
	Why       []Transition
	Report    *schema.HealthReport
	Err       error
	Elapsed   time.Duration
}

// Digest is one cycle's outcome, and the only thing a Notifier ever sees.
type Digest struct {
	Cluster string
	At      time.Time
	// Why the digest was emitted: "changes", "floor" or "heartbeat".
	Trigger string
	// Report is the bounded pass's own view of the cluster this cycle.
	Report *schema.HealthReport
	// Protests are contract violations in that report. The bounded pass has no
	// handback, so they are recorded rather than fixed.
	Protests    []string
	Transitions []Transition
	Escalations []Escalation
	// Dropped is how many actionable transitions the per-cycle cap refused.
	//
	// Reported rather than silently truncated: a cycle that escalated three of
	// eleven namespaces and said nothing about the other eight reads exactly
	// like a cycle where only three namespaces changed.
	Dropped []Transition
	// Unscoped are transitions the trigger cannot act on because they name no
	// namespace — a Node, a PV, a webhook, a cert. They are real findings and
	// they reach the digest; what they cannot do is select a namespace for an
	// agent that is asked about one.
	//
	// Found by running it: the first live cycle escalated one of these and
	// spent 29 seconds asking the agent to assess the namespace "".
	Unscoped []Transition
	// CollectErrors are scans that did not complete. The cycle continues on a
	// partial collection — see bounded.Snapshot.Errors — but a digest built
	// from half a scan must say so.
	CollectErrors []string
}

// Changed reports whether anything happened that an operator needs to see.
func (d Digest) Changed() bool {
	for _, t := range d.Transitions {
		if t.Actionable() || t.Class == "resolved" {
			return true
		}
	}
	return len(d.Escalations) > 0 || len(d.CollectErrors) > 0 || len(d.Unscoped) > 0
}

// Notifier is where a digest goes. An interface because the only implementation
// that matters — Slack — is a separate piece of work, and because a scheduler
// whose notification path cannot be substituted cannot be tested.
type Notifier interface {
	Notify(ctx context.Context, d Digest) error
}

// Escalator runs the full agent against one namespace.
//
// Supplied by the caller rather than built here, for two reasons. The agent is
// constructed per run and thrown away — a resident one would leak one
// namespace's diagnosis into the next, and the orchestrator's context is 93% of
// the bill, so holding it is monotonically more expensive for no benefit. And
// keeping construction outside this package is what lets a test drive the loop
// with a scripted agent and no Vertex call.
type Escalator interface {
	Escalate(ctx context.Context, namespace string, why []Transition) (*schema.HealthReport, error)
}

// Config is one monitored cluster.
type Config struct {
	// Cluster labels this cluster in the diff store. It becomes the first
	// segment of every subject key, so it must be the same string on every
	// cycle — changing it makes every subject look new.
	Cluster string

	// Store is the SQLite file `lookout findings diff` keeps state in. It is
	// created on first use.
	Store string

	// Lookout pins the cluster. One process per cluster: Config pins one
	// kubeconfig and one context, and the ambient current-context is never
	// resolved, so a scheduler cannot reach a cluster it was not given.
	Lookout lookout.Config

	// Namespaces is what the floor sweeps. The cycle itself always scans the
	// whole cluster — one -A scan is cheaper than one per namespace — so this
	// is only the floor's list.
	//
	// Empty disables the floor, which Validate refuses to let happen quietly:
	// the floor is the only path for the absence class, and a scheduler running
	// without one silently cannot see a whole category of fault.
	Namespaces []string

	// Model runs the bounded pass's single analysis call. The subagent tier.
	Model adkmodel.LLM

	// Interval is the cycle period.
	Interval time.Duration

	// Floor is how often every configured namespace gets a full agent run
	// regardless of what the diff said. Daily is the intended order of
	// magnitude: absence findings do not change on a five-minute clock, they
	// change when somebody deploys.
	Floor time.Duration

	// MaxEscalations bounds full-agent runs per cycle. Without it the first
	// cycle against a cluster with five broken namespaces starts five agent
	// runs at once. Zero means unlimited, which is a defensible choice for a
	// small cluster and a bad one everywhere else.
	MaxEscalations int

	// Heartbeat emits a digest every N quiet cycles even when nothing changed,
	// so a silent channel proves the scheduler is alive rather than that the
	// cluster is healthy. Zero disables it.
	Heartbeat int

	Notifier  Notifier
	Escalator Escalator

	// Now is the clock. Nil uses time.Now.
	Now func() time.Time
}

// Validate rejects a configuration that would run but not do its job.
func (c Config) Validate() error {
	switch {
	case c.Cluster == "":
		return fmt.Errorf("scheduler: Cluster is required — it keys the diff store")
	case c.Store == "":
		return fmt.Errorf("scheduler: Store is required — a diff with nowhere to persist reports everything new every cycle")
	case c.Model == nil:
		return fmt.Errorf("scheduler: Model is required")
	case c.Interval <= 0:
		return fmt.Errorf("scheduler: Interval must be positive")
	case c.Escalator == nil:
		return fmt.Errorf("scheduler: Escalator is required — a trigger with nothing to trigger is a digest generator")
	case c.Floor > 0 && len(c.Namespaces) == 0:
		return fmt.Errorf("scheduler: Floor is set but Namespaces is empty, so the floor would sweep nothing")
	}
	return nil
}

func (c Config) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Scheduler is one cluster's monitoring loop.
type Scheduler struct {
	cfg Config

	// cycles counts completed cycles, for the heartbeat.
	cycles int
	// lastFloor is when the floor last swept. Zero means never, and the first
	// cycle runs it — a scheduler that has just started knows nothing, and
	// waiting a day to find a twelve-day-old outage is the failure the floor
	// exists to prevent.
	//
	// Loaded from and written back to a file beside the store, because "has just
	// started" has to mean the *cluster* is new to us rather than the process.
	// It did not, and the demo is where that showed: every `sre-monitor -once`
	// is a fresh process, so every one of them read a zero here and ran a full
	// $0.15 sweep of every configured namespace whatever the differ had said —
	// which is both the wrong bill and, worse, a demo in which the trigger's
	// suppression of an unchanged fault can never be observed, because the floor
	// assessed the namespace anyway.
	lastFloor time.Time

	// The two calls into internal/bounded, as fields so a test can drive a
	// whole cycle without a cluster. Deliberately not knobs on Config: the only
	// way to get a Scheduler that does not really scan is to build one inside
	// this package, which is the same rule internal/sre applies to its
	// unguarded specialist.
	collect func(context.Context, lookout.Config, []string) bounded.Snapshot
	check   func(context.Context, bounded.Config) (bounded.Result, error)
	// diffOverride replaces the `lookout findings diff` invocation. Same rule
	// as the two above: unexported, so the only way to get a Scheduler whose
	// trigger is scripted is to build one inside this package.
	diffOverride func(context.Context, string) ([]Transition, error)
}

func New(cfg Config) (*Scheduler, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	s := &Scheduler{cfg: cfg, collect: bounded.Collect, check: bounded.Check}
	s.lastFloor = readFloorMark(cfg.Store)
	return s, nil
}

// floorMark is the file the floor's timestamp lives in: one RFC3339 line beside
// the diff store.
//
// Beside the store rather than inside it, and that is deliberate rather than
// lazy. The store is lookout's SQLite database with lookout's schema, migrated
// by lookout; a second writer with its own table in someone else's file is a
// migration hazard for a single timestamp. Tying it to the store's *path* keeps
// the one property that matters — `rm findings.db` resets both, which is right,
// because a fresh store makes every subject new and a fresh floor is exactly
// what a scheduler that knows nothing should have.
func floorMark(store string) string { return store + ".floor" }

// readFloorMark returns when the floor last swept, or the zero time.
//
// Every failure reads as "never swept", which sweeps. That is the safe
// direction and the reason there is no error return: an unreadable mark costs a
// sweep, while trusting a corrupt one costs the absence class — the fault that
// produces no transition and is therefore invisible to everything else in this
// loop. floorDue applies the same rule to a mark in the future, whose only
// plausible source is a clock change.
func readFloorMark(store string) time.Time {
	b, err := os.ReadFile(floorMark(store))
	if err != nil {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		return time.Time{}
	}
	return t
}

// writeFloorMark records a sweep.
//
// A write failure is swallowed, and only because of which way it fails: the
// in-process lastFloor is set regardless, so a running scheduler still honours
// its interval, and the cost of a permanently unwritable mark is that a
// restarted one sweeps again — which is precisely today's behaviour. Nothing
// here can make the floor sweep *less* than it should, which is the failure
// that would have to be surfaced.
func writeFloorMark(store string, at time.Time) {
	_ = os.WriteFile(floorMark(store), []byte(at.UTC().Format(time.RFC3339)+"\n"), 0o600)
}

// LastFloor is when the floor last swept, as recorded beside the store, or the
// zero time if it never has.
//
// Exported so a command can say at startup why a cycle it expected to sweep is
// not going to. The mark is a file on disk now rather than a field in a process
// that has just started, which is the fix — but it also means an operator
// running `-once` twice sees the second one skip the floor, and a floor that
// skips silently is indistinguishable from a floor that is broken.
func (s *Scheduler) LastFloor() time.Time { return s.lastFloor }

// Run cycles until the context is cancelled.
//
// Deliberately thin: everything worth testing is in Cycle, which is a function
// of the cluster and the store rather than of the clock. A loop that had to be
// driven by a fake clock to test the escalation cap would be tested less.
func (s *Scheduler) Run(ctx context.Context) error {
	t := time.NewTicker(s.cfg.Interval)
	defer t.Stop()
	for {
		if _, err := s.Cycle(ctx); err != nil {
			// A failed cycle is not a failed scheduler. The next one may
			// succeed, and a monitoring process that exits on the first
			// transient error is worse than one that logs and continues.
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.notifyFailure(ctx, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

// Cycle is one pass: collect, diff, analyse, escalate, notify.
//
// The order is load-bearing in one place. The diff advances the store as a side
// effect, and it runs *before* notification — so a notifier failure cannot make
// the next cycle re-report everything as new. That is upstream's rule and the
// trade it names is real: a notification lost after the state advanced is lost,
// which is better than a duplicate storm every cycle until the channel comes
// back.
func (s *Scheduler) Cycle(ctx context.Context) (Digest, error) {
	at := s.cfg.now()
	d := Digest{Cluster: s.cfg.Cluster, At: at, Trigger: "changes"}

	snap := s.collect(ctx, s.cfg.Lookout, nil)
	d.CollectErrors = snap.Errors
	if snap.Empty() {
		return d, fmt.Errorf("scheduler: no cluster data collected: %s", strings.Join(snap.Errors, "; "))
	}

	// The diff first, so the store advances before anything can fail.
	transitions, err := s.diff(ctx, snap.Health)
	if err != nil {
		// A diff failure costs this cycle's trigger but not its report: the
		// bounded pass still says what the cluster looks like, and saying it
		// with the trigger broken is better than saying nothing.
		d.CollectErrors = append(d.CollectErrors, err.Error())
	}
	d.Transitions = transitions

	res, err := s.check(ctx, bounded.Config{
		Lookout: s.cfg.Lookout, Model: s.cfg.Model, Now: at,
	})
	// Check re-collects, which is one redundant scan pair per cycle. Accepted:
	// it keeps bounded.Check the single entry point that internal/bounded's
	// tests cover, and two subprocess scans are a fraction of a second against
	// an interval measured in minutes. Reusing the snapshot would mean a second
	// public entry point into that package whose only caller is here.
	d.Report, d.Protests = res.Report, res.Protests
	if err != nil {
		d.CollectErrors = append(d.CollectErrors, err.Error())
	}

	d.Escalations, d.Dropped, d.Unscoped = s.escalate(ctx, transitions)

	if s.floorDue(at) {
		d.Trigger = "floor"
		// Skipping what this cycle already escalated. The floor and the trigger
		// overlap whenever a namespace both changed and is on the floor's list,
		// and the first live cycle assessed `shop` twice for it — the same
		// argument that makes two transitions in one namespace one escalation,
		// one layer up.
		d.Escalations = append(d.Escalations, s.sweep(ctx, d.Escalations)...)
		s.lastFloor = at
		writeFloorMark(s.cfg.Store, at)
	}

	s.cycles++
	if !d.Changed() && d.Trigger != "floor" {
		if !s.heartbeatDue() {
			return d, nil
		}
		d.Trigger = "heartbeat"
	}
	s.notify(ctx, d)
	return d, nil
}

// diff pipes the collector's findings through `lookout findings diff`.
func (s *Scheduler) diff(ctx context.Context, report string) ([]Transition, error) {
	if s.diffOverride != nil {
		return s.diffOverride(ctx, report)
	}
	out, err := lookout.Pipe(ctx, s.cfg.Lookout, objectFindings(report),
		"findings", "diff",
		"--report=-",
		"--store="+s.cfg.Store,
		"--cluster="+s.cfg.Cluster,
		"--format=json",
	)
	if err != nil {
		return parseTransitions(out), fmt.Errorf("scheduler: diff: %w", err)
	}
	return parseTransitions(out), nil
}

// objectFindings strips `lookout health`'s scorecard lines from a report before
// it is diffed.
//
// The scan emits two kinds of line: findings about objects, and one
// `kind=health.category` line per scorecard category answering
// healthy|degraded|unavailable. Only the first kind names an object, and the
// differ keys subjects on <cluster>/<namespace>/<kind>/<name>/<reason> — so
// feeding it the scorecard stores a permanent subject that identifies nothing
// and reports `ongoing` in every digest forever. The first live cycle carried
// two of them.
//
// They are also redundant: a degraded category is degraded *because* of an
// object finding in the same report, and that finding is diffed on its own.
//
// The summary line is kept deliberately. lookout's contract is that a stream
// without one is void, so dropping it would make the differ reject the report
// rather than read an empty one.
func objectFindings(report string) string {
	var keep []string
	for _, line := range strings.Split(report, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "kind=health.category") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.Join(keep, "\n")
}

// parseTransitions reads the diff's JSON lines.
//
// lookout emits one record per line and terminates with a summary line, which
// has no "transition" field and is skipped. A line that does not parse is
// skipped rather than failing the cycle: the diff is advisory here — the report
// is produced either way — and one malformed record should not cost the
// transitions that parsed around it.
func parseTransitions(out string) []Transition {
	var ts []Transition
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var t Transition
		if err := json.Unmarshal([]byte(line), &t); err != nil {
			continue
		}
		if t.Class == "" {
			continue
		}
		ts = append(ts, t)
	}
	return ts
}

// escalate runs the agent on the namespaces that changed, capped.
//
// Grouped by namespace, because two transitions in one namespace are one
// incident to investigate rather than two — the agent is asked about a
// namespace, and running it twice would pay twice for the same assessment.
func (s *Scheduler) escalate(ctx context.Context, ts []Transition) (done []Escalation, dropped, unscoped []Transition) {
	byNS := map[string][]Transition{}
	var order []string
	for _, t := range ts {
		if !t.Actionable() {
			continue
		}
		if t.Namespace == "" {
			unscoped = append(unscoped, t)
			continue
		}
		if _, seen := byNS[t.Namespace]; !seen {
			order = append(order, t.Namespace)
		}
		byNS[t.Namespace] = append(byNS[t.Namespace], t)
	}
	// Worst first, so a cap that bites drops the least severe rather than
	// whatever the scan happened to emit last.
	sort.SliceStable(order, func(i, j int) bool {
		return worst(byNS[order[i]]) > worst(byNS[order[j]])
	})

	for i, ns := range order {
		if s.cfg.MaxEscalations > 0 && i >= s.cfg.MaxEscalations {
			dropped = append(dropped, byNS[ns]...)
			continue
		}
		done = append(done, s.runAgent(ctx, ns, byNS[ns]))
	}
	return done, dropped, unscoped
}

// sweep is the floor: every configured namespace, whatever the diff said.
func (s *Scheduler) sweep(ctx context.Context, already []Escalation) []Escalation {
	done := make(map[string]bool, len(already))
	for _, e := range already {
		done[e.Namespace] = true
	}
	var out []Escalation
	for _, ns := range s.cfg.Namespaces {
		if done[ns] {
			continue
		}
		out = append(out, s.runAgent(ctx, ns, nil))
	}
	return out
}

func (s *Scheduler) runAgent(ctx context.Context, ns string, why []Transition) Escalation {
	started := s.cfg.now()
	report, err := s.cfg.Escalator.Escalate(ctx, ns, why)
	return Escalation{
		Namespace: ns, Why: why, Report: report, Err: err,
		Elapsed: s.cfg.now().Sub(started),
	}
}

func (s *Scheduler) floorDue(at time.Time) bool {
	if s.cfg.Floor <= 0 || len(s.cfg.Namespaces) == 0 {
		return false
	}
	// Never swept: sweep now. A scheduler that has just started knows nothing
	// about this cluster, and prod-checkout was broken for twelve days without
	// producing a single transition. A mark in the future is the same case — the
	// clock moved under us and the honest reading of the mark is that we do not
	// know when the last sweep was.
	return s.lastFloor.IsZero() || s.lastFloor.After(at) || at.Sub(s.lastFloor) >= s.cfg.Floor
}

func (s *Scheduler) heartbeatDue() bool {
	return s.cfg.Heartbeat > 0 && s.cycles%s.cfg.Heartbeat == 0
}

func (s *Scheduler) notify(ctx context.Context, d Digest) {
	if s.cfg.Notifier == nil {
		return
	}
	_ = s.cfg.Notifier.Notify(ctx, d)
}

// notifyFailure tells the operator the cycle itself broke.
//
// A scheduler that fails silently is indistinguishable from a healthy cluster,
// which is the most dangerous of all the failure modes here: the absence of
// alerts is exactly what an operator reads as good news.
func (s *Scheduler) notifyFailure(ctx context.Context, err error) {
	s.notify(ctx, Digest{
		Cluster:       s.cfg.Cluster,
		At:            s.cfg.now(),
		Trigger:       "cycle-failed",
		CollectErrors: []string{err.Error()},
	})
}

// worst ranks a namespace's transitions for the cap's ordering.
func worst(ts []Transition) int {
	rank := map[string]int{"critical": 3, "warning": 2, "info": 1}
	high := 0
	for _, t := range ts {
		if r := rank[strings.ToLower(t.Severity)]; r > high {
			high = r
		}
	}
	return high
}
