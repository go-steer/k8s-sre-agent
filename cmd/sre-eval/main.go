// Command sre-eval runs the tier-1 scenario evals against the Go SRE agent.
//
// It replays the upstream project's 31 prose scenarios with no cluster
// attached: lookout's real tool surface is presented, but every check returns
// "offline" instead of telemetry. That measures which checks the agent
// chooses and how it reports — not whether it can find a fault, which is what
// the live tier is for.
//
//	source ~/scripts/claude-env.sh
//	go run ./cmd/sre-eval -out /tmp/eval.json
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/go-steer/mast/pkg/pricing"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-sre-agent/internal/evals"
	"github.com/go-steer/core-sre-agent/internal/kuberead"
	"github.com/go-steer/core-sre-agent/internal/llm"
	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/sre"
)

func main() {
	var (
		dataset  = flag.String("dataset", "evals/dataset/upstream-scenarios.jsonl", "tier-1 dataset")
		out      = flag.String("out", "", "write the full transcript as JSON to this path")
		limit    = flag.Int("limit", 0, "run only the first N examples (0 = all)")
		conc     = flag.Int("concurrency", 4, "examples in flight at once")
		specDir  = flag.String("specs", "", "load specialist specs from this directory instead of the embedded set")
		timeout  = flag.Duration("timeout", 5*time.Minute, "per-example timeout")
		retryFor = flag.Duration("retry-for", 20*time.Minute, "how long to keep retrying one example through provider rate limiting")
		maxCost  = flag.Float64("max-cost", 0, "abort an example once it has spent this many USD (0 = unlimited)")
		maxTurns = flag.Int("max-turns", 0, "abort an example after this many model calls (0 = unlimited)")
		verbose  = flag.Bool("v", false, "print each example's scores as it finishes")
	)
	flag.Parse()

	if err := run(*dataset, *out, *specDir, *limit, *conc, *timeout, *retryFor, *maxCost, *maxTurns, *verbose); err != nil {
		log.Fatal(err)
	}
}

// result is one example's outcome, kept for the transcript.
type result struct {
	Index    int            `json:"index"`
	Scenario string         `json:"scenario"`
	Expected []string       `json:"expected_tools"`
	Run      evals.Run      `json:"run"`
	Scores   []evals.Score  `json:"scores"`
	Error    string         `json:"error,omitempty"`
	Elapsed  string         `json:"elapsed"`
	Extra    map[string]any `json:"extra,omitempty"`
}

func run(dataset, out, specDir string, limit, conc int, timeout, retryFor time.Duration, maxCost float64, maxTurns int, verbose bool) error {
	examples, err := evals.LoadJSONL(dataset)
	if err != nil {
		return fmt.Errorf("load dataset: %w", err)
	}
	if limit > 0 && limit < len(examples) {
		examples = examples[:limit]
	}

	// One catalog for the whole suite: it is the same table for every example,
	// and a per-example build would be 31 chances for the layers underneath it
	// to disagree about what a run cost.
	cat, catErr := evals.Catalog()
	if catErr != nil && maxCost > 0 {
		return fmt.Errorf("pricing catalog (needed by -max-cost): %w", catErr)
	}
	limits := evals.Limits(cat, maxCost, maxTurns)

	ctx := context.Background()
	mainModel, subModel, err := llm.Models(ctx)
	if err != nil {
		return fmt.Errorf("resolve models: %w", err)
	}

	roster, err := sre.SpecialistNames(specDir)
	if err != nil {
		return fmt.Errorf("load specialist roster: %w", err)
	}

	evaluators := evals.DefaultEvaluators()
	results := make([]result, len(examples))

	// Each example gets its own agent and its own recorder. One shared
	// recorder across concurrent runs would interleave trajectories and
	// silently give every example credit for every other example's tools.
	sem := make(chan struct{}, max(1, conc))
	var wg sync.WaitGroup
	for i, ex := range examples {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			started := time.Now()
			r := result{
				Index:    i,
				Scenario: ex.Inputs.Scenario,
				Expected: ex.Outputs.ExpectedTools,
			}

			offline, rec, err := lookout.Offline()
			if err != nil {
				r.Error = err.Error()
				results[i] = r
				return
			}
			agent, err := sre.Build(sre.Config{
				Main:     mainModel,
				Subagent: subModel,
				// kuberead is offline here too, and recorded through the same
				// recorder: tier 1's entire signal is which checks the agent
				// chooses, so an unrecorded tool is a hole in tool_coverage
				// rather than a neutral omission.
				Toolsets: []tool.Toolset{
					offline,
					lookout.Recording(kuberead.Offline(lookout.OfflineMessage), rec),
				},
				SpecDir: specDir,
			})
			if err != nil {
				r.Error = err.Error()
				results[i] = r
				return
			}

			runner := &evals.Runner{Agent: agent, Recorder: rec, Specialists: roster, Limits: limits}
			id := fmt.Sprintf("ex-%02d", i)
			got, err := evals.RetryPolicy{
				Timeout: timeout,
				For:     retryFor,
				OnRetry: func(wait, left time.Duration) {
					if verbose {
						log.Printf("%s rate limited, retrying in %s (%s of budget left)",
							id, wait.Round(time.Second), left.Round(time.Second))
					}
				},
			}.Run(ctx, runner, id, ex)
			r.Elapsed = time.Since(started).Round(time.Millisecond).String()
			// The partial run is kept on failure too — unscored, but it is the
			// only record of what the agent had done when it broke.
			r.Run = got
			if err != nil {
				r.Error = err.Error()
				results[i] = r
				return
			}
			for _, e := range evaluators {
				r.Scores = append(r.Scores, e.Score(ex, got))
			}
			results[i] = r
			if verbose {
				var deleg string
				if len(got.Delegations) > 0 {
					deleg = " via " + strings.Join(got.Delegations, ",")
				}
				log.Printf("ex-%02d %s %s%s", i, r.Elapsed, scoreLine(r.Scores), deleg)
			}
		}()
	}
	wg.Wait()

	var scored [][]evals.Score
	var failures, stopped int
	for _, r := range results {
		if r.Error != "" {
			failures++
			if evals.ExceededBudget(r.Error) {
				stopped++
			}
			continue
		}
		scored = append(scored, r.Scores)
	}
	agg := evals.Summarize(scored)

	report(agg, results, failures)
	reportBaseline(examples)
	reportSubmission(results)
	reportDelegation(results)
	reportUsage(results, cat, catErr)

	if out != "" {
		blob, err := json.MarshalIndent(map[string]any{
			"aggregate":    agg,
			"failures":     failures,
			"budget_stops": stopped,
			"results":      results,
		}, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
			return err
		}
		fmt.Printf("\ntranscript: %s\n", out)
	}
	if failures > 0 {
		// The exit is non-zero either way — an unscored example is an unscored
		// example — but the sentence has to say which, because a suite the
		// operator bounded and a suite that broke want opposite next actions.
		if stopped == failures {
			return fmt.Errorf("%d/%d examples stopped on their budget ceiling", stopped, len(results))
		}
		if stopped > 0 {
			return fmt.Errorf("%d/%d examples did not score (%d stopped on budget)", failures, len(results), stopped)
		}
		return fmt.Errorf("%d/%d examples failed to run", failures, len(results))
	}
	return nil
}

