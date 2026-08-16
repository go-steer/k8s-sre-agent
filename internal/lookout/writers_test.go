package lookout

import (
	"context"
	"os/exec"
	"slices"
	"testing"
	"time"
)

// Ungated, for the reason kindcluster's guards are: a safety check that only
// runs when the lookout binary happens to be on this machine is a safety check
// that does not run in CI. The captured surface is checked-in data, so the
// classification can be tested against it with no binary and no cluster.
func TestEveryCapturedWriteIsClassified(t *testing.T) {
	surface, err := Captured()
	if err != nil {
		t.Fatal(err)
	}
	if len(surface) < 20 {
		t.Fatalf("captured surface has %d tools; it looks truncated", len(surface))
	}

	if unknown := UnclassifiedWriters(surface); len(unknown) > 0 {
		t.Errorf("captured surface declares unclassified write tool(s) %v; cmd/sre-agent refuses to run "+
			"against a lookout that advertises one, so classify them in storeWriters (or explain why "+
			"they must not be)", unknown)
	}

	// And the classification must not outlive the tools it classifies. A name
	// left here after lookout drops the tool reads as "we looked at this",
	// which is the failure this whole task came from one rung down.
	for _, name := range StoreWriters() {
		if !slices.ContainsFunc(surface, func(t ToolInfo) bool { return t.Name == name }) {
			t.Errorf("storeWriters names %q, which the captured surface does not advertise", name)
		}
	}
}

// The three writers are named individually because each one is a decision:
// lookout says it writes, and this repo says the write lands in lookout's own
// store rather than on the cluster. A capture that quietly stops carrying one
// of them should fail here rather than widen what a read-only run is offered.
func TestTheKnownWritersAreTheOnesWeExpect(t *testing.T) {
	want := []string{"k8s_findings_ack", "k8s_findings_diff", "k8s_triage_status"}
	if got := StoreWriters(); !slices.Equal(got, want) {
		t.Errorf("StoreWriters() = %v, want %v", got, want)
	}

	surface, err := Captured()
	if err != nil {
		t.Fatal(err)
	}
	var writes []string
	for _, tl := range surface {
		if !tl.ReadOnly {
			writes = append(writes, tl.Name)
		}
	}
	slices.Sort(writes)
	if !slices.Equal(writes, want) {
		t.Errorf("captured surface declares writes %v, want %v", writes, want)
	}
}

// The check has to fail on a tool nobody has looked at, which is the only
// state it exists to catch — every currently-shipping writer is classified, so
// nothing else in the suite exercises the refusal.
func TestAnUnknownWriterIsNotClassified(t *testing.T) {
	surface := []ToolInfo{
		{Name: "k8s_cluster_health", ReadOnly: true},
		{Name: "k8s_findings_diff"},
		{Name: "k8s_scale_deployment"},
		{Name: "k8s_delete_pod"},
	}
	got := UnclassifiedWriters(surface)
	want := []string{"k8s_delete_pod", "k8s_scale_deployment"}
	if !slices.Equal(got, want) {
		t.Errorf("UnclassifiedWriters() = %v, want %v", got, want)
	}

	// A tool that declares itself read-only is never a writer, and a surface
	// of only read tools yields nothing rather than an empty-but-non-nil
	// complaint the caller would have to special-case.
	if got := UnclassifiedWriters([]ToolInfo{{Name: "k8s_cluster_health", ReadOnly: true}}); len(got) != 0 {
		t.Errorf("UnclassifiedWriters(reads only) = %v, want none", got)
	}
}

// Surface must refuse an unpinned cluster exactly as Toolset does: it spawns
// the same subprocess with the same environment, so it inherits the same
// hazard. Ungated — the refusals happen before any process starts.
func TestSurfaceRefusesUnpinnedClusters(t *testing.T) {
	kubeconfig := fakeKubeconfig(t, "kind-sre-evals")
	for _, tc := range []struct {
		name string
		cfg  Config
	}{
		{"no context", Config{Kubeconfig: kubeconfig}},
		{"no kubeconfig", Config{Context: "kind-sre-evals"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Surface(context.Background(), tc.cfg); err == nil {
				t.Fatal("Surface accepted an unpinned config")
			}
		})
	}
	if _, err := Surface(context.Background(), Config{
		Kubeconfig: kubeconfig, Context: "some-other-cluster",
	}); err == nil {
		t.Fatal("Surface accepted a kubeconfig whose current-context is not the pinned one")
	}
}

// The live half: what the binary on this machine actually declares. Gated on
// the binary being present, because that is all it can be — its value is
// catching a lookout release that added a writer since the last capture, which
// is precisely the drift the checked-in test above cannot see.
func TestLiveSurfaceDeclaresNoUnclassifiedWriter(t *testing.T) {
	if _, err := exec.LookPath(binaryName()); err != nil {
		t.Skipf("lookout binary not found (%v)", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	surface, err := Surface(ctx, Config{
		Kubeconfig: fakeKubeconfig(t, "kind-sre-evals"),
		Context:    "kind-sre-evals",
	})
	if err != nil {
		t.Fatalf("Surface: %v", err)
	}
	if len(surface) == 0 {
		t.Fatal("live lookout advertised no tools")
	}
	if unknown := UnclassifiedWriters(surface); len(unknown) > 0 {
		t.Errorf("the lookout on this machine declares unclassified write tool(s) %v; "+
			"cmd/sre-agent will refuse to run against it", unknown)
	}

	// Annotations must survive the handshake. Every tool coming back
	// ReadOnly:false would look exactly like a lookout that annotates nothing,
	// and would refuse every run for the wrong reason.
	reads := 0
	for _, tl := range surface {
		if tl.ReadOnly {
			reads++
		}
	}
	if reads == 0 {
		t.Error("no tool came back read-only; the readOnlyHint annotation is not being read")
	}
}
