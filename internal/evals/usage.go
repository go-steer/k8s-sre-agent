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

package evals

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/go-steer/mast/pkg/pricing"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// Tokens is what one model was asked for and produced across a run.
//
// Input counts the whole prompt, and Cached is the part of it the provider
// served from cache rather than reprocessing — a subset of Input, not a
// separate bucket. Keeping them apart is not bookkeeping fussiness: the
// provider is built with CacheSystem enabled (internal/llm), cache reads are
// priced at a tenth of fresh input, and a single input figure would overstate
// every number this record exists to produce. Subtract to get fresh input.
type Tokens struct {
	Input    int64 `json:"input"`
	Cached   int64 `json:"cached,omitempty"`
	Output   int64 `json:"output"`
	Thoughts int64 `json:"thoughts,omitempty"`
	// Requests is how many model responses these totals were summed from.
	// A per-request mean is the only way to tell a run that made one large
	// call from one that made twenty small ones, and those cost differently
	// once caching is in play.
	Requests int `json:"requests"`
}

// Fresh is the input the cache did not cover — the figure that moves with the
// trajectory, and the one billed at the full rate.
func (t Tokens) Fresh() int64 { fresh, _ := t.split(); return fresh }

// split divides Input into its fresh and cached halves, which are billed at
// rates an order of magnitude apart.
//
// Cached is clamped to Input rather than trusted. A provider that over-reports
// the cached counter is a quirk core-agent's usage tracker already documents
// and guards against, and here an unclamped subtraction would produce negative
// fresh tokens billed at the *input* rate — a credit, so the error runs in the
// direction nobody checks. Clamping keeps the two halves summing to Input,
// which is the invariant every figure downstream reads.
func (t Tokens) split() (fresh, cached int64) {
	cached = t.Cached
	if cached > t.Input {
		cached = t.Input
	}
	return t.Input - cached, cached
}

// Span is one (agent, model) pair's consumption within a run.
type Span struct {
	Agent string `json:"agent"`
	Model string `json:"model"`
	Tokens
}

// Usage is a run's token consumption, as a small fact table.
//
// One row per (agent, model) pair, and the two views a caller actually wants
// are both aggregations of it: **by model, which is what can be priced**, and
// **by agent, which is where the cost went**. Keeping the rows rather than the
// two summaries is what lets the second view be priced as well — an agent's
// tokens are only billable once you know its tier, and two independent maps
// throw that pairing away.
//
// Both views are load-bearing here. The agent runs two tiers on purpose (the
// orchestrator on the main model, the eight read specialists on the subagent
// model) and `change-executor` is deliberately placed on the main tier with the
// justification that "writes are rare enough that the tier difference costs
// nothing measurable" — a claim no single total can check, and one the model
// view alone cannot check either, because change-executor shares the
// orchestrator's model. The model is a proxy for the tier; the agent *is* the
// tier.
type Usage struct {
	Spans []Span `json:"spans,omitempty"`
}

// Observe adds one event's usage to u.
//
// Events without usage metadata — tool responses, transfers, anything that did
// not come back from a model — contribute nothing. That is why this reads the
// pointer rather than the struct: a model that reports no usage must leave the
// record alone, not write zeroes into it, or "we did not measure" becomes
// indistinguishable from "it was free".
func (u *Usage) Observe(ev *session.Event) {
	if ev == nil || ev.UsageMetadata == nil {
		return
	}
	// Neither key is guaranteed. Vertex does not always echo the model back,
	// and an event can reach here without an author. Unattributed cost is
	// still cost, so it is recorded under a placeholder rather than dropped —
	// a silently missing tier is the failure mode this whole record exists to
	// avoid, and it is exactly how the first live run came back with all 98
	// requests under one name.
	u.add(fallback(ev.Author), fallback(ev.ModelVersion), tokensOf(ev.UsageMetadata))
}

