package kubectl

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVerifyContextRefusesAMismatchedKubeconfig.
//
// The pin is the reason there is no configuration in which the agent diagnoses
// one cluster and remediates another. It fails closed on a kubeconfig it
// cannot read a current-context out of, because a safety check that treats
// "unparseable" as "fine" is not one.
//
// Ungated on purpose, and it moved here from internal/kubewrite when the third
// caller appeared: a safety check that only runs when Docker is up is a safety
// check that does not run in CI, and one that is only exercised through the
// write path stops covering the read path the moment the read path grows a
// cluster connection of its own.
func TestVerifyContextRefusesAMismatchedKubeconfig(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}

	matching := write("match", "apiVersion: v1\ncurrent-context: kind-sre-eval-a1\n")
	if err := VerifyContext(matching, "kind-sre-eval-a1"); err != nil {
		t.Errorf("matching kubeconfig rejected: %v", err)
	}
	quoted := write("quoted", "current-context: \"kind-sre-eval-a1\"\n")
	if err := VerifyContext(quoted, "kind-sre-eval-a1"); err != nil {
		t.Errorf("quoted current-context rejected: %v", err)
	}

	// The names are real ones from this machine's kubeconfig. A check that only
	// refuses a made-up string is not evidence about the clusters it is meant
	// to keep away from.
	for _, live := range []string{"gke_acme_prod", "kode-gopher-smoke", "agent-sandbox-poc"} {
		other := write("other-"+live, "current-context: "+live+"\n")
		if err := VerifyContext(other, "kind-sre-eval-a1"); err == nil {
			t.Errorf("a kubeconfig pinned to %q was accepted", live)
		}
	}

	none := write("none", "apiVersion: v1\nclusters: []\n")
	err := VerifyContext(none, "kind-sre-eval-a1")
	if err == nil {
		t.Fatal("a kubeconfig with no current-context was accepted")
	}
	if !strings.Contains(err.Error(), "no current-context") {
		// internal/lookout's test asserts on this phrase too. Keeping it stable
		// is cheaper than three packages each inventing their own wording for
		// the same refusal.
		t.Errorf("refusal does not say what is wrong: %v", err)
	}
	if err := VerifyContext(filepath.Join(dir, "absent"), "kind-sre-eval-a1"); err == nil {
		t.Error("a missing kubeconfig was accepted")
	}
}

// New refuses both halves of the pin before it looks at anything else. This
// machine has 64 kube contexts.
func TestNewRefusesToGuess(t *testing.T) {
	if _, err := New(Config{Kubeconfig: "/tmp/kubeconfig"}); err == nil {
		t.Error("an empty Context was accepted")
	}
	if _, err := New(Config{Context: "kind-sre-eval-a1"}); err == nil {
		t.Error("an empty Kubeconfig was accepted")
	}
}
