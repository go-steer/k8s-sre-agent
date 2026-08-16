// Command sre-eval-live runs the tier-2 evals against a real, broken cluster.
//
// It creates a throwaway kind cluster, injects faults into one namespace each,
// waits for each fault to actually manifest, and asks the agent to assess a
// namespace without saying what is wrong with it. Scoring is on the structured
// HealthReport: did the agent name the right object with the right failure
// mode, and did it invent any failure that is not there.
//
// This is the tier that measures diagnosis. Tier 1 states the fault in the
// prompt, so it can only measure narration and tool selection.
//
//	source ~/scripts/claude-env.sh
//	go build -o /tmp/lookout ../k8s-lookout/cmd/lookout
//	SRE_LOOKOUT_BIN=/tmp/lookout go run ./cmd/sre-eval-live -v
//
// # Safety
//
// The cluster is created by this command, into a kubeconfig this command
// makes, and torn down by it. internal/kindcluster refuses to touch a cluster
// it did not create and verifies the kubeconfig describes exactly one context.
// The ambient current-context is never resolved. See AGENTS.md.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/pricing"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-sre-agent/internal/bounded"
	"github.com/go-steer/core-sre-agent/internal/evals"
	"github.com/go-steer/core-sre-agent/internal/faults"
	"github.com/go-steer/core-sre-agent/internal/kindcluster"
	"github.com/go-steer/core-sre-agent/internal/kuberead"
	"github.com/go-steer/core-sre-agent/internal/llm"
	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/sre"
)

func main() {
	var (
		out       = flag.String("out", "", "write the full transcript as JSON to this path")
		only      = flag.String("only", "", "comma-separated fixture names to run (default: all)")
		conc      = flag.Int("concurrency", 3, "fixtures scored in flight at once")
		specDir   = flag.String("specs", "", "load specialist specs from this directory instead of the embedded set")
		timeout   = flag.Duration("timeout", 8*time.Minute, "per-fixture agent timeout")
		settle    = flag.Duration("settle", 5*time.Minute, "how long to wait for injected faults to manifest")
		retryFor  = flag.Duration("retry-for", 20*time.Minute, "how long to keep retrying one fixture through provider rate limiting")
		keep      = flag.Bool("keep", false, "leave the cluster running for inspection (you must delete it)")
		clusterID = flag.String("cluster-id", "", "suffix for the cluster name (default: the pid)")
		maxCost   = flag.Float64("max-cost", 0, "abort a fixture once it has spent this many USD (0 = unlimited)")
		maxTurns  = flag.Int("max-turns", 0, "abort a fixture after this many model calls (0 = unlimited)")
		useBnd    = flag.Bool("bounded", false, "score the bounded pass instead of the agent (see internal/bounded)")
		verbose   = flag.Bool("v", false, "print each fixture's scores as it finishes")
	)
	flag.Parse()

	// A ^C between "cluster created" and "cluster deleted" is the one way this
	// command leaks a container. Trapping it makes teardown the default even
	// when the run is abandoned.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := runConfig{
		out: *out, only: *only, conc: *conc, specDir: *specDir,
		timeout: *timeout, settle: *settle, retryFor: *retryFor,
		keep: *keep, clusterID: *clusterID, verbose: *verbose,
		maxCost: *maxCost, maxTurns: *maxTurns, bounded: *useBnd,
	}
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

type runConfig struct {
	out, only, specDir, clusterID string
	conc                          int
	timeout, settle, retryFor     time.Duration
	keep, verbose                 bool

	// maxCost and maxTurns bound one fixture's spend, and both default to
	// unlimited. A ceiling that fires mid-suite produces a fixture that reads
	// exactly like a bad diagnosis — fewer findings, lower recall — so it is
	// opt-in, and report() labels the stop rather than filing it as a failure.
	maxCost  float64
	maxTurns int

	// bounded swaps the producer: the same fixtures and the same four
	// evaluators, scoring internal/bounded instead of the agent.
	//
	// This is the whole reason the bounded pass emits a schema.HealthReport
	// rather than something of its own. Both evaluators and every fixture work
	// against any producer of one, so the cost of the scheduled path is
	// publishable as a delta against the agent's own numbers instead of being
	// argued about — and fault-badselector is the falsifiable prediction, since
	// a Service selecting nothing in front of a healthy Deployment is the
	// absence class the bounded pass structurally cannot reach.
	bounded bool
}

