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

package approval

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"strings"
)

// An Approver decides whether one write may run.
//
// The contract is narrow on purpose: it answers exactly one question, yes or
// no, about one already-formed command. It cannot amend the change, ask the
// agent for clarification, or approve "the rest of them" — an approval channel
// that can do more than accept and decline is one an agent can negotiate with.
//
// Returning an error means the decision could not be taken (nobody answered,
// the channel broke). It is not a way to say no: the write is denied either
// way, and the difference is only in what gets reported.
type Approver interface {
	Approve(ctx context.Context, r Request) (bool, error)
}

// Func adapts a function to Approver.
type Func func(ctx context.Context, r Request) (bool, error)

// Approve implements Approver.
func (f Func) Approve(ctx context.Context, r Request) (bool, error) { return f(ctx, r) }

// DenyAll declines every write. This is the default when no Approver is set,
// and it is a legitimate production configuration: an agent that diagnoses and
// proposes, with the write path present but closed.
func DenyAll() Approver {
	return Func(func(context.Context, Request) (bool, error) { return false, nil })
}

// ApproveAllUnattended approves every write with no human involved.
//
// Named at length because it removes the only thing standing between a model
// and a cluster. It exists for tests and for the live eval tier, which runs
// against a kind cluster that internal/kindcluster created and will delete.
// Never wire it to a cluster anybody cares about.
func ApproveAllUnattended() Approver {
	return Func(func(context.Context, Request) (bool, error) { return true, nil })
}

// Prompt reads decisions from a terminal.
//
// It prints the hint — for a kubewrite tool that is the exact kubectl command
// line — and takes one line back. Only an explicit yes approves; a blank line,
// an unrecognised answer and EOF all decline, so a reviewer who walks away or a
// process with no stdin cannot accidentally consent.
func Prompt(in io.Reader, out io.Writer) Approver {
	lines := bufio.NewScanner(in)
	return Func(func(_ context.Context, r Request) (bool, error) {
		hint := r.Hint
		if hint == "" {
			// A tool that did not write its own hint. Show what there is rather
			// than an empty prompt: approving a call you cannot see is worse
			// than reading raw arguments.
			hint = fmt.Sprintf("Approve %s(%v)?", r.Tool, r.Args)
		}
		fmt.Fprintf(out, "\n%s\n\n[%s] approve? (yes/no) ", hint, r.Agent)
		if !lines.Scan() {
			fmt.Fprintln(out, "no answer — declined")
			return false, lines.Err()
		}
		switch strings.TrimSpace(strings.ToLower(lines.Text())) {
		case "y", "yes":
			return true, nil
		default:
			return false, nil
		}
	})
}
