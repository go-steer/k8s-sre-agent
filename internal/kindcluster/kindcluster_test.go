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

package kindcluster

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The guards below are the ones that keep fault injection away from a real
// cluster, so they are tested without a cluster attached. A safety check that
// only runs when Docker is up is a safety check that does not run in CI.

func TestNameMustCarryThePrefix(t *testing.T) {
	bad := []string{
		"",
		"prod",
		"kind-prod",
		"my-sre-eval-1", // prefix must lead, not appear
		NamePrefix,      // prefix alone names nothing
		"sre-eval",      // near miss: no trailing hyphen
		"gke_acme_prod", // the shape of a real context on this machine
	}
	for _, name := range bad {
		if err := checkName(name); err == nil {
			t.Errorf("checkName(%q) accepted a name this package must not manage", name)
		}
	}
	if err := checkName(NamePrefix + "abc123"); err != nil {
		t.Errorf("checkName rejected a valid name: %v", err)
	}
}

// destroy is the single path to `kind delete`. If it ever stops re-checking
// the name, a caller that skips Delete's check could remove someone's cluster.
//
// The name here is the near miss rather than an obviously foreign one: a
// prefix test written as HasPrefix(name, "sre-eval") — the constant with its
// trailing hyphen dropped — accepts this and deletes a cluster we did not
// make. An unrelated name would pass such a test just as happily.
func TestDestroyRefusesAForeignCluster(t *testing.T) {
	err := destroy(context.Background(), "sre-eval")
	if err == nil {
		t.Fatal("destroy accepted a cluster it did not create")
	}
	if !strings.Contains(err.Error(), NamePrefix) {
		t.Errorf("error should name the prefix rule, got: %v", err)
	}
}

// Likewise on the exported path, with the other near miss: the prefix appears
// but does not lead, which is what a Contains-based check would wave through.
func TestDeleteRefusesAForeignCluster(t *testing.T) {
	c := &Cluster{Name: "team-sre-eval-2", Context: "kind-team-sre-eval-2"}
	if err := c.Delete(context.Background()); err == nil {
		t.Fatal("Delete accepted a cluster it did not create")
	}
}

func TestCreateRejectsABadName(t *testing.T) {
	if _, err := Create(context.Background(), Config{Name: "scratch"}); err == nil {
		t.Fatal("Create accepted an unprefixed name")
	}
}

// kind merges into whatever kubeconfig it is handed. Pointing it at an
// existing file — above all the user's real one — would put 64 contexts into
// the file the agent reads, and verifyIsolation would then be the only thing
// standing between fault injection and production.
func TestCreateRefusesAnExistingKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(path, []byte("apiVersion: v1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := prepareKubeconfig(Config{Name: NamePrefix + "x", Kubeconfig: path})
	if err == nil {
		t.Fatal("prepareKubeconfig accepted an existing kubeconfig")
	}
	if !strings.Contains(err.Error(), "merges") {
		t.Errorf("error should explain the merge hazard, got: %v", err)
	}
}

func TestVerifyIsolation(t *testing.T) {
	const ours = `apiVersion: v1
clusters:
- cluster: {server: https://127.0.0.1:6443}
  name: kind-sre-eval-x
contexts:
- context: {cluster: kind-sre-eval-x, user: kind-sre-eval-x}
  name: kind-sre-eval-x
current-context: kind-sre-eval-x
`
	// The dangerous shape: our context is current, but a production cluster is
	// one `kubectl config use-context` away.
	const merged = `apiVersion: v1
contexts:
- context: {cluster: kind-sre-eval-x}
  name: kind-sre-eval-x
- context: {cluster: gke_acme_prod}
  name: gke_acme_prod
current-context: kind-sre-eval-x
`
	const wrongCurrent = `apiVersion: v1
contexts:
- context: {cluster: gke_acme_prod}
  name: gke_acme_prod
current-context: gke_acme_prod
`

	for _, tc := range []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"isolated", ours, false},
		{"merged with a real cluster", merged, true},
		{"pinned elsewhere", wrongCurrent, true},
		{"no current-context", "apiVersion: v1\n", true},
		{"empty", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			c := &Cluster{
				Name:       NamePrefix + "x",
				Context:    ContextPrefix + NamePrefix + "x",
				Kubeconfig: path,
			}
			err := c.verifyIsolation()
			if tc.wantErr && err == nil {
				t.Error("verifyIsolation accepted a kubeconfig that reaches beyond our cluster")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("verifyIsolation rejected an isolated kubeconfig: %v", err)
			}
		})
	}
}

// The child must not inherit the caller's KUBECONFIG. kubectl merges every
// path in that variable, so an inherited one would re-expose the contexts the
// isolated file was written to exclude.
func TestKindEnvDropsKubeconfig(t *testing.T) {
	env := filterEnv([]string{"PATH=/bin", "KUBECONFIG=/home/user/.kube/config", "HOME=/home/user"}, "KUBECONFIG")
	for _, kv := range env {
		if strings.HasPrefix(kv, "KUBECONFIG=") {
			t.Fatalf("KUBECONFIG survived filtering: %q", env)
		}
	}
	if len(env) != 2 {
		t.Errorf("filterEnv dropped more than it was asked to: %q", env)
	}
}