// result is one fixture's outcome, kept for the transcript.
type result struct {
	Fixture string        `json:"fixture"`
	Prompt  string        `json:"prompt"`
	Run     evals.Run     `json:"run"`
	Scores  []evals.Score `json:"scores"`
	Error   string        `json:"error,omitempty"`
	Elapsed string        `json:"elapsed"`
	// InjectError is kept separate from Error because it means something
	// different: the harness failed to break the cluster, so the fixture
	// measured nothing about the agent.
	InjectError string `json:"inject_error,omitempty"`
}

func run(ctx context.Context, cfg runConfig) error {
	fixtures := faults.All()
	if cfg.only != "" {
		var err error
		if fixtures, err = faults.ByName(strings.Split(cfg.only, ",")); err != nil {
			return err
		}
	}

	// Models and the lookout binary are resolved before the cluster is created:
	// both are common failure modes, and finding out after a two-minute cluster
	// build is a waste of two minutes.
	mainModel, subModel, err := llm.Models(ctx)
	if err != nil {
		return fmt.Errorf("resolve models: %w", err)
	}
	roster, err := sre.SpecialistNames(cfg.specDir)
	if err != nil {
		return fmt.Errorf("load specialist roster: %w", err)
	}
	// Resolved here for the same reason as the models: it is cheap, it can
	// fail, and a cost ceiling nobody can price should not be discovered after
	// ten fixtures have been injected into a fresh cluster.
	cat, catErr := evals.Catalog()
	if catErr != nil && cfg.maxCost > 0 {
		return fmt.Errorf("pricing catalog (needed by -max-cost): %w", catErr)
	}
	limits := evals.Limits(cat, cfg.maxCost, cfg.maxTurns)

	id := cfg.clusterID
	if id == "" {
		id = fmt.Sprintf("%d", os.Getpid())
	}
	name := kindcluster.NamePrefix + id

	log.Printf("creating cluster %s", name)
	cluster, err := kindcluster.Create(ctx, kindcluster.Config{Name: name})
	if err != nil {
		return fmt.Errorf("create cluster: %w", err)
	}
	defer func() {
		if cfg.keep {
			log.Printf("keeping cluster %s (kubeconfig %s) — delete it with: kind delete cluster --name %s",
				cluster.Name, cluster.Kubeconfig, cluster.Name)
			return
		}
		// context.WithoutCancel inside Delete: teardown must run even when the
		// run was cancelled, which is precisely when it matters.
		if err := cluster.Delete(context.Background()); err != nil {
			log.Printf("WARNING: could not delete cluster %s: %v", cluster.Name, err)
		} else {
			log.Printf("deleted cluster %s", cluster.Name)
		}
	}()
	log.Printf("cluster ready, pinned to context %s", cluster.Context)

	if err := cluster.LoadImage(ctx, faults.BaseImage); err != nil {
		return fmt.Errorf("preload %s: %w", faults.BaseImage, err)
	}

	log.Printf("injecting %d fixtures", len(fixtures))
	results := make([]result, len(fixtures))
	injectErrs := injectAll(ctx, cluster, fixtures, cfg.settle)
	for i, f := range fixtures {
		results[i] = result{Fixture: f.Name, Prompt: f.Prompt}
		if injectErrs[i] != nil {
			results[i].InjectError = injectErrs[i].Error()
			log.Printf("WARNING: %s did not manifest: %v", f.Name, injectErrs[i])
		}
	}

	sem := make(chan struct{}, max(1, cfg.conc))
	var wg sync.WaitGroup
	for i, f := range fixtures {
		if results[i].InjectError != "" {
			continue // scoring an agent against a fault that never existed measures nothing
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			started := time.Now()
			got, err := scoreOne(ctx, scoreArgs{
				fault: f, cluster: cluster, main: mainModel, sub: subModel,
				roster: roster, specDir: cfg.specDir, timeout: cfg.timeout,
				retryFor: cfg.retryFor, limits: limits, verbose: cfg.verbose,
				bounded: cfg.bounded,
			})
			results[i].Elapsed = time.Since(started).Round(time.Millisecond).String()
			// Keep the partial run on failure too. It is not scored, but the
			// trajectory and delegations up to the break are the only record of
			// what the agent was doing, and a failure with an empty Run is a
			// failure you cannot diagnose after the cluster is gone.
			results[i].Run = got
			if err != nil {
				results[i].Error = err.Error()
				return
			}
			results[i].Scores = evals.ScoreLive(f, got)
			if cfg.verbose {
				log.Printf("%-20s %s %s%s", f.Name, results[i].Elapsed,
					scoreLine(results[i].Scores), delegationSuffix(got))
			}
		}()
	}
	wg.Wait()

	report(results, cat, catErr)
	if cfg.out != "" {
		if err := writeTranscript(cfg.out, results); err != nil {
			return err
		}
		fmt.Printf("\ntranscript: %s\n", cfg.out)
	}

	var failed, uninjected, stopped int
	for _, r := range results {
		switch {
		case r.InjectError != "":
			uninjected++
		case evals.ExceededBudget(r.Error):
			stopped++
		case r.Error != "":
			failed++
		}
	}
	if uninjected > 0 {
		return fmt.Errorf("%d/%d fixtures never manifested — the harness is broken, not the agent",
			uninjected, len(results))
	}
	if failed > 0 {
		return fmt.Errorf("%d/%d fixtures failed to run", failed, len(results))
	}
	// Reported after the failures, and separately: a ceiling the operator set
	// is not the harness breaking, but a suite that hit one did not measure
	// what its numbers claim, so it must not exit clean either.
	if stopped > 0 {
		return fmt.Errorf("%d/%d fixtures stopped on their budget ceiling", stopped, len(results))
	}
	return nil
}

