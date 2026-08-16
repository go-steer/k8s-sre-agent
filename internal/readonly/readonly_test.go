package readonly

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"

	"github.com/go-steer/core-sre-agent/internal/kubewrite"
)

// A minified, flattened kubeconfig as `kubectl config view --minify --flatten`
// actually emits one: list items at column zero, the context's own `name:`
// indented under the item, and nested cluster/user names elsewhere in the file
// that must not be counted as contexts.
const soleContext = `apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: REDACTED
    server: https://35.0.0.1
  name: gke_example_us-central1_simian-test
contexts:
- context:
    cluster: gke_example_us-central1_simian-test
    user: gke_example_us-central1_simian-test
  name: simian-test
current-context: simian-test
kind: Config
preferences: {}
users:
- name: gke_example_us-central1_simian-test
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: gke-gcloud-auth-plugin
`

const twoContexts = `apiVersion: v1
clusters:
- cluster:
    server: https://35.0.0.1
  name: simian-test-cluster
- cluster:
    server: https://35.0.0.2
  name: prod-cluster
contexts:
- context:
    cluster: simian-test-cluster
    user: a
  name: simian-test
- context:
    cluster: prod-cluster
    user: b
  name: prod
current-context: simian-test
kind: Config
`

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestContextNames(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want []string
	}{
		// The point of the file: a cluster named after the context and a user
		// named after the cluster are both `- name:` entries, and neither is a
		// context.
		{"minified", soleContext, []string{"simian-test"}},
		{"two", twoContexts, []string{"simian-test", "prod"}},
		{"name first", "contexts:\n- name: only\n  context:\n    cluster: c\n", []string{"only"}},
		{"quoted", "contexts:\n- context:\n    cluster: c\n  name: \"quoted-ctx\"\n", []string{"quoted-ctx"}},
		{"empty list", "contexts: []\ncurrent-context: nothing\n", nil},
		{"no contexts key", "clusters:\n- cluster:\n    server: x\n  name: c\n", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ContextNames(tc.raw)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ContextNames() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The load-bearing safety check, and ungated on purpose — the same principle
// internal/kindcluster's guard tests follow. A pin that only holds when the
// live-cluster env gate is set is a pin that does not hold in CI.
func TestVerifySoleContext(t *testing.T) {
	if err := VerifySoleContext(write(t, soleContext), "simian-test"); err != nil {
		t.Errorf("a minified single-context kubeconfig should be accepted: %v", err)
	}

	// The hazard this half exists for: the pin is right, the file is not.
	// kubectl would be fine — we pass --context on every call — but lookout
	// resolves current-context out of the file and nothing stops a later edit
	// from moving it.
	err := VerifySoleContext(write(t, twoContexts), "simian-test")
	if err == nil {
		t.Fatal("a kubeconfig describing a second cluster must be refused")
	}
	if !strings.Contains(err.Error(), "--minify") {
		t.Errorf("the refusal should hand back the recipe that fixes it, got: %v", err)
	}

	// Requesting a context the file does not point at is kubectl's check, and
	// it must still run first.
	if err := VerifySoleContext(write(t, soleContext), "prod"); err == nil {
		t.Error("a context that is not the file's current-context must be refused")
	}

	if err := VerifySoleContext(filepath.Join(t.TempDir(), "absent"), "simian-test"); err == nil {
		t.Error("a missing kubeconfig must be refused")
	}
}

// staticToolset stands in for a toolset that grew a write surface underneath
// us. Nothing in this command builds kubewrite.Tools, so the only way to
// exercise the check is to hand it one.
type staticToolset struct{ tools []tool.Tool }

func (s staticToolset) Name() string                                     { return "static" }
func (s staticToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) { return s.tools, nil }

// nopRunner satisfies kubewrite.Runner without a kubectl on PATH. The tools are
// only ever declared here, never run.
type nopRunner struct{}

func (nopRunner) Run(context.Context, string, ...string) (string, error) { return "", nil }
func (nopRunner) Cluster() string                                        { return "test" }

func TestRefuseWriteToolsRejectsAMutatingTool(t *testing.T) {
	// The real write tools, so the check is tested against the actual names
	// rather than against a string both sides made up.
	writes, err := kubewrite.Tools(kubewrite.Config{Runner: nopRunner{}})
	if err != nil {
		t.Fatalf("build write tools: %v", err)
	}
	if len(writes) == 0 {
		t.Fatal("no write tools to test against")
	}

	err = RefuseWriteTools(context.Background(), []tool.Toolset{staticToolset{writes}})
	if err == nil {
		t.Fatal("a toolset declaring the real write tools must be refused")
	}
	if !strings.Contains(err.Error(), writes[0].Name()) {
		t.Errorf("the refusal should name the offending tool, got: %v", err)
	}

	if err := RefuseWriteTools(context.Background(), nil); err != nil {
		t.Errorf("no toolsets is not a violation: %v", err)
	}

	// And a read tool with a plausible name is not a write tool. The check is
	// an exact name match against kubewrite.ToolNames(), not a heuristic on the
	// word "delete" — k8s_drain_blockers and kubectl_get_pods would both fail a
	// heuristic and neither mutates anything.
	if err := RefuseWriteTools(context.Background(), []tool.Toolset{
		staticToolset{[]tool.Tool{readTool{"k8s_list_resources"}, readTool{"k8s_drain_blockers"}}},
	}); err != nil {
		t.Errorf("read tools must pass: %v", err)
	}
}

// A withheld tool must be gone from the declaration surface, not merely
// discouraged: the surface is what the model acts on, and lookout's three
// store writers each take a `store` path, so leaving one in place leaves a
// read-only assessment holding a create-a-file-anywhere primitive. Ungated,
// like every other refusal in this command.
func TestWithheldToolsAreNotDeclared(t *testing.T) {
	all := []tool.Tool{
		readTool{"k8s_cluster_health"},
		readTool{"k8s_findings_diff"},
		readTool{"k8s_triage_status"},
	}
	filtered := WithoutTools(staticToolset{all}, []string{"k8s_findings_diff", "k8s_triage_status"})

	if filtered.Name() != "static" {
		t.Errorf("filtering renamed the toolset to %q; specialist specs allowlist by name", filtered.Name())
	}
	got, err := filtered.Tools(readonlyCtx{context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name() != "k8s_cluster_health" {
		names := make([]string, len(got))
		for i, tl := range got {
			names[i] = tl.Name()
		}
		t.Errorf("filtered toolset declares %v, want only k8s_cluster_health", names)
	}

	// Nothing to withhold is the common case on a lookout with no writers at
	// all, and it must not wrap the toolset in a filter that could later hide
	// something by accident.
	same := WithoutTools(staticToolset{all}, nil)
	if got, _ := same.Tools(readonlyCtx{context.Background()}); len(got) != len(all) {
		t.Errorf("withholding nothing dropped %d tools", len(all)-len(got))
	}
}

// KUBECONFIG is the one variable that could repoint the lookout subprocess at
// another cluster, and it is the one an operator is most likely to have set —
// running this command at all involves exporting it. Ungated for the same
// reason the other pins are.
func TestCredentialEnvNeverForwardsKubeconfig(t *testing.T) {
	got := FilterCredentialEnv([]string{
		"KUBECONFIG=/home/user/.kube/config",
		"PATH=/usr/bin",
		"HOME=/home/user",
		"GOOGLE_APPLICATION_CREDENTIALS=/creds/sa.json",
		"ANTHROPIC_VERTEX_PROJECT_ID=secret-project",
		"AWS_SECRET_ACCESS_KEY=nope",
	})
	want := []string{"PATH=/usr/bin", "HOME=/home/user", "GOOGLE_APPLICATION_CREDENTIALS=/creds/sa.json"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("FilterCredentialEnv() = %v, want %v", got, want)
	}

	// An allowlist, not a denylist: an unrecognised variable is dropped rather
	// than passed through. The list grows by measuring a provider that needs
	// something, which is how PATH and HOME got here.
	for _, kv := range got {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(CredentialNames, name) {
			t.Errorf("forwarded %q, which is not on the allowlist", name)
		}
	}
}

// readTool is a tool.Tool that only has to answer Name().
type readTool struct{ name string }

func (r readTool) Name() string        { return r.name }
func (r readTool) Description() string { return "" }
func (r readTool) IsLongRunning() bool { return false }
