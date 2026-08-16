// Package approval is the human end of the write gate.
//
// Every mutation this agent can perform pauses on an ADK tool confirmation:
// the tool calls agent.Context.RequestConfirmation, ADK ends the turn with an
// adk_request_confirmation FunctionCall whose ID appears in
// Event.LongRunningToolIDs, and the run resumes only when a matching
// FunctionResponse carrying {"confirmed": bool} arrives. This package is the
// other side of that protocol: it reads the interrupts out of a turn, puts them
// to an Approver, and builds the resume message.
//
// It is deliberately free of any Kubernetes knowledge. What a reviewer reads is
// the hint the tool built — internal/kubewrite makes that the exact kubectl
// command line — so a new write tool needs no change here, and a Slack or web
// front end can be an Approver without learning anything about kubectl.
//
// # Fail closed
//
// Three defaults all point the same way, because the failure this package
// exists to prevent is an unattended write:
//
//   - A nil Approver denies. A caller that forgot to wire one up gets an agent
//     that cannot change the cluster, not one that changes it unsupervised.
//   - An Approver that returns an error denies, and the error is reported.
//   - An interrupt this package does not understand stops the run with an error
//     rather than being skipped, because skipping it would leave the agent
//     waiting on an answer that is never coming.
package approval

import (
	"encoding/json"

	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool/toolconfirmation"
	"google.golang.org/genai"
)

// Request is one write waiting on a human.
type Request struct {
	// ID is the confirmation call's ID. The resume must echo it exactly or the
	// runtime cannot correlate the answer with the paused call.
	ID string

	// Agent is the event author — which agent asked. In this system that is
	// always the write specialist, and a Request from anywhere else is worth
	// noticing.
	Agent string

	// Tool and Args are the call the model actually wants to make, unwrapped
	// from the confirmation envelope.
	Tool string
	Args map[string]any

	// Hint is what the reviewer should read. Tools that build their own (all of
	// internal/kubewrite's do) put the exact command here; ADK's built-in
	// RequireConfirmation flag produces boilerplate about the protocol instead,
	// so a caller rendering a prompt should be prepared for either.
	Hint string

	// Payload is the tool's structured version of the same thing, for a front
	// end that would rather render than print. Shape is the tool's business;
	// use PayloadAs to decode it.
	Payload any
}

// PayloadAs decodes a Request's payload into T.
//
// The round trip through JSON is not redundant: in-process the payload is
// whatever Go value the tool passed, but once a session has been persisted and
// reloaded it comes back as a map[string]any. Callers that type-asserted would
// work in tests and fail in production.
func PayloadAs[T any](r Request) (T, error) {
	var out T
	blob, err := json.Marshal(r.Payload)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(blob, &out)
}

// Decision is an answered Request.
type Decision struct {
	Request  Request
	Approved bool
	// Err is set when the Approver failed rather than decided. Approved is
	// false in that case — see the package comment on failing closed.
	Err error
}

// Pending returns the confirmation interrupts in a turn's events, in the order
// the agent raised them.
//
// The discriminator is membership in Event.LongRunningToolIDs, not the function
// name alone: on the wire an interrupt and an ordinary function call are the
// same shape. Partial events are skipped because streaming re-emits the same
// call for every chunk and only the aggregated event is settled — without the
// dedup a caller would prompt a human once per chunk and spend their answer on
// a phantom.
func Pending(events []*session.Event) []Request {
	var out []Request
	seen := map[string]bool{}
	for _, ev := range events {
		for _, fc := range interrupts(ev) {
			if fc.Name != toolconfirmation.FunctionCallName || seen[fc.ID] {
				continue
			}
			seen[fc.ID] = true
			r := Request{ID: fc.ID, Agent: ev.Author}
			r.Hint, r.Payload = confirmationOf(fc)
			if orig, err := toolconfirmation.OriginalCallFrom(fc); err == nil {
				r.Tool, r.Args = orig.Name, orig.Args
			}
			out = append(out, r)
		}
	}
	return out
}

// Unanswerable names the long-running interrupts in a turn that are not tool
// confirmations — a workflow input request, say.
//
// Nothing in this repo raises one today. It is reported rather than ignored
// because the cost of ignoring is silent: the agent is blocked on an answer,
// the caller sees a turn that merely ended, and the run looks finished when it
// is stuck. Better to fail with the name of the thing nobody knows how to
// answer.
func Unanswerable(events []*session.Event) []string {
	var out []string
	seen := map[string]bool{}
	for _, ev := range events {
		for _, fc := range interrupts(ev) {
			if fc.Name == toolconfirmation.FunctionCallName || seen[fc.ID] {
				continue
			}
			seen[fc.ID] = true
			out = append(out, fc.Name)
		}
	}
	return out
}

// Answer builds the message that resumes a paused run.
//
// Every decision goes into one message on purpose. Anthropic rejects a request
// in which an assistant message's tool_use blocks are not all answered by the
// message immediately after, so answering two interrupts in two messages breaks
// the next request rather than the second write — and it breaks it with an
// error that points at the history, not at the gate.
func Answer(decisions []Decision) *genai.Content {
	parts := make([]*genai.Part, 0, len(decisions))
	for _, d := range decisions {
		parts = append(parts, &genai.Part{FunctionResponse: &genai.FunctionResponse{
			ID:   d.Request.ID,
			Name: toolconfirmation.FunctionCallName,
			// The runtime unmarshals this whole map into a ToolConfirmation, so
			// "confirmed" is the field that decides and everything else is
			// ignored. Role user is required: the resume processor only looks
			// at the most recent user-authored event.
			Response: map[string]any{"confirmed": d.Approved},
		}})
	}
	return &genai.Content{Role: genai.RoleUser, Parts: parts}
}

// interrupts yields the function calls in an event that are waiting on a reply.
func interrupts(ev *session.Event) []*genai.FunctionCall {
	if ev == nil || len(ev.LongRunningToolIDs) == 0 || ev.Partial || ev.Content == nil {
		return nil
	}
	waiting := make(map[string]bool, len(ev.LongRunningToolIDs))
	for _, id := range ev.LongRunningToolIDs {
		waiting[id] = true
	}
	var out []*genai.FunctionCall
	for _, p := range ev.Content.Parts {
		if p.FunctionCall != nil && waiting[p.FunctionCall.ID] {
			out = append(out, p.FunctionCall)
		}
	}
	return out
}

// confirmationOf reads the hint and payload out of an interrupt.
//
// Both shapes are live: ADK attaches the *toolconfirmation.ToolConfirmation
// value in process, and a session that has been persisted and reloaded hands
// back the same thing as a map.
func confirmationOf(fc *genai.FunctionCall) (hint string, payload any) {
	switch tc := fc.Args["toolConfirmation"].(type) {
	case *toolconfirmation.ToolConfirmation:
		if tc != nil {
			return tc.Hint, tc.Payload
		}
	case toolconfirmation.ToolConfirmation:
		return tc.Hint, tc.Payload
	case map[string]any:
		h, _ := tc["hint"].(string)
		return h, tc["payload"]
	}
	return "", nil
}
