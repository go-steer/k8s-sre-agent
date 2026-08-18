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

// Package lookout exposes k8s-lookout's read-path checks as an ADK toolset.
//
// lookout is consumed as a *subprocess* speaking MCP over stdio, never as a
// Go import. k8s-lookout depends on core-agent, which depends on ADK v1,
// while this repo is on ADK v2; linking both majors into one binary is the
// failure mode this indirection exists to prevent. See AGENTS.md.
package lookout

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/go-steer/k8s-sre-agent/internal/kubectl"
)

// ToolsetName is how specialist specs refer to this toolset in their MCP
// allowlist (`tools: mcp: - server: lookout`).
//
// ADK's mcptoolset hardcodes Name() to "mcp_tool_set" regardless of which
// server it wraps, so mast's per-server allowlist cannot tell two servers
// apart. Named() fixes that here rather than in mast: the allowlist is a
// config surface operators read, and "server: mcp_tool_set" tells them
// nothing about which cluster tool they are granting.
const ToolsetName = "lookout"

// EnvBinary overrides the lookout executable path.
const EnvBinary = "SRE_LOOKOUT_BIN"

// DefaultBinary is the executable name resolved on PATH when Config.Binary
// and EnvBinary are both empty.
const DefaultBinary = "lookout"

// Config selects the cluster lookout reads and the binary that reads it.
type Config struct {
	// Binary is the lookout executable. Empty falls back to EnvBinary, then
	// to DefaultBinary resolved on PATH.
	Binary string

	// Kubeconfig is the path to the kubeconfig the subprocess may use. It is
	// passed as KUBECONFIG and is the *only* cluster credential the child
	// inherits — see Toolset for why this is required.
	Kubeconfig string

	// Context names the kube context that must be current in Kubeconfig.
	// Required. Toolset verifies the file's current-context matches before
	// spawning anything.
	Context string

	// Env supplies additional environment for the child, as "K=V" entries.
	// The child otherwise starts from an empty environment.
	Env []string
}

// Toolset spawns `lookout mcp` and adapts its checks to ADK tools.
//
// The subprocess starts from an *empty* environment plus exactly what
// Config specifies. This is deliberate. lookout resolves its cluster from
// KUBECONFIG's current-context with no per-call context flag, so inheriting
// our environment would let it read whichever of this machine's kube
// contexts happens to be current — including live production clusters. An
// explicit Kubeconfig plus a verified Context is the pin; the empty base
// environment is what makes the pin airtight rather than advisory.
//
// The caller closes the returned toolset to terminate the subprocess.
func Toolset(ctx context.Context, cfg Config) (tool.Toolset, error) {
	if cfg.Context == "" {
		return nil, fmt.Errorf("lookout: Context is required — refusing to resolve the ambient current-context")
	}
	if cfg.Kubeconfig == "" {
		return nil, fmt.Errorf("lookout: Kubeconfig is required — refusing to fall back to ~/.kube/config")
	}
	if err := verifyContext(cfg.Kubeconfig, cfg.Context); err != nil {
		return nil, err
	}

	cmd, err := spawn(cfg)
	if err != nil {
		return nil, err
	}

	ts, err := mcptoolset.New(mcptoolset.Config{
		Transport: &mcp.CommandTransport{Command: cmd},
	})
	if err != nil {
		return nil, err
	}
	return Named(ToolsetName, ts), nil
}

// spawn builds the `lookout mcp` command Config describes. Callers must have
// validated cfg first; this is the half Toolset and Surface share.
func spawn(cfg Config) (*exec.Cmd, error) {
	bin := cfg.Binary
	if bin == "" {
		bin = os.Getenv(EnvBinary)
	}
	if bin == "" {
		bin = DefaultBinary
	}
	resolved, err := exec.LookPath(bin)
	if err != nil {
		return nil, fmt.Errorf("lookout: locate %q: %w (build it from ../k8s-lookout or set %s)", bin, err, EnvBinary)
	}

	// #nosec G702 -- resolved is exec.LookPath's answer for an operator-set
	// binary name, not model or cluster input, and the only argument is a
	// constant. The child's environment is constructed just below rather than
	// inherited, which is the containment that actually matters here.
	cmd := exec.Command(resolved, "mcp")
	cmd.Env = append([]string{"KUBECONFIG=" + cfg.Kubeconfig}, cfg.Env...)
	// lookout writes diagnostics to stderr only; stdout is the JSON-RPC
	// channel and must not be touched.
	cmd.Stderr = os.Stderr
	return cmd, nil
}

// ToolInfo is one advertised tool plus the annotation ADK throws away.
//
// mcptoolset.convertTool copies a tool's name, description and schemas onto an
// ADK tool.Tool and drops mcp.Tool.Annotations entirely, so ReadOnlyHint — the
// server's own statement about whether calling the tool changes anything — is
// not reachable from a tool.Toolset. That is the one field a read-only entry
// point most needs, hence this second, annotation-preserving view of the same
// surface.
type ToolInfo struct {
	Name     string
	ReadOnly bool
}