// injectAll injects every fixture concurrently.
//
// Concurrent because the wall-clock cost is almost entirely kubelet backoff
// timers, which run in parallel; serially this is minutes of waiting for
// nothing. Each fixture owns its namespace, so they cannot interfere.
func injectAll(ctx context.Context, c *kindcluster.Cluster, fixtures []faults.Fault, settle time.Duration) []error {
	errs := make([]error, len(fixtures))
	var wg sync.WaitGroup
	for i, f := range fixtures {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = f.Inject(ctx, c, settle)
		}()
	}
	wg.Wait()
	return errs
}

type scoreArgs struct {
	bounded  bool
	fault    faults.Fault
	cluster  *kindcluster.Cluster
	main     adkmodel.LLM
	sub      adkmodel.LLM
	roster   []string
	specDir  string
	timeout  time.Duration
	retryFor time.Duration
	limits   budget.Limits
	verbose  bool
}

// scoreOne builds a fresh agent with its own lookout subprocess and runs one
// fixture.
//
// Fresh per fixture, matching tier 1: the fixtures are independent incidents,
// and a shared session would let one namespace's diagnosis leak into the next
// — which inflates scores in a way that looks like capability.
func scoreOne(ctx context.Context, a scoreArgs) (evals.Run, error) {
	if a.bounded {
		return boundedOne(ctx, a)
	}
	live, err := lookout.Toolset(ctx, lookout.Config{
		Kubeconfig: a.cluster.Kubeconfig,
		Context:    a.cluster.Context,
	})
	if err != nil {
		return evals.Run{}, fmt.Errorf("lookout toolset: %w", err)
	}
	if closer, ok := live.(interface{ Close() error }); ok {
		defer closer.Close()
	}

	// The enumeration tool reads the same cluster, pinned the same way. It is
	// not part of lookout — see internal/kuberead — so it is a second toolset
	// rather than a second check, and it goes through the same recorder so a
	// listing shows up in the trajectory like any other call.
	enum, err := kuberead.Toolset(kuberead.Config{
		Kubeconfig: a.cluster.Kubeconfig,
		Context:    a.cluster.Context,
	})
	if err != nil {
		return evals.Run{}, fmt.Errorf("kuberead toolset: %w", err)
	}

	rec := &lookout.Recorder{}
	agent, err := sre.Build(sre.Config{
		Main:     a.main,
		Subagent: a.sub,
		Toolsets: []tool.Toolset{
			lookout.Recording(live, rec),
			lookout.Recording(enum, rec),
		},
		SpecDir: a.specDir,
	})
	if err != nil {
		return evals.Run{}, fmt.Errorf("build agent: %w", err)
	}

	runner := &evals.Runner{Agent: agent, Recorder: rec, Specialists: a.roster, Limits: a.limits}
	ex := evals.Example{}
	ex.Inputs.Scenario = a.fault.Prompt

	return evals.RetryPolicy{
		Timeout: a.timeout,
		For:     a.retryFor,
		OnRetry: func(wait, left time.Duration) {
			if a.verbose {
				log.Printf("%s rate limited, retrying in %s (%s of budget left)",
					a.fault.Name, wait.Round(time.Second), left.Round(time.Second))
			}
		},
	}.Run(ctx, runner, a.fault.Name, ex)
}

