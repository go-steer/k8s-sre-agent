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

// Command sre-monitor runs the monitoring loop against one cluster.
//
// This is the first entry point that is not one-shot. Everything before it —
// two eval harnesses and cmd/sre-agent — answers a question once and exits; this
// one keeps answering it, which is what the agent was ported to do.
//
//	source ~/scripts/claude-env.sh
//	kubectl config view --minify --flatten --context=CTX > /tmp/kubeconfig-CTX
//	SRE_LOOKOUT_BIN=... go run ./cmd/sre-monitor \
//	  -kubeconfig /tmp/kubeconfig-CTX -context CTX \
//	  -cluster prod-east -store /var/lib/sre/prod-east.db \
//	  -namespace online-boutique,prod-checkout -v
//
// # Three clocks
//
// Every -interval, the bounded pass and a diff of its scan against the previous
// one. On a new or escalated subject, the full agent scoped to that namespace.
// Every -floor, the full agent over every -namespace regardless. See
// internal/scheduler for why one clock is not enough — briefly, the bounded
// pass cannot see an absence, and a namespace broken the same way for twelve
// days never produces a transition.
//
// # One process, one cluster
//
// The kubeconfig pin is per-process: lookout resolves its cluster from
// KUBECONFIG's current-context with no per-call flag, and the file must describe
// exactly one context. Monitoring two clusters is two processes, each pinned,
// each with its own store — which means a bug here cannot reach a cluster this
// process was never given credentials for.
//
// # Read-only, and not by accident
//
// No Config.Writes anywhere, so sre.Build never constructs change-executor. The
// same four guards cmd/sre-agent uses are applied here from internal/readonly,
// and there is a fifth reason they matter more in this command: approval.Prompt
// reads a terminal, and on a 3am cycle there is nobody at it. A scheduler
// pointed at ApproveAllUnattended would turn the structural HITL guarantee into
// a rubber stamp, so the scheduled path stays detect-and-report until
// asynchronous approval exists.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/go-steer/mast/pkg/pricing"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/k8s-sre-agent/internal/evals"
	"github.com/go-steer/k8s-sre-agent/internal/kuberead"
	"github.com/go-steer/k8s-sre-agent/internal/llm"
	"github.com/go-steer/k8s-sre-agent/internal/lookout"
	"github.com/go-steer/k8s-sre-agent/internal/notify"
	"github.com/go-steer/k8s-sre-agent/internal/readonly"
	"github.com/go-steer/k8s-sre-agent/internal/scheduler"
	"github.com/go-steer/k8s-sre-agent/internal/schema"
	"github.com/go-steer/k8s-sre-agent/internal/sre"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig file to use (required; must describe exactly one context)")
		kubecontext = flag.String("context", "", "kube context to pin to (required; must be the kubeconfig's current-context)")
		cluster     = flag.String("cluster", "", "cluster label for the diff store (required; must not change between runs)")
		store       = flag.String("store", "", "SQLite file the finding diff keeps state in (required)")
		namespaces  = flag.String("namespace", "", "comma-separated namespaces the floor sweeps (required unless -floor=0)")
		interval    = flag.Duration("interval", 5*time.Minute, "bounded-pass cycle period")
		floor       = flag.Duration("floor", 24*time.Hour, "how often every namespace gets a full agent run regardless (0 disables)")
		maxEsc      = flag.Int("max-escalations", 3, "full-agent runs allowed per cycle (0 = unlimited)")
		heartbeat   = flag.Int("heartbeat", 12, "emit a digest every N quiet cycles so a silent channel proves liveness (0 disables)")
		specDir     = flag.String("specs", "", "load specialist specs from this directory instead of the embedded set")
		timeout     = flag.Duration("timeout", 8*time.Minute, "per-escalation agent timeout")
		retryFor    = flag.Duration("retry-for", 20*time.Minute, "how long to keep retrying an escalation through provider rate limiting")
		maxCost     = flag.Float64("max-cost", 2.0, "abort one escalation once it has spent this many USD (0 = unlimited)")
		maxTurns    = flag.Int("max-turns", 0, "abort one escalation after this many model calls (0 = unlimited)")
		sbURL       = flag.String("switchboard-url", "", "switchboard outbound ingress base URL; empty logs digests instead of posting them")
		sbConv      = flag.String("switchboard-conversation", "", "conversation digests are posted to (a Slack channel ID, or channel:thread)")
		once        = flag.Bool("once", false, "run a single cycle and exit, for a smoke test or a cron")
		verbose     = flag.Bool("v", false, "log each cycle's transitions and escalations")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := runConfig{
		kubeconfig: *kubeconfig, kubecontext: *kubecontext,
		cluster: *cluster, store: *store, namespaces: splitList(*namespaces),
		interval: *interval, floor: *floor, maxEsc: *maxEsc, heartbeat: *heartbeat,
		specDir: *specDir, timeout: *timeout, retryFor: *retryFor,
		maxCost: *maxCost, maxTurns: *maxTurns, once: *once, verbose: *verbose,
		sbURL: *sbURL, sbConv: *sbConv,
	}
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

