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

package kubewrite

import (
	"fmt"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
)

// Outcome values for Result.Status.
const (
	// StatusApplied — every command ran and succeeded.
	StatusApplied = "applied"
	// StatusPartial — some commands succeeded and some failed. Only a bulk
	// operation can reach this, and it is the reason bulk operations report
	// per-command rather than in aggregate.
	StatusPartial = "partial"
	// StatusFailed — the commands ran and the cluster rejected all of them.
	StatusFailed = "failed"
	// StatusRejected — a human declined. Not an error and not retryable.
	StatusRejected = "rejected"
	// StatusRefused — this package declined before asking anyone.
	StatusRefused = "refused"
)

// Result is what every write tool returns.
//
// Commands is included on success as well as failure because the
// change-executor's reporting protocol asks it to say what it did, and the
// difference between "scaled the deployment" and the command that scaled it is
// the difference between a claim and a record. It is also the only way the
// orchestrator's report can name the change precisely enough for someone to
// undo it.
type Result struct {
	Status   string   `json:"status" jsonschema:"one of applied, partial, failed, rejected (a human declined), refused (this tool declined before asking)"`
	Commands []string `json:"commands,omitempty" jsonschema:"the kubectl commands this call covered, in order"`
	Output   string   `json:"output,omitempty" jsonschema:"combined kubectl output, one block per command"`
	Error    string   `json:"error,omitempty" jsonschema:"why the change did not happen, when it did not"`
}

// step is one kubectl invocation. A tool call may plan several — a bulk delete
// is N invocations under one approval — and the human approves or rejects the
// whole plan, never part of it.
type step struct {
	args  []string
	stdin string
}

// executor turns a plan into an approval prompt and, if approved, into
// kubectl invocations.
type executor struct {
	run     Runner
	cluster string
	guard   guard
}

// maxOutputPerStep and maxOutput bound what comes back into the model's
// context. kubectl is terse on success and can be verbose on failure (a
// rejected apply echoes the object), and a bulk operation multiplies it.
const (
	maxOutputPerStep = 4000
	maxOutput        = 16000
)

// apply is the single path from an intent to a mutated cluster.
//
// Every tool in this package routes through it, which is what makes "all
// writes are gated" a property of the type rather than a list someone has to
// maintain. There is deliberately no unexported variant that skips the gate:
// a bypass that exists for one caller is a bypass.
//
// # The confirmation protocol
//
// This reimplements what functiontool's RequireConfirmation flag does, and the
// reason is the hint. ADK's built-in hint is fixed text about the protocol —
// "Please approve or reject the tool call k8s_scale_deployment() by responding
// with a FunctionResponse..." — and says nothing about what will happen to the
// cluster. The interrupt event does carry the original FunctionCall, so a UI
// can render the arguments, but arguments are not the action: for a bulk
// delete they are a JSON array that a reviewer would have to mentally compile
// into twelve kubectl commands.
//
// So the hint is the commands. What the human reads is what runs, character
// for character. The mechanism is otherwise identical to functiontool's and to
// tool.confirmationTool's — RequestConfirmation records the pending
// confirmation in EventActions and ADK synthesises the adk_request_confirmation
// event from that (internal/llminternal/functions.go), so nothing here depends
// on the returned error; returning tool.ErrConfirmationRequired matches what
// ADK's own tools return and is what retryandreflect looks for when it decides
// not to retry.
func (e *executor) apply(ctx adkagent.Context, toolName, effect string, steps []step) (Result, error) {
	commands := make([]string, 0, len(steps))
	for _, s := range steps {
		commands = append(commands, e.render(s))
	}

	// A rejection never reaches here: functiontool.Run short-circuits it before
	// the handler, which is why gatedTool exists. So the only two states are
	// "nobody has been asked yet" and "a human approved".
	if ctx.ToolConfirmation() == nil {
		if err := ctx.RequestConfirmation(e.hint(effect, steps, commands), plan{
			Cluster:  e.cluster,
			Effect:   effect,
			Commands: commands,
		}); err != nil {
			return Result{}, err
		}
		// RequestConfirmation already sets Actions().SkipSummarization; ADK's
		// own tools set it a second time and it is not needed.
		return Result{}, fmt.Errorf("write tool %q %w", toolName, tool.ErrConfirmationRequired)
	}

	var (
		out    strings.Builder
		failed []string
	)
	for i, s := range steps {
		body, err := e.run.Run(ctx, s.stdin, s.args...)
		fmt.Fprintf(&out, "$ %s\n%s\n", commands[i], truncate(body, maxOutputPerStep))
		if err != nil {
			failed = append(failed, commands[i])
		}
	}

	res := Result{Commands: commands, Output: truncate(out.String(), maxOutput)}
	switch {
	case len(failed) == 0:
		res.Status = StatusApplied
	case len(failed) == len(steps):
		res.Status = StatusFailed
		res.Error = "the cluster rejected the change; the output says why"
	default:
		res.Status = StatusPartial
		res.Error = fmt.Sprintf("%d of %d commands failed: %s. The rest were applied — "+
			"do not re-run the whole batch.", len(failed), len(steps), strings.Join(failed, "; "))
	}
	return res, nil
}

// plan is the structured payload attached to the confirmation, for a UI that
// wants to render the change rather than print the hint.
type plan struct {
	Cluster  string   `json:"cluster"`
	Effect   string   `json:"effect"`
	Commands []string `json:"commands"`
}

// hint is what a human reads before approving.
//
// Written for someone glancing at a Slack message at 3am, so the order is:
// which cluster, what it does in one line, then the exact commands. The
// cluster comes first because approving the right change against the wrong
// cluster is the mistake this whole package is arranged to prevent.
func (e *executor) hint(effect string, steps []step, commands []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Approve this change to cluster %q?\n\n%s\n\n", e.cluster, effect)
	for _, c := range commands {
		fmt.Fprintf(&b, "    %s\n", c)
	}
	for _, s := range steps {
		if s.stdin == "" {
			continue
		}
		fmt.Fprintf(&b, "\nwith this piped to stdin:\n\n%s\n", indent(truncate(s.stdin, maxOutputPerStep), "    "))
	}
	b.WriteString("\nApproving runs exactly the command(s) above. Rejecting runs nothing.")
	return b.String()
}

// render produces the command line, quoted so a reviewer can paste it.
func (e *executor) render(s step) string {
	parts := make([]string, 0, len(s.args)+3)
	parts = append(parts, "kubectl", "--context", shellQuote(e.cluster))
	for _, a := range s.args {
		parts = append(parts, shellQuote(a))
	}
	return strings.Join(parts, " ")
}

// shellQuote is for display only — nothing here is executed through a shell,
// which is why an argument can safely contain a JSON patch in the first place.
// It exists so the rendered line is one a human can copy and re-run.
func shellQuote(s string) string {
	if s != "" && !strings.ContainsAny(s, " \t\n\"'\\$`|&;<>()*?[]{}#~!") {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func indent(s, prefix string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + fmt.Sprintf("\n… (%d bytes truncated)", len(s)-n)
}