func report(agg evals.Aggregate, results []result, failures int) {
	names := make([]string, 0, len(agg.Means))
	for n := range agg.Means {
		names = append(names, n)
	}
	sort.Strings(names)

	fmt.Printf("\n%d examples, %d scored, %d failed\n\n", len(results), agg.N, failures)
	for _, n := range names {
		fmt.Printf("  %-20s %.3f   (scored %d, skipped %d)\n",
			n, agg.Means[n], agg.Counts[n], agg.Skipped[n])
	}

	// Errors are the first thing to look at and the easiest to miss in a
	// score table, so they print after the means rather than scrolling past
	// above them.
	// A budget stop is labelled as one. It is an operator's ceiling doing its
	// job, not the agent or the provider failing, and the two look identical in
	// a list of error strings.
	for _, r := range results {
		switch {
		case r.Error == "":
		case evals.ExceededBudget(r.Error):
			fmt.Printf("\n  ex-%02d STOPPED ON BUDGET: %s\n", r.Index, firstLine(r.Error))
		default:
			fmt.Printf("\n  ex-%02d FAILED: %s\n", r.Index, firstLine(r.Error))
		}
	}
}

// reportBaseline prints what tool_coverage pays out for no investigation at
// all. lookout's k8s_triage_workload answers several kubectl intents in one
// call, so the evaluator's practical floor is well above zero — a tool_coverage
// mean is only meaningful next to it.
//
// Two floors, because one understates it. The second is the broad discovery
// fan-out: three calls an agent can make against any namespace before it has
// read a single result. That is the number a real score has to beat, and the
// gap between the two is how much the subsumption table compounds.
func reportBaseline(examples []evals.Example) {
	one := []string{"k8s_triage_workload"}
	blind := []string{"k8s_triage_workload", "k8s_triage_delta", "k8s_list_resources"}
	reflex, n := evals.ReflexBaseline(examples, one)
	fanout, _ := evals.ReflexBaseline(examples, blind)
	fmt.Printf("\n  tool_coverage baseline: %.3f  (k8s_triage_workload alone, %d examples)\n", reflex, n)
	fmt.Printf("                          %.3f  (blind fan-out: %s)\n", fanout, strings.Join(blind, " + "))
}

// reportSubmission prints how many runs produced a HealthReport at all.
//
// A run that ends without one is a contract failure, not a bad diagnosis, and
// the two need telling apart: it scores 0 on severity and folds into the mean
// looking exactly like an agent that answered and got it wrong. Both examples
// that missed here ended by asking the user a question instead of reporting,
// which is a prompt problem — invisible in an aggregate, obvious in a count.
func reportSubmission(results []result) {
	var submitted, ran int
	for _, r := range results {
		if r.Error != "" {
			continue
		}
		ran++
		if r.Run.Health != nil {
			submitted++
		}
	}
	fmt.Printf("  reported: %d/%d runs submitted a health report\n", submitted, ran)
	for _, r := range results {
		if r.Error == "" && r.Run.Health == nil {
			fmt.Printf("    ex-%02d ended with no report\n", r.Index)
		}
	}
}