type runConfig struct {
	kubeconfig, kubecontext string
	cluster, store          string
	namespaces              []string
	interval, floor         time.Duration
	maxEsc, heartbeat       int
	specDir                 string
	timeout, retryFor       time.Duration
	// maxCost defaults to $2, unlike every other command in this repo, and the
	// difference is deliberate. A ceiling that fires mid-run produces a partial
	// answer that reads like a bad diagnosis, which is why the eval harnesses
	// ship unlimited — a quietly bounded baseline is worse than an expensive
	// one. This is the operational path the budget note points at instead: an
	// agent that loops here is a bill that arrives every cycle, and no baseline
	// is being published.
	maxCost  float64
	maxTurns int
	once     bool
	verbose  bool

	// switchboard is where digests go. Empty logs them instead, which is the
	// right default for a smoke run and the wrong one for anything left
	// running: a monitoring loop nobody is reading is a monitoring loop that is
	// not doing its job.
	sbURL, sbConv string
}

func run(ctx context.Context, cfg runConfig) error {
	switch {
	case cfg.kubeconfig == "" || cfg.kubecontext == "":
		return fmt.Errorf("-kubeconfig and -context are both required; this command does not resolve the ambient current-context")
	case cfg.cluster == "":
		return fmt.Errorf("-cluster is required: it labels this cluster in the diff store, and changing it makes every subject look new")
	case cfg.store == "":
		return fmt.Errorf("-store is required: a diff with nowhere to persist reports everything as new on every cycle")
	}
	if err := readonly.VerifySoleContext(cfg.kubeconfig, cfg.kubecontext); err != nil {
		return err
	}
	// Asked once at startup rather than per cycle: the answer is a property of
	// the lookout binary, which does not change while the process runs, and a
	// per-cycle handshake would be one more subprocess every five minutes.
	withheld, err := readonly.LookoutWriters(ctx, cfg.kubeconfig, cfg.kubecontext)
	if err != nil {
		return err
	}

	mainModel, subModel, err := llm.Models(ctx)
	if err != nil {
		return fmt.Errorf("resolve models: %w", err)
	}
	cat, catErr := evals.Catalog()
	if catErr != nil && cfg.maxCost > 0 {
		return fmt.Errorf("pricing catalog (needed by -max-cost): %w", catErr)
	}

	notifier, err := buildNotifier(cfg)
	if err != nil {
		return err
	}

	esc := &agentEscalator{cfg: cfg, main: mainModel, sub: subModel, withheld: withheld, catalog: cat}
	s, err := scheduler.New(scheduler.Config{
		Cluster:        cfg.cluster,
		Store:          cfg.store,
		Lookout:        readonly.LookoutConfig(cfg.kubeconfig, cfg.kubecontext),
		Namespaces:     cfg.namespaces,
		Model:          subModel,
		Interval:       cfg.interval,
		Floor:          cfg.floor,
		MaxEscalations: cfg.maxEsc,
		Heartbeat:      cfg.heartbeat,
		Notifier:       notifier,
		Escalator:      esc,
	})
	if err != nil {
		return err
	}

	log.Printf("monitoring %s every %s (floor %s over %d namespace(s), max %d escalation(s)/cycle)",
		cfg.cluster, cfg.interval, cfg.floor, len(cfg.namespaces), cfg.maxEsc)
	// When the floor last swept, because it is persisted beside the store and a
	// second `-once` will therefore skip it. That is the intended behaviour and
	// it looks exactly like a broken floor from the outside.
	if cfg.floor > 0 {
		if last := s.LastFloor(); last.IsZero() {
			log.Printf("floor: never swept this store, so this cycle sweeps")
		} else {
			log.Printf("floor: last swept %s, next due %s (recorded in %s.floor)",
				last.UTC().Format(time.RFC3339), last.Add(cfg.floor).UTC().Format(time.RFC3339), cfg.store)
		}
	} else {
		// Loud, because the floor is the only path for the absence class: the
		// bounded pass reported `ok` with no findings on both fault-badselector
		// and fault-invoicing, so those namespaces never produce a transition.
		log.Printf("WARNING: -floor=0, so nothing will ever run the full agent on a namespace " +
			"that produces no transition. Faults that are an absence — a Service with no " +
			"endpoints, a workload failing only in its logs — will not be found.")
	}

	if cfg.once {
		_, err := s.Cycle(ctx)
		return err
	}
	if err := s.Run(ctx); err != nil && ctx.Err() == nil {
		return err
	}
	log.Printf("stopped")
	return nil
}

