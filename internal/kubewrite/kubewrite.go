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

// Package kubewrite is the write half of the Kubernetes data plane.
//
// internal/lookout is the read half: a k8s-lookout subprocess speaking MCP,
// which returns diagnostics and mutates nothing. lookout has no write surface
// and is not going to grow one, so every mutation this agent can perform is
// defined here.
//
// # Why kubectl and not client-go
//
// Every tool in this package renders a `kubectl` command line and executes it
// as a subprocess. client-go would be the obvious alternative and is the wrong
// choice here for three reasons, in ascending order of how much they matter.
//
// It is a large dependency for a small job. This repo already declined to pull
// client-go in to read one scalar out of a kubeconfig (see
// kubectl.VerifyContext), and the write path is a few dozen verbs.
//
// Writes are rare and human-gated, so a subprocess costs nothing that matters.
// The latency budget of an operation that waits on a person is not measured in
// milliseconds.
//
// And — the real reason — **a kubectl command line is reviewable and an API
// call is not**. Every write here pauses for a human, and what that human is
// shown is the literal command that will run:
//
//	kubectl --context kind-sre-eval-a1 scale deployment/checkout-api -n prod --replicas=5
//
// A reviewer can read that, recognise it, and reject it. The equivalent
// client-go approval prompt would have to be a rendering of a patch object
// that no one can check against what the library will actually send. The gate
// is only as good as what it shows, so the executable form and the reviewable
// form should be the same string.
//
// # The pin
//
// Config requires an explicit Kubeconfig and Context and refuses to resolve
// the ambient current-context, exactly as lookout.Config and kuberead.Config
// do, and for the same reason: this machine has 64 kube contexts and several
// are live. The check itself is internal/kubectl's, shared by all three, which
// is what makes "the cluster we read" and "the cluster we write" the same by
// construction.
//
// # Everything here is gated
//
// There is no ungated write. The confirmation is applied by the executor that
// every tool routes through, not by a per-tool flag, and not by a list of tool
// names kept somewhere else. Upstream keeps such a list
// (CHANGE_EXECUTOR_INTERRUPT_ON) and it is already missing two of its own
// mutating tools — kubectl_patch_configmap and kubectl_apply_custom_resource
// are defined and not listed. A gate you have to remember to extend is a gate
// that eventually is not extended.
package kubewrite

import (
	"fmt"
	"time"

	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/k8s-sre-agent/internal/kubectl"
)

// ToolsetName is how a specialist spec refers to this toolset. It is not an
// MCP server and cannot be granted through the spec allowlist — see
// sre.WriteAgentName for why the binding is in Go — but the name is still what
// appears in logs and in the roster test.
const ToolsetName = "kubewrite"

// DefaultBinary is the executable resolved on PATH when Config.Binary is empty.
const DefaultBinary = kubectl.DefaultBinary

// DefaultTimeout bounds one kubectl invocation. Writes are fast; a scale that
// has not returned in a minute is a control plane that is not answering, and
// waiting longer only delays the report.
const DefaultTimeout = kubectl.DefaultTimeout

// DefaultProtectedNamespaces are refused outright rather than left to prompt
// discipline, ported from upstream's PROTECTED_NAMESPACES.
//
// Note what the refusal is protecting against. It is not protecting against a
// malicious agent — a human approves every write, and a human who approves
// `kubectl delete deployment coredns -n kube-system` has made a decision this
// package cannot override. It protects against the case where the agent is
// right about the diagnosis, the operator is skimming, and the object happens
// to be one whose deletion takes out the control plane or the agent itself.
// Those namespaces are never the answer to an incident, so refusing them costs
// nothing and removes them from the set of things a tired reviewer can wave
// through.
var DefaultProtectedNamespaces = []string{
	"kube-system",
	"kube-public",
	"kube-node-lease",
	"sre-agent",
}

// DefaultBulkMax caps how many objects one approval may destroy, ported from
// upstream's BULK_DELETE_MAX. A single approval should not delete more than a
// person can actually read.
const DefaultBulkMax = 25

// MaxReplicas bounds every scale.
//
// This is not in upstream and is not about the cluster's capacity — it is
// about the failure mode of approval itself. A reviewer reads the *shape* of a
// command and recognises `kubectl scale deployment/web -n prod --replicas=N`
// as the thing they asked for; the digits are the part attention skips. No
// legitimate remediation crosses this bound, so putting it here turns a
// misplaced keystroke from an outage into a refusal, at the cost of a tool
// error in the one case where someone genuinely wants more.
const MaxReplicas = 500

// Config binds the write path to exactly one cluster.
type Config struct {
	// Kubeconfig is the credential file kubectl may use. Required: there is no
	// fallback to ~/.kube/config.
	Kubeconfig string

	// Context names the kube context every invocation passes as --context. It
	// must also be the Kubeconfig's current-context. Required.
	Context string

	// Binary is the kubectl executable. Empty resolves DefaultBinary on PATH.
	Binary string

	// Timeout bounds one invocation. Zero uses DefaultTimeout.
	Timeout time.Duration

	// ProtectedNamespaces are refused before a human is asked. Nil uses
	// DefaultProtectedNamespaces; an explicitly empty non-nil slice disables
	// the check, which is the only way to turn it off and is deliberately
	// awkward to express by accident.
	ProtectedNamespaces []string

	// BulkMax caps a bulk operation's target count. Zero uses DefaultBulkMax.
	BulkMax int

	// Runner substitutes the kubectl subprocess. Nil builds the real one.
	// Tests set this; nothing in production should.
	Runner Runner
}

// Tools builds the write toolset.
//
// The returned tools are ordinary ADK tools and could be attached to any
// agent. They are not, and the reason is structural rather than conventional:
// see sre.WriteAgentName.
func Tools(cfg Config) ([]tool.Tool, error) {
	run := cfg.Runner
	if run == nil {
		var err error
		run, err = newKubectl(cfg)
		if err != nil {
			return nil, err
		}
	}

	protected := cfg.ProtectedNamespaces
	if protected == nil {
		protected = DefaultProtectedNamespaces
	}
	bulkMax := cfg.BulkMax
	if bulkMax == 0 {
		bulkMax = DefaultBulkMax
	}

	e := &executor{
		run:     run,
		cluster: cfg.Context,
		guard:   guard{protected: protected, bulkMax: bulkMax},
	}
	tools, err := e.tools()
	if err != nil {
		return nil, fmt.Errorf("kubewrite: build tools: %w", err)
	}
	return tools, nil
}

// ToolNames lists the write tools in declaration order.
//
// Exported so the roster can be asserted without building a cluster
// connection. A test that pins this list is what makes adding a write tool a
// deliberate act: a new mutation cannot appear in the agent's hands without
// someone editing the expected roster in the same change.
func ToolNames() []string {
	names := make([]string, 0, len(toolSpecs))
	for _, s := range toolSpecs {
		names = append(names, s.name)
	}
	return names
}