// boundedOne scores the bounded pass over the same fixture.
//
// No agent is built and no toolset is spawned for a model to choose from: the
// two scans are invoked directly, and the single analysis call runs on the
// subagent tier. What comes back is assembled into the same evals.Run the agent
// produces, because the evaluators read a Run and the comparison is only worth
// anything if both sides are measured by identical code.
//
// Three of the Run's fields are filled deliberately rather than left zero. The
// trajectory names the two scans, so a transcript says what was collected
// rather than looking like an agent that used no tools. Usage is attributed to
// bounded.AgentName, so the cost lands in the by-agent table next to
// sre-orchestrator instead of under "unknown". And Protests carries whatever
// the shared report contract rejected — this producer has no handback, so a
// violation is recorded rather than fixed, and the count has to be visible or
// the two producers' reports would not be comparable on quality either.
func boundedOne(ctx context.Context, a scoreArgs) (evals.Run, error) {
	cfg := bounded.Config{
		Lookout: lookout.Config{
			Kubeconfig: a.cluster.Kubeconfig,
			Context:    a.cluster.Context,
		},
		Namespaces: []string{a.fault.Namespace()},
		Model:      a.sub,
	}

	return evals.RetryPolicy{
		Timeout: a.timeout,
		For:     a.retryFor,
		OnRetry: func(wait, left time.Duration) {
			if a.verbose {
				log.Printf("%s rate limited, retrying in %s (%s of budget left)",
					a.fault.Name, wait.Round(time.Second), left.Round(time.Second))
			}
		},
	}.Do(ctx, func(ctx context.Context) (evals.Run, error) {
		res, err := bounded.Check(ctx, cfg)
		run := evals.Run{
			Health:   res.Report,
			Protests: res.Protests,
			Trajectory: []string{
				"lookout health", "lookout triage delta",
			},
			Calls: []lookout.Call{
				{Tool: "lookout health", Args: map[string]any{"namespace": a.fault.Namespace()}},
				{Tool: "lookout triage delta", Args: map[string]any{"namespace": a.fault.Namespace()}},
			},
		}
		if ev := res.Event(); ev != nil {
			run.Usage.Observe(ev)
		}
		if res.Report != nil {
			run.Response = res.Report.Summary
			run.Report = &evals.StructuredResult{
				OverallSeverity: string(res.Report.OverallSeverity),
				Summary:         res.Report.Summary,
			}
		}
		return run, err
	})
}

