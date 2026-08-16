// Command sre-agent runs one read-only health assessment against a cluster
// that already exists.
//
// This is the first entry point that is not an eval harness. Both eval tiers
// build their own cluster — tier 1 has none at all, tier 2 creates and destroys
// a kind cluster — so nothing in this repo could be pointed at a cluster
// somebody else made. That is the gap this fills, and the reason it exists is
// that a real cluster tests things a two-object fixture namespace cannot: an
// enumeration of hundreds of objects instead of three, telemetry that is
// actually present, partial RBAC, and whether the agent invents an incident in
// a namespace that is merely noisy.
//
//	source ~/scripts/claude-env.sh
//	kubectl config view --minify --flatten --context=CTX > /tmp/kubeconfig-CTX
//	SRE_LOOKOUT_BIN=... go run ./cmd/sre-agent \
//	  -kubeconfig /tmp/kubeconfig-CTX -context CTX -namespace some-ns -v
//
// # Read-only, structurally
//
// The agent is built with no Config.Writes, so sre.Build does not construct
// change-executor at all and the orchestrator has no delegation target for a
// mutation. That is the same wiring both eval tiers use.
//
// On a cluster where the credentials are admin — which is the normal case, and
// is true of the cluster this was written for — "read-only" is then a property
// of our own wiring rather than of RBAC. So it is asserted rather than
// intended, in two ways, both before the first turn. Every tool on every
// toolset is checked against kubewrite.ToolNames(), and a match is a fatal
// error. And lookout is asked what it advertises: a tool whose own annotation
// says it writes is withheld from the model, or, if this repo has not
// classified it, refuses the run outright (lookoutWriters). Both checks are
// cheap and both fail at startup instead of mid-diagnosis.
//
// Two cluster pins, both refusing to guess. --context is passed on every
// kubectl invocation (internal/kubectl), and the kubeconfig must name that
// context as current *and* describe exactly one context. The second half is
// not redundant: lookout resolves its cluster from KUBECONFIG's current-context
// with no per-call flag, so for that subprocess the file is the only pin there
// is. A kubeconfig describing one cluster cannot reach a second one.
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

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/pricing"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-sre-agent/internal/evals"
	"github.com/go-steer/core-sre-agent/internal/kuberead"
	"github.com/go-steer/core-sre-agent/internal/llm"
	"github.com/go-steer/core-sre-agent/internal/lookout"
	"github.com/go-steer/core-sre-agent/internal/readonly"
	"github.com/go-steer/core-sre-agent/internal/sre"
)