// reportUsage prints what the suite cost, per model and per agent.
//
// Not a score, and next to the delegation lines rather than in the table, for
// the same reason: it is a fact about the run that the means cannot express.
// The splits are the part that pays for itself here — the orchestrator and the
// eight specialists are on deliberately different tiers, and a single total
// cannot say whether a fan-out moved work onto the cheap one or simply added to
// the expensive one.
//
// A catalog this cannot build is not a reason to fail a suite that has already
// run: the tokens are still worth printing, and evals.Lines renders an unpriced
// group as "$—" rather than as free.
func reportUsage(results []result, cat *pricing.Catalog, catErr error) {
	usage := make([]evals.Usage, 0, len(results))
	for _, r := range results {
		usage = append(usage, r.Run.Usage)
	}
	if catErr != nil {
		fmt.Printf("\n  pricing unavailable: %v\n", catErr)
	}
	fmt.Println()
	for _, line := range evals.UsageSummary(usage, cat) {
		fmt.Printf("  %s\n", line)
	}
}

// reportDelegation prints how often the orchestrator used a specialist. A
// roster that is never invoked scores identically to one that is, so without
// this the specialist specs could quietly become dead config.
//
// The outcome lines print unconditionally, including their zeroes. A count that
// only appears when it is non-zero is indistinguishable from a count nobody
// wired up, which is exactly how an inert roster reported healthy delegation
// across two published baselines.
func reportDelegation(results []result) {
	counts := map[string]int{}
	var used int
	for _, r := range results {
		if len(r.Run.Delegations) == 0 {
			continue
		}
		used++
		for _, d := range r.Run.Delegations {
			counts[d]++
		}
	}
	if used == 0 {
		fmt.Printf("  delegation: none — the orchestrator answered every example alone\n")
	} else {
		fmt.Printf("  delegation: %d/%d examples  (%s)\n", used, len(results), histogram(counts))
	}

	errs := 0
	for _, r := range results {
		errs += len(r.Run.DelegationErrors)
	}
	fmt.Printf("  delegation errors: %d\n", errs)
	for _, r := range results {
		for _, e := range r.Run.DelegationErrors {
			fmt.Printf("    ex-%02d %s\n", r.Index, firstLine(e))
		}
	}

	reportStalls(results)
	reportProtests(results)
}

// reportProtests prints how many runs had their health report accepted only
// after the report tool ran out of handbacks.
//
// The same kind of line as reportStalls and for the same reason. maxRejections
// exists because a rejection loop with -max-turns unlimited costs the whole
// run, and accepting under protest costs only what the defect cost before the
// check existed — but a protested report is otherwise indistinguishable from a
// clean one, so without this the bound is a silent cap. It has never fired;
// printing the zero is the point, because "never fired" is a claim somebody
// should be able to read off a run rather than assume.
func reportProtests(results []result) {
	var runs int
	for _, r := range results {
		if len(r.Run.Protests) > 0 {
			runs++
		}
	}
	if runs == 0 {
		fmt.Printf("  reports accepted under protest: none\n")
		return
	}
	fmt.Printf("  reports accepted under protest: %d/%d runs\n", runs, len(results))
	// The entry is the violations the model could not resolve in three tries,
	// which is the diagnosis: it names the check that could not be satisfied.
	for _, r := range results {
		for _, v := range r.Run.Protests {
			fmt.Printf("    ex-%02d %s\n", r.Index, firstLine(v))
		}
	}
}

// reportStalls prints how many runs contained a specialist that stopped without
// reporting and had to be closed out by the stall guard.
//
// This is a guard line, not a score, and it exists because the fix made the
// failure survivable. A stalled specialist used to take the run down, which is
// impossible to miss; now it costs one section of the answer, which is
// impossible to see in an aggregate — the run completes, the orchestrator
// reports, and the mean moves by however much the missing area was worth.
func reportStalls(results []result) {
	counts := map[string]int{}
	var runs int
	for _, r := range results {
		if len(r.Run.Stalls) == 0 {
			continue
		}
		runs++
		for _, s := range r.Run.Stalls {
			counts[evals.StallName(s)]++
		}
	}
	if runs == 0 {
		fmt.Printf("  stalled specialists: none\n")
		return
	}
	fmt.Printf("  stalled specialists: %d/%d runs  (%s)\n", runs, len(results), histogram(counts))
	// The entry carries what the specialist said last, which is the part worth
	// reading: it names the data the read path could not give it.
	for _, r := range results {
		for _, s := range r.Run.Stalls {
			fmt.Printf("    ex-%02d %s\n", r.Index, firstLine(s))
		}
	}
}

// histogram renders a name=count map most-frequent first. The shape matters as
// much as the total: a roster can be nominally "all 8 used" with a specialist
// hanging by one example, and only the histogram shows it.
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