// buildNotifier picks where digests go.
//
// switchboard when it is configured, the log otherwise. Both halves of that are
// deliberate. A missing -switchboard-url is not an error, because -once against
// a scratch cluster wants the log and nothing else; but a *partial*
// configuration is, because a URL with no conversation is somebody who meant to
// wire up chat and will otherwise discover at 3am that the digests went to
// stdout.
func buildNotifier(cfg runConfig) (scheduler.Notifier, error) {
	if cfg.sbURL == "" && cfg.sbConv == "" {
		log.Printf("no -switchboard-url: digests will be logged, not posted")
		return &logNotifier{verbose: cfg.verbose}, nil
	}
	if cfg.sbURL == "" || cfg.sbConv == "" {
		return nil, fmt.Errorf("-switchboard-url and -switchboard-conversation must be given together")
	}
	sb, err := notify.New(notify.Config{
		BaseURL:      cfg.sbURL,
		Conversation: cfg.sbConv,
		// Generous, because the ingress calls Slack synchronously: this covers
		// a platform round trip, not just the hop to switchboard.
		Timeout: 30 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	log.Printf("posting digests to %s via %s", cfg.sbConv, cfg.sbURL)
	// Still logged as well. The digest is the operational record of a cycle and
	// a chat outage should not erase it — and a notifier error is otherwise
	// invisible, because scheduler.notify deliberately does not fail a cycle on
	// one.
	return &teeNotifier{to: sb, also: &logNotifier{verbose: cfg.verbose}}, nil
}

// teeNotifier sends a digest to both, and reports a chat failure through the
// log rather than up the stack: the cycle has already happened and failing it
// retroactively would turn a Slack outage into a monitoring outage.
type teeNotifier struct {
	to   scheduler.Notifier
	also scheduler.Notifier
}

func (t *teeNotifier) Notify(ctx context.Context, d scheduler.Digest) error {
	_ = t.also.Notify(ctx, d)
	if err := t.to.Notify(ctx, d); err != nil {
		log.Printf("digest not delivered: %v", err)
	}
	return nil
}

// agentEscalator builds one full agent per escalation and throws it away.
//
// Not resident, for two reasons this repo has measured. A shared session lets
// one namespace's diagnosis leak into the next, which both eval tiers avoid the
// same way. And the orchestrator's context is 93.4% of a run's input tokens, so
// an agent carrying every previous escalation's transcript gets monotonically
// more expensive for no diagnostic benefit.
type agentEscalator struct {
	cfg      runConfig
	main     adkmodel.LLM
	sub      adkmodel.LLM
	withheld []string
	catalog  *pricing.Catalog
}

func (e *agentEscalator) Escalate(ctx context.Context, ns string, why []scheduler.Transition) (*schema.HealthReport, error) {
	live, err := lookout.Toolset(ctx, readonly.LookoutConfig(e.cfg.kubeconfig, e.cfg.kubecontext))
	if err != nil {
		return nil, fmt.Errorf("lookout toolset: %w", err)
	}
	if closer, ok := live.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	enum, err := kuberead.Toolset(kuberead.Config{
		Kubeconfig: e.cfg.kubeconfig,
		Context:    e.cfg.kubecontext,
	})
	if err != nil {
		return nil, fmt.Errorf("kuberead toolset: %w", err)
	}

	toolsets := []tool.Toolset{readonly.WithoutTools(live, e.withheld), enum}
	// Re-asserted per escalation rather than once at startup. It is one
	// enumeration against an in-process toolset, and the thing it guards
	// against — a toolset that grew a write surface — is exactly the kind of
	// change that would arrive between cycles in a long-lived process.
	if err := readonly.RefuseWriteTools(ctx, toolsets); err != nil {
		return nil, err
	}

	rec := &lookout.Recorder{}
	recorded := make([]tool.Toolset, 0, len(toolsets))
	for _, ts := range toolsets {
		recorded = append(recorded, lookout.Recording(ts, rec))
	}
	// No Writes. The whole reason this command is safe to leave running.
	agent, err := sre.Build(sre.Config{
		Main: e.main, Subagent: e.sub, Toolsets: recorded, SpecDir: e.cfg.specDir,
	})
	if err != nil {
		return nil, fmt.Errorf("build agent: %w", err)
	}

	runner := &evals.Runner{
		Agent: agent, Recorder: rec,
		Limits: evals.Limits(e.catalog, e.cfg.maxCost, e.cfg.maxTurns),
	}
	ex := evals.Example{}
	ex.Inputs.Scenario = prompt(ns, why)

	run, err := evals.RetryPolicy{
		Timeout: e.cfg.timeout,
		For:     e.cfg.retryFor,
		OnRetry: func(wait, left time.Duration) {
			log.Printf("%s rate limited, retrying in %s (%s of budget left)",
				ns, wait.Round(time.Second), left.Round(time.Second))
		},
	}.Run(ctx, runner, ns, ex)
	if err != nil {
		return run.Health, err
	}
	return run.Health, nil
}

// prompt tells the agent which namespace to look at and what changed.
//
// The transitions are handed over because the agent would otherwise spend its
// first several tool calls rediscovering what the cycle already knows. What it
// deliberately does *not* say is what the fault is: the transitions are object
// names and control-plane reason tokens, which is evidence, and the diagnosis
// is still the agent's to make. On the floor sweep there are no transitions and
// the prompt is the one tier 2 uses verbatim.
func prompt(ns string, why []scheduler.Transition) string {
	base := fmt.Sprintf("Assess the health of the %q namespace in this Kubernetes cluster "+
		"and report what you find.", ns)
	if len(why) == 0 {
		return base
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\n\nThis assessment was triggered because the last scan saw these changes " +
		"since the previous one. Treat them as a starting point, not as the diagnosis:\n")
	for _, t := range why {
		fmt.Fprintf(&b, "  - %s %s reason=%s severity=%s", t.Class, t.Target(), t.Reason, t.Severity)
		if t.FirstSeen != "" {
			fmt.Fprintf(&b, " first_seen=%s", t.FirstSeen)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// logNotifier is the stand-in until Slack exists.
//
// Behind scheduler.Notifier rather than inlined, so the Slack implementation is
// a new type rather than an edit to the loop — and so the loop can be tested
// with a notifier that records instead of prints.
type logNotifier struct{ verbose bool }

func (n *logNotifier) Notify(_ context.Context, d scheduler.Digest) error {
	sev := "ok"
	if d.Report != nil {
		sev = string(d.Report.OverallSeverity)
	}
	log.Printf("[%s] %s: %s — %d transition(s), %d escalation(s)",
		d.Cluster, d.Trigger, sev, len(d.Transitions), len(d.Escalations))

	for _, e := range d.CollectErrors {
		log.Printf("  collection: %s", e)
	}
	// Never silently truncated: a cycle that escalated three of eleven
	// namespaces and said nothing about the other eight reads exactly like a
	// cycle where only three changed.
	for _, t := range d.Dropped {
		log.Printf("  DROPPED by the escalation cap: %s %s", t.Class, t.Target())
	}
	// Cluster-scoped: real, and not something a namespace-scoped agent can be
	// sent to investigate. Printed so it is an operator's problem rather than
	// nobody's.
	for _, t := range d.Unscoped {
		log.Printf("  CLUSTER-SCOPED (no namespace to escalate to): %s %s reason=%s severity=%s",
			t.Class, t.Target(), t.Reason, t.Severity)
	}
	// The bounded pass has no handback, so a contract violation is recorded
	// rather than fixed. Printing it is what stops a protested report reading
	// like a clean one.
	for _, p := range d.Protests {
		log.Printf("  UNDER PROTEST: %s", p)
	}
	for _, e := range d.Escalations {
		switch {
		case e.Err != nil:
			log.Printf("  %s: escalation failed after %s: %v", e.Namespace, e.Elapsed.Round(time.Second), e.Err)
		case e.Report != nil:
			log.Printf("  %s: %s (%d findings) in %s", e.Namespace,
				e.Report.OverallSeverity, len(e.Report.Findings), e.Elapsed.Round(time.Second))
		}
	}
	if !n.verbose {
		return nil
	}
	for _, t := range d.Transitions {
		log.Printf("  %-10s %s reason=%s severity=%s", t.Class, t.Target(), t.Reason, t.Severity)
	}
	if d.Report != nil {
		blob, _ := json.MarshalIndent(d.Report, "  ", "  ")
		log.Printf("  bounded report: %s", blob)
	}
	return nil
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
