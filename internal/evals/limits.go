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
	"strings"

	"github.com/go-steer/mast/pkg/budget"
	"github.com/go-steer/mast/pkg/pricing"

	"github.com/go-steer/k8s-sre-agent/internal/llm"
)

// Limits builds one run's ceilings, priced through cat. A zero maxCostUSD or
// maxTurns is unlimited.
//
// maxTurns counts model calls, not agent turns: a Task specialist that loops
// five times before finish_task has spent five. It is the crude ceiling and
// the useful one, because a runaway agent is a loop long before it is an
// expensive single call.
func Limits(cat *pricing.Catalog, maxCostUSD float64, maxTurns int) budget.Limits {
	lim := budget.Limits{
		MaxCostUSD: maxCostUSD,
		MaxTurns:   maxTurns,
		Catalog:    cat,
	}
	// A model the catalog cannot price falls through to a flat per-1K rate,
	// and leaving that at zero would make an unrecognised model free — a
	// ceiling that silently stops metering, which is the one failure mode a
	// ceiling must not have. For enforcement the safe direction is to
	// overcharge, so the fallback is the main tier's average rate: both models
	// we run are in the catalog today (including the dated Vertex subagent ID,
	// via longest-prefix), so this only ever applies to a model that arrived
	// without anyone updating the table.
	if r, ok := cat.Lookup(llm.Main); ok && !r.IsZero() {
		lim.RatePer1K = (r.InputPerMTok + r.OutputPerMTok) / 2 / 1000
	}
	return lim
}

// ExceededBudget reports whether a recorded error was a ceiling firing rather
// than the run breaking. The transcript keeps errors as strings, so this
// matches on budget.ErrExceeded's text rather than unwrapping.
//
// It exists so a command can say so out loud. A truncated run reads exactly
// like an agent that found less than it should have — same missing findings,
// same low scores — and the whole "no silent caps" rule in this harness is that
// a bound the operator chose has to be visible in the output it produced.
func ExceededBudget(msg string) bool {
	return strings.Contains(msg, budget.ErrExceeded.Error())
}
