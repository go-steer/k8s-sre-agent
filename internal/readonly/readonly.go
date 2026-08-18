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

// Package readonly holds the guards that make an entry point read-only against
// a cluster somebody else owns.
//
// They live here rather than in one command because two now need them —
// cmd/sre-agent for a one-shot assessment and cmd/sre-monitor for the
// scheduler — and safety code that has to be remembered and re-pasted is safety
// code that eventually is not. That is the same argument the write gate makes
// about upstream's CHANGE_EXECUTOR_INTERRUPT_ON list, which has already
// drifted from the tool list it is meant to shadow.
//
// None of this makes the *cluster* safe: tier 3 runs against simian-test with
// admin credentials, because that is what the cluster has. It makes our binary
// unable to use them, which is the part we control. Four layers, each covering
// a different way the pin could fail:
//
//   - No write tools are built. Neither command passes sre.Config.Writes, so
//     change-executor is not in the roster at all.
//   - RefuseWriteTools asserts that at startup rather than trusting it.
//   - LookoutWriters asks the binary which of its tools declare a write, since
//     ADK's mcptoolset throws the annotation away; an unclassified write is
//     fatal and a classified store write is withheld.
//   - VerifySoleContext requires the kubeconfig to describe exactly one
//     context, which is what makes the pin hold for a subprocess that has no
//     per-call context flag.
package readonly

import (
	"context"
	"fmt"
	"log"
	"os"
	"slices"
	"sort"
	"strings"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/genai"

	"github.com/go-steer/k8s-sre-agent/internal/kubectl"
	"github.com/go-steer/k8s-sre-agent/internal/kubewrite"
	"github.com/go-steer/k8s-sre-agent/internal/lookout"
)

// readonlyCtx is the minimal agent.ReadonlyContext a Toolset needs to
// enumerate its tools before any agent or session exists.
type readonlyCtx struct{ context.Context }

func (readonlyCtx) UserContent() *genai.Content          { return nil }
func (readonlyCtx) InvocationID() string                 { return "readonly" }
func (readonlyCtx) AgentName() string                    { return "readonly" }
func (readonlyCtx) ReadonlyState() session.ReadonlyState { return nil }
func (readonlyCtx) UserID() string                       { return "readonly" }
func (readonlyCtx) AppName() string                      { return "readonly" }
func (readonlyCtx) SessionID() string                    { return "readonly" }
func (readonlyCtx) Branch() string                       { return "" }

var _ adkagent.ReadonlyContext = readonlyCtx{}

// credentialNames is the environment a client-go exec credential plugin needs,
// as an explicit allowlist.
//
// internal/lookout starts its subprocess from an empty environment on purpose:
// lookout resolves its cluster from KUBECONFIG's current-context with no
// per-call flag, so an inherited environment would let it read whichever of
// this machine's 64 contexts happens to be current. That is exactly right for a
// kind cluster, whose kubeconfig carries an embedded client certificate and
// needs nothing else.
//
// A managed cluster does not work that way. A GKE kubeconfig authenticates
// through `exec: gke-gcloud-auth-plugin`, which has to be found on PATH and
// reads its credentials out of HOME. With an empty environment every lookout
// call against simian-test failed with "executable gke-gcloud-auth-plugin not
// found", the eight specialists spent the run reasoning about a cluster none of
// them could see, and five of them stalled.
//
// So the pin has to be re-argued rather than assumed. It now rests on two
// things instead of three: KUBECONFIG names the file, and the file describes
// exactly one context (VerifySoleContext). What HOME adds is that
// ~/.kube/config becomes *reachable* — it is not read, because KUBECONFIG takes
// precedence in client-go's loading rules, but "the child cannot see another
// kubeconfig" is no longer literally true. That is why the single-context check
// is mandatory here and merely prudent in kindcluster.
//
// KUBECONFIG itself is never forwarded, no matter what is in our environment:
// lookout.Toolset sets it, and a forwarded copy would be the one variable that
// could actually repoint the child. FilterCredentialEnv drops it.
var CredentialNames = []string{
	"PATH", "HOME",
	// gcloud/GKE. CLOUDSDK_CONFIG relocates the gcloud config directory;
	// GOOGLE_APPLICATION_CREDENTIALS names a service-account key, which is how
	// this runs unattended.
	"CLOUDSDK_CONFIG", "CLOUDSDK_CORE_PROJECT", "GOOGLE_APPLICATION_CREDENTIALS",
	"USE_GKE_GCLOUD_AUTH_PLUGIN",
}