func fallback(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// tokensOf reads one response's usage.
//
// Thoughts are not added to Output: the Anthropic provider folds thinking
// tokens into the output count already (pkg/providers/anthropic/stream.go), so
// this field is a breakdown of Output, not an addition to it. Adding it would
// bill reasoning twice.
func tokensOf(m *genai.GenerateContentResponseUsageMetadata) Tokens {
	return Tokens{
		Input:    int64(m.PromptTokenCount),
		Cached:   int64(m.CachedContentTokenCount),
		Output:   int64(m.CandidatesTokenCount),
		Thoughts: int64(m.ThoughtsTokenCount),
		Requests: 1,
	}
}

func (u *Usage) add(agent, model string, t Tokens) {
	for i := range u.Spans {
		if u.Spans[i].Agent == agent && u.Spans[i].Model == model {
			u.Spans[i].Tokens = mergeTokens(u.Spans[i].Tokens, t)
			return
		}
	}
	u.Spans = append(u.Spans, Span{Agent: agent, Model: model, Tokens: t})
}

// mergeTokens sums two totals field by field.
func mergeTokens(a, b Tokens) Tokens {
	a.Input += b.Input
	a.Cached += b.Cached
	a.Output += b.Output
	a.Thoughts += b.Thoughts
	a.Requests += b.Requests
	return a
}

// Empty reports whether nothing was measured. Distinguishing this from a
// genuine zero is the reason callers should not print "0 tokens" for a run
// whose provider reported no usage at all.
func (u Usage) Empty() bool { return len(u.Spans) == 0 }

// Total sums every span. Useful for a headline figure; useless for cost, which
// needs the per-model split.
func (u Usage) Total() Tokens {
	var out Tokens
	for _, s := range u.Spans {
		out = mergeTokens(out, s.Tokens)
	}
	return out
}

// Add merges another run's usage into u, for aggregating across a suite.
func (u *Usage) Add(other Usage) {
	for _, s := range other.Spans {
		u.add(s.Agent, s.Model, s.Tokens)
	}
}

// A Group is one row of a rendered breakdown: some set of spans, their tokens,
// and what they cost.
//
// Unpriced is not the same as USD == 0. A model the catalog does not know is a
// real cost nobody can name, and rendering it as free is the same mistake as
// rendering an unmeasured run as free — see Empty.
type Group struct {
	Name string
	Tokens
	USD      float64
	Unpriced bool
}

// ByModel aggregates the run by model. This is the priced view, because a rate
// is a property of a model and of nothing else.
func (u Usage) ByModel(cat *pricing.Catalog) []Group {
	return u.groupBy(cat, func(s Span) string { return s.Model })
}

// ByAgent aggregates the run by the agent that spent the tokens, priced through
// each span's own model. This is the breakdown that answers what a fan-out
// cost — the question the model view cannot answer, since it cannot separate
// two agents that share a tier, nor the eight specialists from each other.
func (u Usage) ByAgent(cat *pricing.Catalog) []Group {
	return u.groupBy(cat, func(s Span) string { return s.Agent })
}

// groupBy folds the spans under key, in first-seen order, pricing each span
// against its own model before summing. Pricing per span rather than per group
// is what makes the per-agent view meaningful: two spans in one group may be
// billed at different rates.
func (u Usage) groupBy(cat *pricing.Catalog, key func(Span) string) []Group {
	var out []Group
	index := map[string]int{}
	for _, s := range u.Spans {
		k := key(s)
		i, ok := index[k]
		if !ok {
			i = len(out)
			index[k] = i
			out = append(out, Group{Name: k})
		}
		usd, priced := cost(cat, s)
		out[i].Tokens = mergeTokens(out[i].Tokens, s.Tokens)
		out[i].USD += usd
		if !priced {
			out[i].Unpriced = true
		}
	}
	// Biggest spender first, name-tiebroken. Sorting rather than keeping
	// first-seen order is what makes two baselines diffable: a suite's runs
	// finish in whatever order the workers return them, so first-seen is
	// deterministic per run and not across a suite.
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Input != out[j].Input {
			return out[i].Input > out[j].Input
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// cost prices one span. A nil catalog, an unknown model and a model the catalog
// knows only as free are three different things; the first two report unpriced,
// and only the third is a real zero.
func cost(cat *pricing.Catalog, s Span) (float64, bool) {
	if cat == nil {
		return 0, false
	}
	rates, ok := cat.Lookup(s.Model)
	if !ok || rates.IsZero() {
		return 0, false
	}
	fresh, cached := s.split()
	return rates.CostUSDWithCache(int(fresh), int(cached), int(s.Output)), true
}

// Catalog is the pricing table these commands quote costs from.
//
// Builtin layer only, deliberately: the operator's `~/.mast/pricing.json` and a
// project `pricing.json` are both machine-local, and a baseline number recorded
// in this repo must not depend on a file that is not. The cost of that choice
// is that a rate change ships with mast rather than with an edit here, which is
// the right way round for a number nobody is meant to tune.
func Catalog() (*pricing.Catalog, error) {
	return pricing.NewCatalog(pricing.Options{})
}

// Lines renders one line per group. Empty when nothing was measured, so a
// caller that prints nothing is telling the truth rather than claiming a free
// run.
func Lines(groups []Group) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		// Fresh input is what the cache did not cover, and it is the figure
		// that moves with the trajectory. Printing input alone would hide the
		// effect of the system-prompt cache entirely.
		out = append(out, fmt.Sprintf("%-26s in=%s (fresh %s) out=%s reqs=%-4d %s",
			g.Name, thousands(g.Input), thousands(g.Fresh()), thousands(g.Output),
			g.Requests, dollars(g)))
	}
	return out
}

// dollars renders a group's cost, or an em dash when any part of it was billed
// at a rate we do not have. Printing $0.0000 for an unknown rate would be a
// claim, and the wrong one.
func dollars(g Group) string {
	if g.Unpriced {
		return "$—"
	}
	return fmt.Sprintf("$%.4f", g.USD)
}

// UsageSummary renders the token block a command prints under a suite of runs.
//
// The coverage line comes first and is printed even when it is complete,
// because the aggregate is a sum over whatever reported usage and a sum says
// nothing about what it left out. A suite where half the runs failed before
// reaching the model produces a perfectly plausible-looking total.
func UsageSummary(runs []Usage, cat *pricing.Catalog) []string {
	var total Usage
	measured := 0
	for _, u := range runs {
		if u.Empty() {
			continue
		}
		measured++
		total.Add(u)
	}
	if measured == 0 {
		return []string{fmt.Sprintf("tokens: not measured (0/%d runs reported usage)", len(runs))}
	}

	models := total.ByModel(cat)
	out := []string{fmt.Sprintf("tokens: %d/%d runs measured", measured, len(runs))}
	for _, line := range Lines(models) {
		out = append(out, "  "+line)
	}

	// Per run rather than per request: the question this exists to answer is
	// what one assessment costs, and a suite's runs are the unit that is
	// scheduled, repeated and paid for.
	t := total.Total()
	mean := Group{
		Name:     "per run (mean)",
		Tokens:   Tokens{Input: t.Input / int64(measured), Output: t.Output / int64(measured), Requests: t.Requests / measured},
		Unpriced: anyUnpriced(models),
	}
	for _, g := range models {
		mean.USD += g.USD / float64(measured)
	}
	out = append(out, fmt.Sprintf("  %-26s in=%s out=%s reqs=%-4d %s",
		mean.Name, thousands(mean.Input), thousands(mean.Output), mean.Requests, dollars(mean)))

	// The per-agent block comes second because it is the diagnostic rather than
	// the price, and it is printed for a single agent too: "the orchestrator
	// spent all of it" is the answer on a run that never delegated, and that is
	// worth seeing next to a delegation count of zero rather than inferring it.
	if agents := total.ByAgent(cat); len(agents) > 0 {
		out = append(out, "  by agent:")
		for _, line := range Lines(agents) {
			out = append(out, "    "+line)
		}
	}
	return out
}

func anyUnpriced(groups []Group) bool {
	for _, g := range groups {
		if g.Unpriced {
			return true
		}
	}
	return false
}

// thousands renders n with comma separators — token counts run to seven
// digits and are unreadable without them.
func thousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	return b.String()
}