func main() {
	var (
		kubeconfig  = flag.String("kubeconfig", "", "kubeconfig file to use (required; must describe exactly one context)")
		kubecontext = flag.String("context", "", "kube context to pin to (required; must be the kubeconfig's current-context)")
		namespaces  = flag.String("namespace", "", "comma-separated namespaces to assess (required)")
		repeat      = flag.Int("repeat", 1, "assess each namespace this many times, in independent sessions")
		out         = flag.String("out", "", "write the full transcript as JSON to this path")
		specDir     = flag.String("specs", "", "load specialist specs from this directory instead of the embedded set")
		timeout     = flag.Duration("timeout", 8*time.Minute, "per-namespace agent timeout")
		retryFor    = flag.Duration("retry-for", 20*time.Minute, "how long to keep retrying through provider rate limiting")
		maxCost     = flag.Float64("max-cost", 0, "abort a namespace's assessment once it has spent this many USD (0 = unlimited)")
		maxTurns    = flag.Int("max-turns", 0, "abort a namespace's assessment after this many model calls (0 = unlimited)")
		verbose     = flag.Bool("v", false, "log each tool call trajectory as a namespace finishes")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg := runConfig{
		kubeconfig: *kubeconfig, kubecontext: *kubecontext,
		namespaces: splitList(*namespaces), repeat: *repeat,
		out: *out, specDir: *specDir,
		timeout: *timeout, retryFor: *retryFor,
		maxCost: *maxCost, maxTurns: *maxTurns, verbose: *verbose,
	}
	if err := run(ctx, cfg); err != nil {
		log.Fatal(err)
	}
}

type runConfig struct {
	kubeconfig, kubecontext string
	namespaces              []string
	repeat                  int
	out, specDir            string
	timeout, retryFor       time.Duration
	// maxCost and maxTurns bound one namespace's spend. Both default to
	// unlimited: an assessment cut off part-way through reports whatever it
	// had, which reads exactly like an agent that found less than it should
	// have. A ceiling is for a scheduler running this on a cycle, where a
	// looping agent is a bill rather than a bad number, and it should be
	// chosen rather than inherited.
	maxCost  float64
	maxTurns int
	verbose  bool
}

// assessment is one namespace's run, kept for the transcript.
type assessment struct {
	Namespace string    `json:"namespace"`
	Attempt   int       `json:"attempt"`
	Prompt    string    `json:"prompt"`
	Run       evals.Run `json:"run"`
	Error     string    `json:"error,omitempty"`
	Elapsed   string    `json:"elapsed"`
}

func run(ctx context.Context, cfg runConfig) error {
	if cfg.kubeconfig == "" || cfg.kubecontext == "" {
		return fmt.Errorf("-kubeconfig and -context are both required; this command does not resolve the ambient current-context")
	}
	if len(cfg.namespaces) == 0 {
		// No implicit cluster-wide sweep. On a real cluster that is a much
		// larger and much more expensive question than it looks, and it should
		// be asked deliberately rather than by omitting a flag.
		return fmt.Errorf("-namespace is required: name the namespaces to assess")
	}
	if cfg.repeat < 1 {
		return fmt.Errorf("-repeat must be at least 1")
	}
	if err := readonly.VerifySoleContext(cfg.kubeconfig, cfg.kubecontext); err != nil {
		return err
	}
	// Asked once, before the first namespace: what does this lookout release
	// actually declare? The answer decides whether the run proceeds and which
	// tools it withholds, so it has to come from the binary rather than from
	// the capture in internal/lookout.
	withheld, err := readonly.LookoutWriters(ctx, cfg.kubeconfig, cfg.kubecontext)
	if err != nil {
		return err
	}

	mainModel, subModel, err := llm.Models(ctx)
	if err != nil {
		return fmt.Errorf("resolve models: %w", err)
	}
	roster, err := sre.SpecialistNames(cfg.specDir)
	if err != nil {
		return fmt.Errorf("load specialist roster: %w", err)
	}
	// One catalog for every namespace in this invocation, resolved before the
	// first assessment. A pricing table this cannot build is fatal only when a
	// cost ceiling was actually asked for: unpriced calls fall through to the
	// flat fallback, so -max-cost would stop meaning dollars. Without the flag
	// it costs the footer's dollar column and nothing else.
	cat, catErr := evals.Catalog()
	if catErr != nil {
		if cfg.maxCost > 0 {
			return fmt.Errorf("pricing catalog (needed by -max-cost): %w", catErr)
		}
		log.Printf("pricing unavailable, costs will print as $—: %v", catErr)
	}
	limits := evals.Limits(cat, cfg.maxCost, cfg.maxTurns)

	log.Printf("assessing %d namespace(s) on context %s, read-only", len(cfg.namespaces), cfg.kubecontext)

	var results []assessment
	for _, ns := range cfg.namespaces {
		for attempt := 1; attempt <= cfg.repeat; attempt++ {
			start := time.Now()
			prompt := fmt.Sprintf("Assess the health of the %q namespace and report what you find.", ns)
			r, err := assess(ctx, assessArgs{
				cfg: cfg, ns: ns, prompt: prompt,
				main: mainModel, sub: subModel, roster: roster, limits: limits,
				withheld: withheld,
			})
			a := assessment{
				Namespace: ns, Attempt: attempt, Prompt: prompt,
				Run: r, Elapsed: time.Since(start).Round(time.Millisecond).String(),
			}
			if err != nil {
				a.Error = err.Error()
				log.Printf("%s: %v", ns, err)
			}
			results = append(results, a)
			printAssessment(a, cat, cfg.verbose)
			if ctx.Err() != nil {
				return ctx.Err()
			}
		}
	}

	if cfg.out != "" {
		blob, err := json.MarshalIndent(results, "", "  ")
		if err != nil {
			return fmt.Errorf("encode transcript: %w", err)
		}
		if err := os.WriteFile(cfg.out, blob, 0o600); err != nil {
			return fmt.Errorf("write transcript: %w", err)
		}
		log.Printf("transcript: %s", cfg.out)
	}
	return nil
}

type assessArgs struct {
	cfg       runConfig
	ns        string
	prompt    string
	main, sub adkmodel.LLM
	roster    []string
	limits    budget.Limits

	// withheld are the lookout tools this run must not offer the model,
	// resolved once from the binary's own annotations (lookoutWriters).
	withheld []string
}

// assess builds a fresh agent with its own lookout subprocess and assesses one
// namespace.
//
// Fresh per namespace, matching both eval tiers: a shared session would let one
// namespace's diagnosis leak into the next, which on a real cluster would also
// mean carrying an unbounded transcript into every subsequent turn.
func assess(ctx context.Context, a assessArgs) (evals.Run, error) {
	live, err := lookout.Toolset(ctx, readonly.LookoutConfig(a.cfg.kubeconfig, a.cfg.kubecontext))
	if err != nil {
		return evals.Run{}, fmt.Errorf("lookout toolset: %w", err)
	}
	if closer, ok := live.(interface{ Close() error }); ok {
		defer closer.Close()
	}
	live = readonly.WithoutTools(live, a.withheld)

	enum, err := kuberead.Toolset(kuberead.Config{
		Kubeconfig: a.cfg.kubeconfig,
		Context:    a.cfg.kubecontext,
	})
	if err != nil {
		return evals.Run{}, fmt.Errorf("kuberead toolset: %w", err)
	}

	rec := &lookout.Recorder{}
	toolsets := []tool.Toolset{
		lookout.Recording(live, rec),
		lookout.Recording(enum, rec),
	}
	if err := readonly.RefuseWriteTools(ctx, toolsets); err != nil {
		return evals.Run{}, err
	}

	// No Writes: change-executor is not built, so there is no delegation
	// target for a mutation at all.
	agent, err := sre.Build(sre.Config{
		Main:     a.main,
		Subagent: a.sub,
		Toolsets: toolsets,
		SpecDir:  a.cfg.specDir,
	})
	if err != nil {
		return evals.Run{}, fmt.Errorf("build agent: %w", err)
	}

	runner := &evals.Runner{
		Agent:       agent,
		Recorder:    rec,
		Specialists: a.roster,
		Limits:      a.limits,
	}
	ex := evals.Example{}
	ex.Inputs.Scenario = a.prompt

	return evals.RetryPolicy{
		Timeout: a.cfg.timeout,
		For:     a.cfg.retryFor,
		OnRetry: func(wait, left time.Duration) {
			log.Printf("%s rate limited, retrying in %s (%s of budget left)",
				a.ns, wait.Round(time.Second), left.Round(time.Second))
		},
	}.Run(ctx, runner, a.ns, ex)
}

func printAssessment(a assessment, cat *pricing.Catalog, verbose bool) {
	// A budget stop is named as one. The assessment below it is short because
	// the operator's ceiling cut it off, not because the namespace was quiet.
	if evals.ExceededBudget(a.Error) {
		fmt.Printf("\n=== %s (attempt %d, %s) — STOPPED ON BUDGET: %s\n",
			a.Namespace, a.Attempt, a.Elapsed, a.Error)
	}
	h := a.Run.Health
	if h == nil {
		fmt.Printf("\n=== %s (attempt %d, %s) — NO STRUCTURED REPORT\n", a.Namespace, a.Attempt, a.Elapsed)
		if a.Run.Response != "" {
			fmt.Printf("%s\n", a.Run.Response)
		}
		printFooter(a, cat, verbose)
		return
	}
	fmt.Printf("\n=== %s (attempt %d, %s) — %s\n%s\n",
		a.Namespace, a.Attempt, a.Elapsed, strings.ToUpper(string(h.OverallSeverity)), h.Summary)
	for _, f := range h.Findings {
		fmt.Printf("  [%s] %s/%s reason=%s — %s\n",
			f.Severity, f.Kind, f.ResourceName, f.Reason, f.Title)
	}
	for _, action := range h.RecommendedActions {
		fmt.Printf("  → %s\n", action)
	}
	printFooter(a, cat, verbose)
}

// printFooter prints what the assessment cost: calls, delegations, stalls and
// tokens. It runs on the no-report path too, and that is the path it matters
// most on — a run whose read tools were all failing still spent the tokens, and
// the first tier-3 attempt burned 66 calls and five stalls on an auth error
// with no way to say what that came to.
func printFooter(a assessment, cat *pricing.Catalog, verbose bool) {
	fmt.Printf("  %d tool calls", len(a.Run.Trajectory))
	if len(a.Run.Delegations) > 0 {
		fmt.Printf(", delegated to %s", strings.Join(a.Run.Delegations, ","))
	}
	if len(a.Run.Stalls) > 0 {
		fmt.Printf(", STALLED: %s", strings.Join(a.Run.Stalls, ","))
	}
	// Uppercase for the same reason STALLED is: both say the report you are
	// about to read is not the one the contract asks for, and on this command
	// there is no aggregate underneath to notice it in.
	if len(a.Run.Protests) > 0 {
		fmt.Printf(", UNDER PROTEST: %s", strings.Join(a.Run.Protests, "; "))
	}
	fmt.Println()
	// A nil catalog costs the dollar column and nothing else; the assessment
	// has already run, and an unpriced group renders "$—".
	for _, line := range evals.Lines(a.Run.Usage.ByModel(cat)) {
		fmt.Printf("  %s\n", line)
	}
	// The per-agent split is the one that explains the eleven-fold spread
	// between the 8-call and 91-call assessments of the same namespace: the
	// model line says what the run cost, and this says which agent it went to.
	for _, line := range evals.Lines(a.Run.Usage.ByAgent(cat)) {
		fmt.Printf("    %s\n", line)
	}
	if verbose {
		fmt.Printf("  trajectory: %s\n", strings.Join(a.Run.Trajectory, " "))
	}
}

func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}