func CredentialEnv() []string { return FilterCredentialEnv(os.Environ()) }

func FilterCredentialEnv(environ []string) []string {
	want := map[string]bool{}
	for _, n := range CredentialNames {
		want[n] = true
	}
	var out []string
	for _, kv := range environ {
		name, _, ok := strings.Cut(kv, "=")
		if ok && want[name] {
			out = append(out, kv)
		}
	}
	return out
}

// LookoutConfig is the pinned lookout configuration both read-only entry
// points use: the kubeconfig and context the operator named, plus the
// credential allowlist and nothing else.
func LookoutConfig(kubeconfig, kubecontext string) lookout.Config {
	return lookout.Config{
		Kubeconfig: kubeconfig,
		Context:    kubecontext,
		Env:        CredentialEnv(),
	}
}

// lookoutWriters asks the lookout binary which of its tools declare a write,
// refuses the run on one this repo has not classified, and returns the rest so
// the model is never offered them.
//
// This exists because the assumption the name-match guard below rests on
// stopped holding without anything failing. lookout was a read-only binary
// when that guard was written; it now ships k8s_findings_diff (which advances
// persisted finding state as a side effect of reporting it), k8s_findings_ack
// and k8s_triage_status, all three advertising ReadOnlyHint:false. None of
// them mutates a Kubernetes object — they write the sentinel's SQLite store,
// named by their own `store` argument — so the cluster guarantee never
// actually broke. What broke is that nothing here could tell.
//
// Two different responses, because there are two different hazards:
//
//   - A write nobody has classified is fatal. Fail-closed is right when the
//     unknown is "what does this tool change": the refusal names the tool and
//     costs a human one reading of its description.
//   - A classified store write is withheld rather than refused. Withholding
//     costs this command nothing — every one of the three needs a sentinel
//     store that a one-shot assessment does not have, so all three would fail
//     anyway — and it removes the one thing they could otherwise do here,
//     which is create or overwrite a SQLite file at any path the model names.
//     "Read-only" should mean the model is not handed a write, not that its
//     writes happen to be useless.
//
// Withholding is logged rather than silent, on the harness's own no-silent-caps
// rule: a tool surface quietly one smaller is indistinguishable from a lookout
// release that dropped a check.
func LookoutWriters(ctx context.Context, kubeconfig, kubecontext string) ([]string, error) {
	surface, err := lookout.Surface(ctx, LookoutConfig(kubeconfig, kubecontext))
	if err != nil {
		return nil, fmt.Errorf("lookout tool surface: %w", err)
	}
	if unknown := lookout.UnclassifiedWriters(surface); len(unknown) > 0 {
		return nil, fmt.Errorf("refusing to run: lookout declares write tool(s) this repo has not "+
			"classified: %s. A read-only assessment must not offer a tool whose effect nobody has "+
			"looked at. Read the tool's description, and if the write lands in lookout's own store "+
			"rather than on the cluster, add it to storeWriters in internal/lookout",
			strings.Join(unknown, ", "))
	}

	var withheld []string
	for _, t := range surface {
		if !t.ReadOnly {
			withheld = append(withheld, t.Name)
		}
	}
	sort.Strings(withheld)
	if len(withheld) > 0 {
		log.Printf("withholding %d lookout tool(s) that declare a write: %s",
			len(withheld), strings.Join(withheld, ", "))
	}
	return withheld, nil
}

