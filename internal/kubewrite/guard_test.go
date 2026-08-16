package kubewrite

import (
	"strings"
	"testing"
)

func testGuard() guard {
	return guard{protected: DefaultProtectedNamespaces, bulkMax: DefaultBulkMax}
}

func TestNameValidation(t *testing.T) {
	g := testGuard()
	cases := []struct {
		in string
		ok bool
	}{
		{"web", true},
		{"web-abc12", true},
		{"data-db-0", true},
		{"my.app.v2", true},
		{"gke-pool-1-abc", true},
		{"", false},
		{"   ", false},
		{"Web", false},          // uppercase is not a DNS subdomain
		{"-web", false},         // the property the whole guard exists for
		{"--all", false},        // ... spelled out
		{"web-", false},         // trailing dash
		{"web pod", false},      // whitespace
		{"web;rm -rf /", false}, // nothing is shelled out, but still no
		{"web/../etc", false},   // slashes would change the resource path
		{strings.Repeat("a", 253), true},
		{strings.Repeat("a", 254), false},
	}
	for _, tc := range cases {
		err := g.name("pod_name", tc.in)
		if (err == nil) != tc.ok {
			t.Errorf("name(%q): err = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

// TestRefusalsSayNothingChanged. Every refusal ends the same way, because the
// model's next move should be to report it, and "refused" without "nothing
// changed" leaves the state of the cluster ambiguous in the report.
func TestRefusalsSayNothingChanged(t *testing.T) {
	g := testGuard()
	for _, err := range []error{
		g.name("pod_name", "--all"),
		g.namespace("kube-system"),
		g.resourceType("-x"),
		g.quantity("new_size", "lots"),
		g.dataKey("bad key"),
		g.replicas(-1),
		g.replicas(MaxReplicas + 1),
		g.bulk(0),
		g.bulk(DefaultBulkMax + 1),
		g.protectedIn([]string{"prod", "kube-public"}),
	} {
		if err == nil {
			t.Fatal("expected a refusal")
		}
		if !strings.HasPrefix(err.Error(), "refused: ") {
			t.Errorf("refusal does not announce itself: %q", err)
		}
		if !strings.HasSuffix(err.Error(), "Nothing was changed.") {
			t.Errorf("refusal does not say the cluster is unchanged: %q", err)
		}
	}
}

// TestRefusalTextIsNotReformatted pins the reason refusef uses errors.New.
// Refusal text routinely carries a manifest or a percentage, and a second pass
// through a format verb would mangle it — silently, and only for the inputs
// that contain a '%'.
func TestRefusalTextIsNotReformatted(t *testing.T) {
	err := refusef("target_cpu_utilization %d%% is out of range for %q.", 700, "web")
	want := "refused: target_cpu_utilization 700% is out of range for \"web\". Nothing was changed."
	if err.Error() != want {
		t.Errorf("got  %q\nwant %q", err, want)
	}
}

func TestQuantityValidation(t *testing.T) {
	g := testGuard()
	for _, ok := range []string{"1", "500m", "0.5", "512Mi", "20Gi", "2k", "1000000", "4G"} {
		if err := g.quantity("q", ok); err != nil {
			t.Errorf("quantity(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "20 Gi", "lots", "-1", "20gb", "1e9", "--all"} {
		if err := g.quantity("q", bad); err == nil {
			t.Errorf("quantity(%q) = nil, want a refusal", bad)
		}
	}
}

func TestDataKeyValidation(t *testing.T) {
	g := testGuard()
	for _, ok := range []string{"LOG_LEVEL", "app.conf", "a-b_c.d", "1"} {
		if err := g.dataKey(ok); err != nil {
			t.Errorf("dataKey(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"", "has space", "a/b", "a=b"} {
		if err := g.dataKey(bad); err == nil {
			t.Errorf("dataKey(%q) = nil, want a refusal", bad)
		}
	}
}

func TestReplicaCeiling(t *testing.T) {
	g := testGuard()
	for _, ok := range []int{0, 1, MaxReplicas} {
		if err := g.replicas(ok); err != nil {
			t.Errorf("replicas(%d) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []int{-1, MaxReplicas + 1} {
		if err := g.replicas(bad); err == nil {
			t.Errorf("replicas(%d) = nil, want a refusal", bad)
		}
	}
}

// TestProtectedNamespacesAreConfigurable, including the one way to turn the
// check off. Nil means "use the defaults" and an explicitly empty non-nil
// slice disables — a distinction that is deliberately hard to express by
// accident, since the accident is a cluster with no protected namespaces.
func TestProtectedNamespacesAreConfigurable(t *testing.T) {
	t.Run("nil takes the defaults", func(t *testing.T) {
		_, tools := kit(t, Config{})
		res, _ := call(t, tools, newGateContext(), "kubectl_delete_pod",
			map[string]any{"pod_name": "coredns-1", "namespace": "kube-system"})
		if res.Status != StatusRefused {
			t.Errorf("status = %q, want %q", res.Status, StatusRefused)
		}
	})

	t.Run("a custom list replaces them", func(t *testing.T) {
		run, tools := kit(t, Config{ProtectedNamespaces: []string{"prod"}})
		if res, _ := call(t, tools, newGateContext(), "kubectl_delete_pod",
			map[string]any{"pod_name": "web-1", "namespace": "prod"}); res.Status != StatusRefused {
			t.Errorf("prod not protected: %q", res.Status)
		}
		if res, _ := call(t, tools, approved(), "kubectl_delete_pod",
			map[string]any{"pod_name": "coredns-1", "namespace": "kube-system"}); res.Status != StatusApplied {
			t.Errorf("kube-system still protected: %q", res.Status)
		}
		if len(run.calls) != 1 {
			t.Errorf("ran %d commands, want 1", len(run.calls))
		}
	})

	t.Run("an empty non-nil list disables the check", func(t *testing.T) {
		_, tools := kit(t, Config{ProtectedNamespaces: []string{}})
		res, _ := call(t, tools, approved(), "kubectl_delete_pod",
			map[string]any{"pod_name": "coredns-1", "namespace": "kube-system"})
		if res.Status != StatusApplied {
			t.Errorf("status = %q, want %q", res.Status, StatusApplied)
		}
	})
}

// TestTheRealRunnerRefusesToGuess. Both fields are required; neither falls
// back to an ambient value. This machine has 64 kube contexts.
//
// The kubeconfig check the refusal runs on now lives in internal/kubectl,
// shared with the read path; kubectl.TestVerifyContextRefusesAMismatchedKubeconfig
// covers it. What is still worth asserting here is that Tools reaches it at
// all rather than building a runner that resolves the ambient context.
func TestTheRealRunnerRefusesToGuess(t *testing.T) {
	if _, err := Tools(Config{Kubeconfig: "/tmp/kubeconfig"}); err == nil {
		t.Error("an empty Context was accepted")
	}
	if _, err := Tools(Config{Context: "kind-sre-eval-a1"}); err == nil {
		t.Error("an empty Kubeconfig was accepted")
	}
}
