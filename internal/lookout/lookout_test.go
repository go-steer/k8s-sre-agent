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

package lookout

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// fakeKubeconfig writes a syntactically valid kubeconfig pinned to a context
// whose server is unroutable. Any check that actually dials the cluster
// fails; tools/list, which does not, still works. That makes it safe to run
// the MCP handshake on a machine full of live cluster credentials.
func fakeKubeconfig(t *testing.T, ctxName string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	body := `apiVersion: v1
kind: Config
current-context: ` + ctxName + `
clusters:
- name: nowhere
  cluster:
    server: https://127.0.0.1:1
contexts:
- name: ` + ctxName + `
  context:
    cluster: nowhere
    user: nobody
users:
- name: nobody
  user: {}
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// The guard is the whole point of this package's safety story: this machine
// has dozens of kube contexts including live GKE clusters, and lookout has no
// per-call context flag. Every path that could reach an unnamed cluster must
// fail before the subprocess starts.
func TestToolsetRefusesUnpinnedClusters(t *testing.T) {
	kubeconfig := fakeKubeconfig(t, "kind-sre-evals")

	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"no context", Config{Kubeconfig: kubeconfig}, "Context is required"},
		{"no kubeconfig", Config{Context: "kind-sre-evals"}, "Kubeconfig is required"},
		// The wording is internal/kubectl's now, shared with the write path and
		// with kuberead — hence "touch" rather than "read".
		{"context mismatch", Config{Kubeconfig: kubeconfig, Context: "gke-prod"}, "refusing to touch a cluster"},
		{"missing file", Config{Kubeconfig: kubeconfig + ".nope", Context: "kind-sre-evals"}, "read kubeconfig"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := Toolset(context.Background(), tc.cfg)
			if err == nil {
				t.Fatalf("Toolset succeeded; want refusal containing %q", tc.want)
			}
			if ts != nil {
				t.Error("Toolset returned a non-nil toolset alongside an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not contain %q", err, tc.want)
			}
		})
	}
}

func TestVerifyContextRejectsKubeconfigWithoutCurrentContext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte("apiVersion: v1\nkind: Config\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := verifyContext(path, "anything")
	if err == nil || !strings.Contains(err.Error(), "no current-context") {
		t.Fatalf("got %v, want a no-current-context refusal", err)
	}
}

// TestToolsetListsChecks performs the real MCP handshake against a real
// `lookout mcp` subprocess. It needs the binary but no cluster: the toolset
// is only enumerated, never invoked.
//
// The assertions cover the names alias.go maps onto Python tool names. A
// silent rename in lookout would otherwise show up as an unexplained drop in
// tool_coverage rather than as a build failure.
func TestToolsetListsChecks(t *testing.T) {
	if _, err := exec.LookPath(binaryName()); err != nil {
		t.Skipf("lookout binary not found (%v); build it or set %s", err, EnvBinary)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	ts, err := Toolset(ctx, Config{
		Kubeconfig: fakeKubeconfig(t, "kind-sre-evals"),
		Context:    "kind-sre-evals",
	})
	if err != nil {
		t.Fatalf("Toolset: %v", err)
	}

	tools, err := ts.Tools(readonlyCtx{ctx})
	if err != nil {
		t.Fatalf("enumerate tools: %v", err)
	}
	names := map[string]bool{}
	for _, tl := range tools {
		names[tl.Name()] = true
	}
	if len(names) == 0 {
		t.Fatal("lookout exposed no tools")
	}

	// Every lookout name alias.go knows about must exist, or the alias is
	// dead weight and the Go agent silently loses credit for that intent.
	for _, want := range []string{
		"k8s_cluster_health",
		"k8s_triage_status",
		"k8s_triage_workload",
		"k8s_triage_logs",
		"k8s_event_timeline",
		"k8s_resource_top",
		"k8s_recent_changes",
	} {
		if !names[want] {
			t.Errorf("lookout no longer exposes %q; internal/evals/alias.go is stale", want)
		}
	}

	got := make([]string, 0, len(names))
	for n := range names {
		got = append(got, n)
	}
	sort.Strings(got)
	t.Logf("%d lookout tools: %s", len(got), strings.Join(got, " "))
}

// readonlyCtx is the minimum agent.ReadonlyContext a toolset enumeration
// needs. ADK's own constructor lives in an internal package, and mcptoolset
// only reads the embedded context.Context when listing, so a stub is both
// sufficient and the only option available from outside the module.
type readonlyCtx struct{ context.Context }

func (readonlyCtx) UserContent() *genai.Content          { return nil }
func (readonlyCtx) InvocationID() string                 { return "test" }
func (readonlyCtx) AgentName() string                    { return "test" }
func (readonlyCtx) ReadonlyState() session.ReadonlyState { return nil }
func (readonlyCtx) UserID() string                       { return "test" }
func (readonlyCtx) AppName() string                      { return "test" }
func (readonlyCtx) SessionID() string                    { return "test" }
func (readonlyCtx) Branch() string                       { return "" }

func binaryName() string {
	if b := os.Getenv(EnvBinary); b != "" {
		return b
	}
	return DefaultBinary
}

// TestOfflineSurfaceIsLoadable proves the embedded capture parses and yields
// the full tool surface with descriptions intact — the descriptions are what
// drive tool selection, so an empty one silently degrades every eval score.
func TestOfflineSurfaceIsLoadable(t *testing.T) {
	ts, rec, err := Offline()
	if err != nil {
		t.Fatal(err)
	}
	if ts.Name() != ToolsetName {
		t.Errorf("toolset name = %q, want %q — specialist allowlists key on this", ts.Name(), ToolsetName)
	}
	tools, err := ts.Tools(readonlyCtx{t.Context()})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) < 20 {
		t.Fatalf("offline surface has %d tools; the capture looks truncated", len(tools))
	}
	for _, tl := range tools {
		if tl.Description() == "" {
			t.Errorf("%s has an empty description", tl.Name())
		}
	}
	if got := rec.Names(); len(got) != 0 {
		t.Errorf("fresh recorder already holds %v", got)
	}
}

// TestOfflineMatchesLiveSurface catches drift between the embedded capture and
// the lookout build on this machine. Drift is not an error in itself — it means
// the capture is stale and should be regenerated.
func TestOfflineMatchesLiveSurface(t *testing.T) {
	if _, err := exec.LookPath(binaryName()); err != nil {
		t.Skipf("lookout binary not found (%v)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	live, err := Toolset(ctx, Config{
		Kubeconfig: fakeKubeconfig(t, "kind-sre-evals"),
		Context:    "kind-sre-evals",
	})
	if err != nil {
		t.Fatal(err)
	}
	liveTools, err := live.Tools(readonlyCtx{ctx})
	if err != nil {
		t.Fatal(err)
	}

	offline, _, err := Offline()
	if err != nil {
		t.Fatal(err)
	}
	offlineTools, err := offline.Tools(readonlyCtx{ctx})
	if err != nil {
		t.Fatal(err)
	}

	got := map[string]string{}
	for _, tl := range offlineTools {
		got[tl.Name()] = tl.Description()
	}
	for _, tl := range liveTools {
		desc, ok := got[tl.Name()]
		if !ok {
			t.Errorf("live lookout exposes %q but the capture does not; run `go run ./dev/captureschema`", tl.Name())
			continue
		}
		if desc != tl.Description() {
			t.Errorf("%q description drifted from the capture; run `go run ./dev/captureschema`", tl.Name())
		}
		delete(got, tl.Name())
	}
	for name := range got {
		t.Errorf("capture holds %q which live lookout no longer exposes; run `go run ./dev/captureschema`", name)
	}
}