// Surface lists what the lookout binary advertises, annotations included.
//
// It is a separate handshake rather than a read of the captured tools.json,
// and deliberately so: the captured surface is whatever binary somebody last
// ran captureschema against, and the failure this exists to catch is precisely
// a lookout release that grows a write underneath us. Listing tools touches no
// cluster — the child still gets the same pinned kubeconfig and the same empty
// base environment as the toolset's, so nothing is loosened to ask the
// question.
func Surface(ctx context.Context, cfg Config) ([]ToolInfo, error) {
	if cfg.Context == "" || cfg.Kubeconfig == "" {
		return nil, fmt.Errorf("lookout: Surface needs the same Kubeconfig and Context pin as Toolset")
	}
	if err := verifyContext(cfg.Kubeconfig, cfg.Context); err != nil {
		return nil, err
	}
	cmd, err := spawn(cfg)
	if err != nil {
		return nil, err
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "k8s-sre-agent", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return nil, fmt.Errorf("lookout: connect for tool list: %w", err)
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("lookout: list tools: %w", err)
	}
	out := make([]ToolInfo, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, ToolInfo{Name: t.Name, ReadOnly: t.Annotations != nil && t.Annotations.ReadOnlyHint})
	}
	return out, nil
}

// storeWriters are the lookout tools that advertise ReadOnlyHint:false, with
// the reason each one does.
//
// "lookout is a read-only binary" was true when this repo started and is not
// true now: three of its twenty-four tools declare a write. All three write to
// the *sentinel's own SQLite store* — the file named by their `store`
// argument — and none of them mutates a Kubernetes object, which is why they
// are classified here rather than refused outright.
//
// The list is explicit so that a lookout release adding a fourth writer stops
// a read-only run until somebody classifies it. That is the fail-closed
// direction: the cost of a stale list is a refusal that names the tool, and
// the cost of no list at all was a guarantee that had quietly stopped holding.
var storeWriters = map[string]string{
	"k8s_findings_diff": "advances the persisted finding state as a side effect of reporting transitions (--dry-run is its read-only mode)",
	"k8s_findings_ack":  "opens or clears a suppression window on a finding",
	"k8s_triage_status": "files a triage record so later scans stop reporting the incident as a fresh unknown",
}

// StoreWriters returns the classified write-declaring tool names, sorted.
func StoreWriters() []string {
	out := make([]string, 0, len(storeWriters))
	for name := range storeWriters {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// UnclassifiedWriters returns the tools in surface that declare a write and
// are not in storeWriters, sorted. Empty means every write lookout advertises
// is one this repo has looked at.
func UnclassifiedWriters(surface []ToolInfo) []string {
	var out []string
	for _, t := range surface {
		if t.ReadOnly {
			continue
		}
		if _, known := storeWriters[t.Name]; !known {
			out = append(out, t.Name)
		}
	}
	sort.Strings(out)
	return out
}

// Named relabels a toolset. Exported because the offline eval toolset must
// answer to the same name as the live one — a specialist spec that allowlists
// "lookout" has to work in both.
func Named(name string, ts tool.Toolset) tool.Toolset { return named{name, ts} }

type named struct {
	name string
	tool.Toolset
}

func (n named) Name() string { return n.name }

// verifyContext fails unless the kubeconfig's current-context is exactly the
// expected one.
//
// The check itself is internal/kubectl's, shared with the write path and with
// kuberead. Wrapped rather than called bare so the error still says which half
// of the data plane refused; the refusal is the same refusal either way, and
// three copies of twenty lines of YAML scanning was one too many.
func verifyContext(path, want string) error {
	if err := kubectl.VerifyContext(path, want); err != nil {
		return fmt.Errorf("lookout: %w", err)
	}
	return nil
}

// Scan runs one lookout subcommand to completion and returns its stdout.
//
// This is the third way into the same binary, alongside Toolset (MCP, for the
// agent) and Surface (a tool list, for the read-only guard), and it exists for
// the bounded pass: a collection step that spends no model tokens has to invoke
// the checks itself rather than offer them to a model. `lookout health` and
// `lookout triage delta` are the two that answer "anything wrong here" in one
// call each.
//
// It shares spawn's pinning rather than reimplementing it, which is the whole
// reason it lives in this package. The subprocess starts from an empty
// environment plus Config.Env, KUBECONFIG names the only cluster it can see,
// and the current-context is verified before anything is executed — the same
// three properties Toolset documents at length, none of which survive being
// re-derived at a call site.
//
// A non-zero exit is returned as an error *with* whatever the command printed,
// because lookout's checks are partial by design: a scan that reached nine of
// ten categories and failed the tenth has still produced nine categories of
// answer, and discarding them because the process exited 1 would turn a
// degraded collection into no collection.
func Scan(ctx context.Context, cfg Config, args ...string) (string, error) {
	return Pipe(ctx, cfg, "", args...)
}

// Pipe is Scan with something on the child's stdin.
//
// It exists for one command: `lookout findings diff --report=-` reads a finding
// report on stdin and diffs it against its SQLite store. That is the pipeline
// lookout documents, and running it as two processes with a temp file in
// between would be the same thing with a file to clean up.
func Pipe(ctx context.Context, cfg Config, stdin string, args ...string) (string, error) {
	if cfg.Context == "" || cfg.Kubeconfig == "" {
		return "", fmt.Errorf("lookout: Scan needs the same Kubeconfig and Context pin as Toolset")
	}
	if err := verifyContext(cfg.Kubeconfig, cfg.Context); err != nil {
		return "", err
	}
	cmd, err := spawn(cfg)
	if err != nil {
		return "", err
	}
	// spawn builds the `mcp` invocation; this wants a different subcommand on
	// the same pinned environment.
	cmd.Args = append([]string{cmd.Path}, args...)

	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return out.String(), ctx.Err()
	}
	if runErr != nil {
		return out.String(), fmt.Errorf("lookout %s: %w", strings.Join(args, " "), runErr)
	}
	return out.String(), nil
}