// withoutTools hides named tools from the model. The toolset keeps its name,
// which the specialist specs allowlist by.
func WithoutTools(ts tool.Toolset, names []string) tool.Toolset {
	if len(names) == 0 {
		return ts
	}
	hide := make(map[string]bool, len(names))
	for _, n := range names {
		hide[n] = true
	}
	return tool.FilterToolset(ts, func(_ adkagent.ReadonlyContext, t tool.Tool) bool {
		return !hide[t.Name()]
	})
}

// refuseWriteTools fails the run if any toolset declares a tool that mutates
// the cluster.
//
// Nothing here builds kubewrite.Tools, so this can only fire if a toolset grows
// a write surface underneath us — which is exactly the assumption worth
// checking at startup rather than trusting: the cost is one enumeration and the
// failure it guards against is a mutation to a cluster we do not own. lookout
// has since grown writes of its own; they go to its store rather than to the
// cluster and LookoutWriters handles them, so this stays an exact match against
// kubewrite.ToolNames() rather than a heuristic.
func RefuseWriteTools(ctx context.Context, toolsets []tool.Toolset) error {
	writes := kubewrite.ToolNames()
	for _, ts := range toolsets {
		tools, err := ts.Tools(readonlyCtx{ctx})
		if err != nil {
			return fmt.Errorf("enumerate tools: %w", err)
		}
		for _, t := range tools {
			if slices.Contains(writes, t.Name()) {
				return fmt.Errorf("refusing to run: toolset declares the mutating tool %q, "+
					"and this command is read-only by construction", t.Name())
			}
		}
	}
	return nil
}

// verifySoleContext requires the kubeconfig to name want as current and to
// describe exactly one context.
//
// The first half is internal/kubectl's pin. The second is kindcluster's
// isolation check applied to a cluster we did not create, and it is the half
// that matters for the lookout subprocess, which has no per-call context flag
// and resolves whatever is current in the file it is handed. This machine has
// dozens of contexts including production clusters; a minified kubeconfig is
// the difference between a pin that holds and a pin that holds until someone
// drops a flag.
func VerifySoleContext(path, want string) error {
	if err := kubectl.VerifyContext(path, want); err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read kubeconfig %s: %w", path, err)
	}
	names := ContextNames(string(raw))
	if len(names) != 1 || names[0] != want {
		return fmt.Errorf("kubeconfig %s describes %d contexts (%s); this command requires a file "+
			"describing exactly one, so that the lookout subprocess cannot reach another cluster. "+
			"Produce one with: kubectl config view --minify --flatten --context=%s > FILE",
			path, len(names), strings.Join(names, ","), want)
	}
	return nil
}

// contextNames pulls the `- name:` entries out of the top-level contexts list.
// Line-oriented on purpose, the same way kubectl.VerifyContext is: pulling in a
// YAML parser to read two fields out of a file we are about to hand to kubectl
// verbatim buys nothing, and kubectl is the thing that has to agree with us.
func ContextNames(raw string) []string {
	var names []string
	inContexts := false
	for _, line := range strings.Split(raw, "\n") {
		// A top-level key opens or closes the block. List items sit at column
		// zero in kubectl's own output, so a leading "-" is inside the block,
		// not a new key.
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			inContexts = strings.HasPrefix(line, "contexts:")
			continue
		}
		if !inContexts {
			continue
		}
		// The only `name:` inside a context entry is the context's own — the
		// nested keys under `context:` are cluster/user/namespace. So this
		// cannot pick up a cluster name and count it as a second context.
		item := strings.TrimPrefix(strings.TrimSpace(line), "- ")
		if rest, ok := strings.CutPrefix(item, "name:"); ok {
			names = append(names, strings.Trim(strings.TrimSpace(rest), `"'`))
		}
	}
	return names
}
