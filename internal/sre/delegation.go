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

package sre

import (
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// serializeDelegations drops ordinary tool calls that the model batched into
// the same turn as a delegation.
//
// # The defect this works around
//
// ADK v2.2.0's runChat (agent/llmagent/llm_agent_wrapper.go) handles a
// delegation by dispatching it and then breaking out of the agent's event
// iterator, so the outer loop can re-enter with the specialist's answer in
// history:
//
//	taskFCs := extractTaskDelegationFCs(ev, toolsDict)
//	for _, fc := range taskFCs { if !dispatchAndYield(fc) { return } }
//	if len(taskFCs) > 0 { hadTaskFC = true; break }
//
// Every delegation in the event is dispatched, so a multi-specialist fan-out
// is fine. An ordinary tool call sharing that event is not: the break
// abandons the iterator before the base flow executes it, so the call is never
// run and never answered. The assistant message keeps a `tool_use` block with
// no `tool_result` after it, and Anthropic rejects the *next* request outright:
//
//	messages.4: `tool_use` ids were found without `tool_result` blocks
//	immediately after: toolu_...
//
// It killed 16 of 31 tier-1 examples the first time delegation actually
// worked. The failure is downstream of and unrelated to whatever the agent was
// reasoning about, which is why it reads as a provider error rather than as
// the framework bug it is.
//
// # Why this shape of workaround
//
// Dropping the batched call before the response becomes an event means the
// dangling `tool_use` never enters history. Nothing is lost: the model simply
// did not make that call this round, and it is free to make it on the next one
// — which it does, since the reason it wanted the data has not changed. The
// cost is a round trip.
//
// The alternative was to instruct the orchestrator to delegate alone. That is
// the kind of invariant a prompt cannot hold: the model batches calls because
// batching is usually right, and one lapse costs the entire run rather than
// one call. TestParallelDelegationKeepsCallsAndResponsesPaired pins the
// behaviour, and will start failing — correctly — if ADK fixes runChat and
// this becomes unnecessary.
func serializeDelegations(specialists []string) llmagent.AfterModelCallback {
	isSpecialist := make(map[string]bool, len(specialists))
	for _, n := range specialists {
		isSpecialist[n] = true
	}

	return func(_ adkagent.Context, resp *adkmodel.LLMResponse, err error) (*adkmodel.LLMResponse, error) {
		// Partials are aggregated into a final response that this callback
		// sees separately; editing a fragment would corrupt the aggregation.
		if err != nil || resp == nil || resp.Partial || resp.Content == nil {
			return nil, nil
		}

		var delegations, others int
		for _, p := range resp.Content.Parts {
			if p == nil || p.FunctionCall == nil {
				continue
			}
			if isSpecialist[p.FunctionCall.Name] {
				delegations++
			} else {
				others++
			}
		}
		if delegations == 0 || others == 0 {
			return nil, nil
		}

		kept := make([]*genai.Part, 0, len(resp.Content.Parts))
		for _, p := range resp.Content.Parts {
			// Only function calls are dropped. Text and thinking parts stay:
			// Anthropic requires thinking blocks to be replayed with their
			// signatures intact, so removing one breaks the next request in a
			// different way.
			if p != nil && p.FunctionCall != nil && !isSpecialist[p.FunctionCall.Name] {
				continue
			}
			kept = append(kept, p)
		}

		// Copy rather than mutate: the caller owns the response, and the
		// content may be shared with an event already yielded.
		out := *resp
		content := *resp.Content
		content.Parts = kept
		out.Content = &content
		return &out, nil
	}
}
