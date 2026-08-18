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

// Package kubectl is the pinned kubectl subprocess the data plane shares.
//
// Three packages needed the same two things — a kubectl invocation that cannot
// reach a cluster nobody named, and the kubeconfig check that makes that true.
// internal/lookout and internal/kubewrite each carried a copy, and
// kubewrite's said "two copies is the limit; if a third appears, extract it".
// internal/kuberead is the third.
//
// The interface a caller wants differs by half — the write path wants kubectl's
// stderr folded into the output, because kubectl's diagnosis of a failed change
// is the useful part, while the read path wants stdout clean so it can parse
// JSON out of it. So this package exports one concrete Client with both
// methods, and each caller declares the narrow interface it actually uses.
package kubectl

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// DefaultBinary is the executable resolved on PATH when Config.Binary is empty.
const DefaultBinary = "kubectl"

// DefaultTimeout bounds one invocation.
const DefaultTimeout = time.Minute

// Config binds a Client to exactly one cluster.
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
}

// Client runs kubectl against one pinned context.
type Client struct {
	binary     string
	kubeconfig string
	context    string
	timeout    time.Duration
}

// New builds a Client, refusing to guess at either half of the pin.
func New(cfg Config) (*Client, error) {
	if cfg.Context == "" {
		return nil, fmt.Errorf("kubectl: Context is required — refusing to resolve the ambient current-context")
	}
	if cfg.Kubeconfig == "" {
		return nil, fmt.Errorf("kubectl: Kubeconfig is required — refusing to fall back to ~/.kube/config")
	}
	if err := VerifyContext(cfg.Kubeconfig, cfg.Context); err != nil {
		return nil, err
	}
	bin := cfg.Binary
	if bin == "" {
		bin = DefaultBinary
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("kubectl: locate %q: %w", bin, err)
	}
	timeout := cfg.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}
	return &Client{binary: resolved, kubeconfig: cfg.Kubeconfig, context: cfg.Context, timeout: timeout}, nil
}

// Cluster names the context every invocation is pinned to.
func (c *Client) Cluster() string { return c.context }

// Run executes kubectl and returns stdout and stderr interleaved, whether or
// not the command succeeded. This is the write path's shape: a failed change's
// explanation is on stderr and is the half worth reporting.
func (c *Client) Run(ctx context.Context, stdin string, args ...string) (string, error) {
	var out bytes.Buffer
	err := c.exec(ctx, stdin, &out, &out, args)
	return strings.TrimRight(out.String(), "\n"), err
}

// Output executes kubectl and keeps the two streams apart. The read path needs
// this: kubectl can print a partial result on stdout and a per-resource
// permission error on stderr in the same run, and a parser handed the
// concatenation of the two sees neither.
func (c *Client) Output(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	var o, e bytes.Buffer
	err = c.exec(ctx, "", &o, &e, args)
	return o.String(), strings.TrimRight(e.String(), "\n"), err
}

// exec is the one place a kubectl child is spawned.
//
// The same shape as kindcluster's Kubectl, deliberately not shared with it:
// that package exists to create and destroy throwaway clusters, and a
// dependency from the production data plane onto the fault-injection harness
// would be a strange edge to have to explain. What both encode is that an
// inherited KUBECONFIG puts this machine's 64 contexts back within reach of a
// dropped flag, so the child's environment is constructed, not inherited, and
// --context is passed on every call rather than trusted to the file.
func (c *Client) exec(ctx context.Context, stdin string, stdout, stderr *bytes.Buffer, args []string) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	// #nosec G204 -- variable argv is what this type is for. There is no shell:
	// exec passes argv directly, so the arguments cannot be reinterpreted. The
	// binary is operator-supplied config, c.context was checked against the
	// kubeconfig by VerifyContext, and readonly.Guard is what bounds the verbs.
	cmd := exec.CommandContext(ctx, c.binary, append([]string{"--context", c.context}, args...)...)
	cmd.Env = []string{
		"KUBECONFIG=" + c.kubeconfig,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	return cmd.Run()
}

// VerifyContext fails unless the kubeconfig's current-context is the one we
// were told to use.
//
// Stricter than it needs to be, since every invocation passes --context
// explicitly. It is worth it because it makes "the cluster we read" and "the
// cluster we write" the same by construction: one Kubeconfig+Context pair
// configures lookout, kuberead and kubewrite alike, and there is no
// configuration in which the agent diagnoses one cluster and remediates
// another.
//
// Parsed textually rather than through client-go: a kubeconfig's
// current-context is a single top-level scalar, and pulling in k8s.io/client-go
// to read one string would be a heavy dependency for it. A file this cannot
// parse fails closed, which is the behaviour a safety check should have.
func VerifyContext(path, want string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("kubectl: read kubeconfig %s: %w", path, err)
	}
	got := ""
	for _, line := range strings.Split(string(raw), "\n") {
		rest, ok := strings.CutPrefix(line, "current-context:")
		if !ok {
			continue
		}
		got = strings.Trim(strings.TrimSpace(rest), `"'`)
		break
	}
	if got == "" {
		return fmt.Errorf("kubectl: %s declares no current-context; refusing to guess", path)
	}
	if got != want {
		return fmt.Errorf("kubectl: %s is pinned to context %q but %q was requested — "+
			"refusing to touch a cluster the caller did not name", path, got, want)
	}
	return nil
}