func report(results []result, cat *pricing.Catalog, catErr error) {
	var scored [][]evals.Score
	for _, r := range results {
		if r.Error == "" && r.InjectError == "" {
			scored = append(scored, r.Scores)
		}
	}
	agg := evals.Summarize(scored)

	names := make([]string, 0, len(agg.Means))
	for n := range agg.Means {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("\n%d fixtures, %d scored\n\n", len(results), agg.N)
	for _, n := range names {
		fmt.Printf("  %-20s %.3f   (scored %d, skipped %d)\n", n, agg.Means[n], agg.Counts[n], agg.Skipped[n])
	}

	reportDelegation(results)

	fmt.Println()
	usage := make([]evals.Usage, 0, len(results))
	for _, r := range results {
		usage = append(usage, r.Run.Usage)
	}
	// A missing pricing catalog costs the dollar column and nothing else — the
	// suite has already run and its tokens are still the record.
	if catErr != nil {
		fmt.Printf("  pricing unavailable: %v\n", catErr)
	}
	for _, line := range evals.UsageSummary(usage, cat) {
		fmt.Printf("  %s\n", line)
	}

	fmt.Printf("\n  per fixture:\n")
	for _, r := range results {
		switch {
		case r.InjectError != "":
			fmt.Printf("    %-22s NOT INJECTED: %s\n", r.Fixture, firstLine(r.InjectError))
		case evals.ExceededBudget(r.Error):
			// Distinct from FAILED: the operator's ceiling fired, and the
			// findings this fixture is missing are missing because of that
			// rather than because the agent did not find them.
			fmt.Printf("    %-22s STOPPED ON BUDGET: %s\n", r.Fixture, firstLine(r.Error))
		case r.Error != "":
			fmt.Printf("    %-22s FAILED: %s\n", r.Fixture, firstLine(r.Error))
		default:
			fmt.Printf("    %-22s %s\n", r.Fixture, scoreLine(r.Scores))
			for _, s := range r.Scores {
				if !s.Skipped && s.Value < 1 {
					fmt.Printf("      %-18s %s\n", s.Name, s.Comment)
				}
			}
		}
	}
}

func writeTranscript(path string, results []result) error {
	var scored [][]evals.Score
	for _, r := range results {
		if r.Error == "" && r.InjectError == "" {
			scored = append(scored, r.Scores)
		}
	}
	blob, err := json.MarshalIndent(map[string]any{
		"aggregate": evals.Summarize(scored),
		"results":   results,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(blob, '\n'), 0o644)
}

// reportDelegation prints the three delegation outcomes for the live tier:
// which specialists were invoked, which came back with an error, and which
// stopped without reporting.
//
// Tier 2 needs this more than tier 1 does, not less. A fixture is one namespace
// with one fault, so the orchestrator can often answer it alone — and a run
// where it did scores identically to one where the roster fanned out and
// agreed. Without the count there is no way to tell a change in the agent's
// judgement from a roster that has gone quiet.
func reportDelegation(results []result) {
	deleg, stalls := map[string]int{}, map[string]int{}
	var used, stalled, errs int
	for _, r := range results {
		if len(r.Run.Delegations) > 0 {
			used++
		}
		for _, d := range r.Run.Delegations {
			deleg[d]++
		}
		if len(r.Run.Stalls) > 0 {
			stalled++
		}
		for _, s := range r.Run.Stalls {
			stalls[evals.StallName(s)]++
		}
		errs += len(r.Run.DelegationErrors)
	}

	fmt.Println()
	if used == 0 {
		fmt.Printf("  delegation: none — the orchestrator answered every fixture alone\n")
	} else {
		fmt.Printf("  delegation: %d/%d fixtures, %d specialists  (%s)\n",
			used, len(results), len(deleg), histogram(deleg))
	}
	fmt.Printf("  delegation errors: %d\n", errs)
	for _, r := range results {
		for _, e := range r.Run.DelegationErrors {
			fmt.Printf("    %-22s %s\n", r.Fixture, firstLine(e))
		}
	}
	if stalled == 0 {
		fmt.Printf("  stalled specialists: none\n")
	} else {
		fmt.Printf("  stalled specialists: %d/%d fixtures  (%s)\n", stalled, len(results), histogram(stalls))
		// The entry carries what the specialist said last, which is the part
		// worth reading: it names the data the read path could not give it.
		for _, r := range results {
			for _, s := range r.Run.Stalls {
				fmt.Printf("    %-22s %s\n", r.Fixture, firstLine(s))
			}
		}
	}
	reportProtests(results)
}

// reportProtests prints how many fixtures had their health report accepted only
// after the report tool ran out of handbacks.
//
// The same kind of line as the stall count and for the same reason.
// maxRejections exists because a rejection loop with -max-turns unlimited costs
// the whole fixture, and accepting under protest costs only what the defect
// cost before the check existed — but a protested report is otherwise
// indistinguishable from a clean one, so without this the bound is a silent
// cap. It has never fired; printing the zero is the point, because "never
// fired" is a claim somebody should be able to read off a run rather than
// assume.
func reportProtests(results []result) {
	var protested int
	for _, r := range results {
		if len(r.Run.Protests) > 0 {
			protested++
		}
	}
	if protested == 0 {
		fmt.Printf("  reports accepted under protest: none\n")
		return
	}
	fmt.Printf("  reports accepted under protest: %d/%d fixtures\n", protested, len(results))
	// The entry is the violations the model could not resolve in three tries,
	// which is the diagnosis: it names the check that could not be satisfied.
	for _, r := range results {
		for _, v := range r.Run.Protests {
			fmt.Printf("    %-22s %s\n", r.Fixture, firstLine(v))
		}
	}
}

// histogram renders a name=count map most-frequent first. The shape matters as
// much as the total: a roster can be nominally "all 8 used" with a specialist
// hanging by one fixture, and only the histogram shows it.
func histogram(counts map[string]int) string {
	names := make([]string, 0, len(counts))
	for n := range counts {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if counts[names[i]] != counts[names[j]] {
			return counts[names[i]] > counts[names[j]]
		}
		return names[i] < names[j]
	})
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s=%d", n, counts[n]))
	}
	return strings.Join(parts, " ")
}

func delegationSuffix(r evals.Run) string {
	if len(r.Delegations) == 0 {
		return ""
	}
	return " via " + strings.Join(r.Delegations, ",")
}

func scoreLine(scores []evals.Score) string {
	parts := make([]string, 0, len(scores))
	for _, s := range scores {
		if s.Skipped {
			parts = append(parts, s.Name+"=skip")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s=%.2f", s.Name, s.Value))
	}
	return strings.Join(parts, " ")
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
