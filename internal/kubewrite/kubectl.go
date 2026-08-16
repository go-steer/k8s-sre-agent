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
	"context"
	"fmt"

	"github.com/go-steer/k8s-sre-agent/internal/kubectl"
)

// Runner executes one kubectl invocation and returns its combined output.
//
// An interface rather than a concrete type so tests can drive every tool
// without a cluster. That is not only convenience: a package whose entire
// purpose is to mutate clusters must be testable in a way that cannot mutate
// one, or its tests will not be run.
//
// Declared here rather than imported because this is the consumer's half of
// internal/kubectl's Client: the write path wants the two streams interleaved,
// and the read path does not. *kubectl.Client satisfies both.
type Runner interface {
	// Run executes kubectl with args, piping stdin when it is non-empty. It
	// returns the combined output whether or not the command succeeded, because
	// kubectl's diagnosis of a failure is on stderr and is the useful half.
	Run(ctx context.Context, stdin string, args ...string) (string, error)

	// Cluster names the context every invocation is pinned to. Used to render
	// the command line a human approves.
	Cluster() string
}

// newKubectl builds the real runner. The pin — an explicit kubeconfig, an
// explicit context, and a check that the two agree — lives in internal/kubectl
// because the read path holds itself to exactly the same rule.
func newKubectl(cfg Config) (Runner, error) {
	c, err := kubectl.New(kubectl.Config{
		Kubeconfig: cfg.Kubeconfig,
		Context:    cfg.Context,
		Binary:     cfg.Binary,
		Timeout:    cfg.Timeout,
	})
	if err != nil {
		return nil, fmt.Errorf("kubewrite: %w", err)
	}
	return c, nil
}
