package evals

import (
	"strings"
	"testing"

	"github.com/go-steer/mast/pkg/pricing"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func usageEvent(mv string, prompt, cached, out, thoughts int32) *session.Event {
	return authoredUsageEvent("", mv, prompt, cached, out, thoughts)
}

func authoredUsageEvent(author, mv string, prompt, cached, out, thoughts int32) *session.Event {
	return &session.Event{
		Author: author,
		LLMResponse: model.LLMResponse{
			ModelVersion: mv,
			UsageMetadata: &genai.GenerateContentResponseUsageMetadata{
				PromptTokenCount:        prompt,
				CachedContentTokenCount: cached,
				CandidatesTokenCount:    out,
				ThoughtsTokenCount:      thoughts,
			},
		},
	}
}

func testCatalog(t *testing.T) *pricing.Catalog {
	t.Helper()
	cat, err := Catalog()
	if err != nil {
		t.Fatalf("Catalog: %v", err)
	}
	return cat
}

// group finds one rendered row by name. Fails rather than returning a zero
// value: a breakdown missing the row under test would otherwise assert
// successfully against every field being 0.
func group(t *testing.T, groups []Group, name string) Group {
	t.Helper()
	for _, g := range groups {
		if g.Name == name {
			return g
		}
	}
	t.Fatalf("no group %q in %v", name, groups)
	return Group{}
}

func TestUsageSumsPerModel(t *testing.T) {
	var u Usage
	u.Observe(usageEvent("claude-sonnet-5", 1000, 800, 50, 10))
	u.Observe(usageEvent("claude-sonnet-5", 2000, 1900, 70, 0))
	u.Observe(usageEvent("claude-haiku-4-5", 500, 0, 30, 0))

	models := u.ByModel(testCatalog(t))
	if got, want := len(models), 2; got != want {
		t.Fatalf("models = %d, want %d (%v)", got, want, models)
	}
	main := group(t, models, "claude-sonnet-5")
	if main.Input != 3000 || main.Cached != 2700 || main.Output != 120 || main.Thoughts != 10 {
		t.Errorf("main tier = %+v", main.Tokens)
	}
	if main.Requests != 2 {
		t.Errorf("main tier requests = %d, want 2", main.Requests)
	}

	// The whole reason for the split: one total cannot tell these two tiers
	// apart, and they are billed differently.
	if total := u.Total(); total.Input != 3500 || total.Output != 150 || total.Requests != 3 {
		t.Errorf("Total() = %+v", total)
	}
	// Biggest spender first, so two baselines are diffable regardless of the
	// order the runs finished in.
	if models[0].Name != "claude-sonnet-5" {
		t.Errorf("groups are not ordered by spend: %v", models)
	}
}

// The model split cannot answer the question this repo keeps asking. Two
// agents on the same tier are one row in the model view, so "what did the
// fan-out cost" is unanswerable from it — and change-executor deliberately
// shares the orchestrator's model, which is exactly the pair that collapses.
func TestUsageSplitsByAgentWithinOneModel(t *testing.T) {
	cat := testCatalog(t)
	var u Usage
	u.Observe(authoredUsageEvent("sre-orchestrator", "claude-sonnet-5", 1000, 0, 100, 0))
	u.Observe(authoredUsageEvent("pod-inspector", "claude-sonnet-5", 4000, 0, 200, 0))

	if got, want := len(u.ByModel(cat)), 1; got != want {
		t.Fatalf("models = %d, want %d — the premise of this test is that they share a tier", got, want)
	}
	agents := u.ByAgent(cat)
	if got := group(t, agents, "pod-inspector").Input; got != 4000 {
		t.Errorf("pod-inspector input = %d, want 4000", got)
	}
	if got := group(t, agents, "sre-orchestrator").Input; got != 1000 {
		t.Errorf("sre-orchestrator input = %d, want 1000", got)
	}
	// Both views are folds of the same rows, so they must agree on the money
	// as well as the tokens. A caller that reads one and reports the other's
	// figure would be silently wrong.
	if byModel, byAgent := sumUSD(u.ByModel(cat)), sumUSD(agents); byModel != byAgent {
		t.Errorf("by-model cost $%.6f != by-agent cost $%.6f", byModel, byAgent)
	}

	// And the aggregate has to carry it: Add losing the agent key would leave
	// every suite-level summary blind to the split, with nothing failing.
	var suite Usage
	suite.Add(u)
	suite.Add(u)
	if got := group(t, suite.ByAgent(cat), "pod-inspector").Requests; got != 2 {
		t.Errorf("aggregated pod-inspector requests = %d, want 2", got)
	}
	if block := strings.Join(UsageSummary([]Usage{u}, cat), "\n"); !strings.Contains(block, "pod-inspector") {
		t.Errorf("summary drops the per-agent breakdown:\n%s", block)
	}
}

func sumUSD(groups []Group) float64 {
	var out float64
	for _, g := range groups {
		out += g.USD
	}
	return out
}

// Pricing a per-agent view is only possible because a span carries both keys.
// Here the two agents are on different tiers and spent identical tokens, so
// any implementation that priced a *group* rather than each span would give
// them the same cost — and the whole tiering decision would be unmeasurable.
func TestPerAgentCostUsesEachAgentsOwnTier(t *testing.T) {
	cat := testCatalog(t)
	var u Usage
	u.Observe(authoredUsageEvent("sre-orchestrator", "claude-sonnet-5", 1_000_000, 0, 0, 0))
	u.Observe(authoredUsageEvent("pod-inspector", "claude-haiku-4-5-20251001", 1_000_000, 0, 0, 0))

	agents := u.ByAgent(cat)
	main := group(t, agents, "sre-orchestrator")
	sub := group(t, agents, "pod-inspector")
	if main.Unpriced || sub.Unpriced {
		t.Fatalf("both tiers should price from the builtin catalog: %+v %+v", main, sub)
	}
	// The dated Vertex model ID resolves through the catalog's longest-prefix
	// fallback; an exact-match-only lookup would silently render "$—" for
	// every subagent call this repo has ever made.
	if sub.USD <= 0 {
		t.Errorf("subagent tier priced at $%.6f — the dated model ID did not resolve", sub.USD)
	}
	if main.USD <= sub.USD {
		t.Errorf("main tier $%.6f is not dearer than the subagent tier $%.6f on identical tokens", main.USD, sub.USD)
	}
}

// Cached input is billed at a tenth of fresh input, and 63% of this agent's
// prompt tokens are cache reads. Billing the whole prompt at the input rate
// would overstate every figure the record exists to produce.
func TestCachedInputIsBilledAtTheCacheRate(t *testing.T) {
	cat := testCatalog(t)
	var fresh, cached Usage
	fresh.Observe(usageEvent("claude-sonnet-5", 1_000_000, 0, 0, 0))
	cached.Observe(usageEvent("claude-sonnet-5", 1_000_000, 1_000_000, 0, 0))

	hot := group(t, cached.ByModel(cat), "claude-sonnet-5").USD
	cold := group(t, fresh.ByModel(cat), "claude-sonnet-5").USD
	if hot >= cold {
		t.Fatalf("cache-read input priced at $%.4f, no cheaper than fresh input at $%.4f", hot, cold)
	}
}

// An unknown rate is not a free run, and the two must not render alike — the
// same distinction Empty draws between "not measured" and "zero".
func TestAnUnknownRateIsNotZero(t *testing.T) {
	cat := testCatalog(t)
	var u Usage
	u.Observe(usageEvent("some-model-nobody-priced", 1_000_000, 0, 1000, 0))

	g := group(t, u.ByModel(cat), "some-model-nobody-priced")
	if !g.Unpriced {
		t.Fatalf("unknown model reported as priced at $%.4f", g.USD)
	}
	if line := Lines([]Group{g})[0]; !strings.Contains(line, "$—") || strings.Contains(line, "$0.0000") {
		t.Errorf("unpriced group renders as free: %s", line)
	}
	// And a nil catalog is the same case, not a panic and not a zero.
	if !group(t, u.ByModel(nil), "some-model-nobody-priced").Unpriced {
		t.Error("a nil catalog reported a priced group")
	}
}

// An event that reports no usage must leave the record untouched. Writing a
// zero would make "the provider told us nothing" read exactly like "this run
// was free", which is the confusion the whole record exists to prevent — and
// most events in a run are tool responses with no usage at all.
func TestUsageDistinguishesUnmeasuredFromZero(t *testing.T) {
	var u Usage
	u.Observe(nil)
	u.Observe(&session.Event{})
	u.Observe(&session.Event{LLMResponse: model.LLMResponse{ModelVersion: "claude-sonnet-5"}})

	if !u.Empty() {
		t.Fatalf("Empty() = false after only usage-free events: %+v", u.Spans)
	}
	if total := u.Total(); total.Requests != 0 {
		t.Errorf("Total().Requests = %d, want 0", total.Requests)
	}

	// A genuine zero-token response is measured, and is not the same thing.
	u.Observe(usageEvent("claude-sonnet-5", 0, 0, 0, 0))
	if u.Empty() {
		t.Error("Empty() = true after a real, if empty, model response")
	}
	if got := group(t, u.ByModel(testCatalog(t)), "claude-sonnet-5").Requests; got != 1 {
		t.Errorf("requests = %d, want 1", got)
	}
}

// A suite total is a sum over the runs that reported, and the printed block
// has to say how many that was. Half the fixtures failing before they reached
// the model produces a total that looks entirely reasonable on its own.
func TestUsageSummaryReportsCoverage(t *testing.T) {
	cat := testCatalog(t)
	var measured Usage
	measured.Observe(usageEvent("claude-sonnet-5", 1000, 400, 100, 0))

	block := strings.Join(UsageSummary([]Usage{measured, {}, {}}, cat), "\n")
	if !strings.Contains(block, "1/3 runs measured") {
		t.Errorf("summary does not disclose coverage:\n%s", block)
	}
	// Fresh input is the figure that moves with the trajectory; printing the
	// prompt total alone would hide the system-prompt cache entirely.
	if !strings.Contains(block, "fresh 600") {
		t.Errorf("summary does not separate cached from fresh input:\n%s", block)
	}

	none := strings.Join(UsageSummary([]Usage{{}, {}}, cat), "\n")
	if !strings.Contains(none, "not measured") {
		t.Errorf("a suite that measured nothing must say so, got:\n%s", none)
	}
	if strings.Contains(none, "in=0") {
		t.Errorf("an unmeasured suite must not print a zero total:\n%s", none)
	}
}

// Vertex does not always echo the model back, and an event can arrive without
// an author. Cost that cannot be attributed is still cost, so it is recorded
// rather than dropped.
func TestUsageKeepsUnattributedCost(t *testing.T) {
	var u Usage
	u.Observe(usageEvent("", 100, 0, 10, 0))
	cat := testCatalog(t)
	if got := group(t, u.ByModel(cat), "unknown").Input; got != 100 {
		t.Errorf("unknown model input = %d, want 100", got)
	}
	if got := group(t, u.ByAgent(cat), "unknown").Input; got != 100 {
		t.Errorf("unknown agent input = %d, want 100", got)
	}
}

// An over-reported cached counter must not produce negative fresh input.
//
// This is a provider quirk core-agent's usage tracker already guards against,
// and the unguarded subtraction fails in the direction nobody audits: fresh
// input goes negative, and CostUSDWithCache bills it at the *input* rate, so
// the run comes out cheaper the more the provider miscounts. Cheap and wrong
// is the one error a cost figure never gets challenged on.
func TestAnOverReportedCacheCounterCannotCreditTheRun(t *testing.T) {
	cat := testCatalog(t)

	var u Usage
	u.Observe(usageEvent("claude-sonnet-5", 1000, 1500, 100, 0))

	if fresh := u.Total().Fresh(); fresh != 0 {
		t.Errorf("Fresh() = %d, want 0 — the two halves must still sum to Input", fresh)
	}
	g := group(t, u.ByModel(cat), "claude-sonnet-5")
	if g.USD <= 0 {
		t.Errorf("cost = %v, want positive — a miscounted cache read is not a refund", g.USD)
	}

	// And it must not beat the price of the same call fully cached, which is
	// the cheapest that prompt can honestly be.
	var honest Usage
	honest.Observe(usageEvent("claude-sonnet-5", 1000, 1000, 100, 0))
	if want := group(t, honest.ByModel(cat), "claude-sonnet-5").USD; g.USD != want {
		t.Errorf("cost = %v, want %v (clamped to a fully-cached prompt)", g.USD, want)
	}
}
