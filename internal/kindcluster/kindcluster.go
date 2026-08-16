// Package kindcluster manages the throwaway cluster the live eval tier runs
// against.
//
// # Why this package is paranoid
//
// The live tier injects real faults: it creates workloads that crash, exhaust
// memory, and fail to schedule. This machine has 64 kube contexts, several of
// them live GKE clusters. A fault injector that resolved the ambient
// current-context would eventually run against one of them, and the failure
// would be silent right up until it wasn't.
//
// So the pin is mechanical, not advisory, and it is enforced four ways:
//
//  1. kind writes to a kubeconfig *we* create, never ~/.kube/config. The file
//     the agent is handed physically cannot name a production cluster.
//  2. Only clusters whose names carry NamePrefix are created or deleted.
//  3. Create refuses to adopt a cluster that already exists — an existing
//     cluster of that name is by definition not one we made.
//  4. Every kubectl invocation passes both KUBECONFIG and --context, and runs
//     from an environment that does not inherit the caller's KUBECONFIG.
//
// Any one of these would usually be enough. They are all here because the
// cost of the check is a few milliseconds and the cost of the miss is a
// production incident.
package kindcluster

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
)

// NamePrefix marks a cluster as ours. Create rejects a name without it and
// Delete refuses to remove a cluster that lacks it, so no code path in this
// package can touch a cluster a human made by hand.
const NamePrefix = "sre-eval-"

// ContextPrefix is what kind prepends to a cluster name to form its context.
const ContextPrefix = "kind-"

// DefaultCreateTimeout bounds cluster creation. A kind node pulls and starts
// a full control plane; a minute is normal and five means something is wrong.
const DefaultCreateTimeout = 5 * time.Minute

// Config describes the cluster to create.
type Config struct {
	// Name is the kind cluster name. It must start with NamePrefix. Callers
	// should make it unique per run so concurrent runs cannot collide.
	Name string

	// Image is the node image (e.g. "kindest/node:v1.31.0"). Empty uses the
	// default for the installed kind.
	Image string

	// Kubeconfig is where the cluster's credentials are written. Empty creates
	// a temp file that Delete removes. It must not be an existing kubeconfig:
	// kind merges into the file it is given, and merging into a real one would
	// defeat the isolation this package exists to provide.
	Kubeconfig string

	// CreateTimeout bounds `kind create`. Zero uses DefaultCreateTimeout.
	CreateTimeout time.Duration
}

// Cluster is a live kind cluster, pinned to its own kubeconfig.
type Cluster struct {
	// Name is the kind cluster name.
	Name string
	// Context is the kube context, always ContextPrefix + Name.
	Context string
	// Kubeconfig is the isolated credential file. Pass this and Context to
	// lookout.Config; together they are the pin.
	Kubeconfig string

	ownsKubeconfig bool
}

// Create brings up a kind cluster and returns it pinned to its own kubeconfig.
//
// The caller must Delete it. Create is not idempotent by design — see the
// package comment on why adopting an existing cluster is refused rather than
// treated as success.
func Create(ctx context.Context, cfg Config) (c *Cluster, err error) {
	if err := checkName(cfg.Name); err != nil {
		return nil, err
	}
	if _, err := exec.LookPath("kind"); err != nil {
		return nil, fmt.Errorf("kindcluster: kind is not on PATH: %w", err)
	}
	if _, err := exec.LookPath("kubectl"); err != nil {
		return nil, fmt.Errorf("kindcluster: kubectl is not on PATH: %w", err)
	}

	existing, err := List(ctx)
	if err != nil {
		return nil, err
	}
	for _, name := range existing {
		if name == cfg.Name {
			return nil, fmt.Errorf("kindcluster: cluster %q already exists — refusing to adopt a cluster "+
				"this process did not create; delete it first or pick a unique name", cfg.Name)
		}
	}

	kubeconfig, owns, err := prepareKubeconfig(cfg)
	if err != nil {
		return nil, err
	}
	// A failure between here and the end leaves a half-built cluster and a
	// stray file, both of which cost real resources.
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		defer cancel()
		_ = destroy(cleanupCtx, cfg.Name)
		if owns {
			_ = os.Remove(kubeconfig)
		}
	}()

	timeout := cfg.CreateTimeout
	if timeout == 0 {
		timeout = DefaultCreateTimeout
	}
	createCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// --wait blocks until the control-plane node reports Ready. Without it
	// `kind create` returns while the node still carries its not-ready taint,
	// and the first workloads applied sit unschedulable for up to 75 seconds —
	// measured, on this machine. That delay lands inside the caller's *settle*
	// budget, where it reads as "the fault took four minutes to manifest"
	// rather than as "the cluster was not up yet". Paying it here, once, makes
	// the settle budget measure the fault.
	args := []string{
		"create", "cluster",
		"--name", cfg.Name,
		"--kubeconfig", kubeconfig,
		"--wait", "120s",
	}
	if cfg.Image != "" {
		args = append(args, "--image", cfg.Image)
	}
	if out, err := runKind(createCtx, args...); err != nil {
		return nil, fmt.Errorf("kindcluster: create %s: %w\n%s", cfg.Name, err, out)
	}

	cl := &Cluster{
		Name:           cfg.Name,
		Context:        ContextPrefix + cfg.Name,
		Kubeconfig:     kubeconfig,
		ownsKubeconfig: owns,
	}
	if err := cl.verifyIsolation(); err != nil {
		return nil, err
	}
	return cl, nil
}

// Delete tears the cluster down and removes the kubeconfig Create made.
//
// Safe to call twice, and safe to call on a context that is already cancelled:
// teardown normally runs from a defer after the run that failed, and a
// cancelled context must not be the reason a fault-injected cluster survives.
func (c *Cluster) Delete(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if err := checkName(c.Name); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()

	err := destroy(ctx, c.Name)
	if c.ownsKubeconfig {
		if rmErr := os.Remove(c.Kubeconfig); rmErr != nil && !os.IsNotExist(rmErr) && err == nil {
			err = rmErr
		}
	}
	return err
}

// LoadImage copies an image from the host daemon into the cluster's node.
//
// Fixtures use it so that a workload which is *supposed* to run does not
// depend on registry reachability at eval time. Without it a network blip
// turns a healthy control workload into an ImagePullBackOff, and the agent is
// then marked wrong for reporting a fault that genuinely exists.
func (c *Cluster) LoadImage(ctx context.Context, image string) error {
	if err := checkName(c.Name); err != nil {
		return err
	}
	if out, err := runKind(ctx, "load", "docker-image", image, "--name", c.Name); err != nil {
		return fmt.Errorf("kindcluster: load %s: %w\n%s", image, err, out)
	}
	return nil
}

// Kubectl runs kubectl against this cluster and only this cluster.
//
// Both --context and KUBECONFIG are set, and the child's environment is built
// from scratch so an inherited KUBECONFIG cannot widen what is visible.
func (c *Cluster) Kubectl(ctx context.Context, args ...string) (string, error) {
	return c.kubectl(ctx, nil, args...)
}

// Apply pipes a manifest to `kubectl apply -f -`.
func (c *Cluster) Apply(ctx context.Context, manifest string) (string, error) {
	return c.kubectl(ctx, strings.NewReader(manifest), "apply", "-f", "-")
}

// Delete removes a manifest's objects. Errors on already-absent objects are
// the caller's to ignore; fixtures are torn down with the whole cluster
// anyway, so this exists for tests that reuse one.
func (c *Cluster) DeleteManifest(ctx context.Context, manifest string) (string, error) {
	return c.kubectl(ctx, strings.NewReader(manifest), "delete", "--ignore-not-found", "-f", "-")
}

func (c *Cluster) kubectl(ctx context.Context, stdin *strings.Reader, args ...string) (string, error) {
	full := append([]string{"--context", c.Context}, args...)
	cmd := exec.CommandContext(ctx, "kubectl", full...)
	// Built from scratch, not inherited: an ambient KUBECONFIG in the caller's
	// environment would otherwise be merged with ours by kubectl, putting 64
	// contexts back within reach of a --context typo.
	cmd.Env = []string{
		"KUBECONFIG=" + c.Kubeconfig,
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
	}
	if stdin != nil {
		cmd.Stdin = stdin
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("kubectl %s: %w\n%s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// verifyIsolation asserts the kubeconfig kind wrote names our cluster and
// nothing else.
//
// This is the check that makes the other guards redundant rather than
// load-bearing: if the file the agent is handed contains exactly one context
// and that context is ours, then no bug downstream — a wrong flag, a dropped
// --context, a lookout regression — can reach another cluster, because no
// other cluster is described in the only file the child process can read.
func (c *Cluster) verifyIsolation() error {
	raw, err := os.ReadFile(c.Kubeconfig)
	if err != nil {
		return fmt.Errorf("kindcluster: read %s: %w", c.Kubeconfig, err)
	}
	text := string(raw)

	var current string
	for _, line := range strings.Split(text, "\n") {
		if rest, ok := strings.CutPrefix(line, "current-context:"); ok {
			current = strings.Trim(strings.TrimSpace(rest), `"'`)
			break
		}
	}
	if current != c.Context {
		return fmt.Errorf("kindcluster: %s has current-context %q, want %q", c.Kubeconfig, current, c.Context)
	}

	// Counted textually rather than by parsing: a context entry is
	// "- context:" under the contexts list, and any count above one means kind
	// merged into a file that already described another cluster.
	if n := strings.Count(text, "- context:"); n != 1 {
		return fmt.Errorf("kindcluster: %s describes %d contexts, want exactly 1 — "+
			"the eval kubeconfig must not be a merged one", c.Kubeconfig, n)
	}
	return nil
}

// List returns the kind clusters on this machine, ours and otherwise.
func List(ctx context.Context) ([]string, error) {
	out, err := runKind(ctx, "get", "clusters")
	if err != nil {
		// kind exits non-zero with this on a clean machine.
		if strings.Contains(out, "No kind clusters found") {
			return nil, nil
		}
		return nil, fmt.Errorf("kindcluster: list clusters: %w\n%s", err, out)
	}
	var names []string
	for _, line := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "No kind clusters") {
			names = append(names, s)
		}
	}
	return names, nil
}

// Reap deletes leftover clusters carrying NamePrefix. A run killed with
// SIGKILL leaves its cluster behind, and a stale one is both a resource leak
// and the thing Create's no-adopt rule will trip over on the next run.
func Reap(ctx context.Context) ([]string, error) {
	names, err := List(ctx)
	if err != nil {
		return nil, err
	}
	var reaped []string
	for _, n := range names {
		if !strings.HasPrefix(n, NamePrefix) {
			continue
		}
		if err := destroy(ctx, n); err != nil {
			return reaped, err
		}
		reaped = append(reaped, n)
	}
	return reaped, nil
}

// destroy is the only path to `kind delete`, and it re-checks the prefix.
// Delete and Reap both call it; centralizing the check means a future caller
// cannot forget it.
func destroy(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if out, err := runKind(ctx, "delete", "cluster", "--name", name); err != nil {
		return fmt.Errorf("kindcluster: delete %s: %w\n%s", name, err, out)
	}
	return nil
}

func checkName(name string) error {
	if !strings.HasPrefix(name, NamePrefix) {
		return fmt.Errorf("kindcluster: cluster name %q does not start with %q — "+
			"this package only manages clusters it created", name, NamePrefix)
	}
	if strings.TrimPrefix(name, NamePrefix) == "" {
		return fmt.Errorf("kindcluster: cluster name %q is only the prefix", name)
	}
	return nil
}

// prepareKubeconfig returns the path to write credentials to, and whether
// Delete should remove it.
func prepareKubeconfig(cfg Config) (path string, owns bool, err error) {
	if cfg.Kubeconfig == "" {
		f, err := os.CreateTemp("", "sre-eval-kubeconfig-*.yaml")
		if err != nil {
			return "", false, err
		}
		name := f.Name()
		if err := f.Close(); err != nil {
			return "", false, err
		}
		// kind writes the file itself; an empty one left here would merely be
		// overwritten, but removing it keeps "the file exists" meaning
		// "the cluster exists".
		if err := os.Remove(name); err != nil {
			return "", false, err
		}
		return name, true, nil
	}
	if _, err := os.Stat(cfg.Kubeconfig); err == nil {
		return "", false, fmt.Errorf("kindcluster: %s already exists — kind merges into an existing "+
			"kubeconfig, which would break the one-context isolation guarantee", cfg.Kubeconfig)
	} else if !os.IsNotExist(err) {
		return "", false, err
	}
	return cfg.Kubeconfig, false, nil
}

func runKind(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "kind", args...)
	// kind talks to the container runtime, not to a cluster, so it needs the
	// caller's environment (DOCKER_HOST and friends) — but never an inherited
	// KUBECONFIG, since --kubeconfig is what keeps the write isolated.
	cmd.Env = filterEnv(os.Environ(), "KUBECONFIG")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func filterEnv(env []string, drop ...string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		key, _, _ := strings.Cut(kv, "=")
		if slices.Contains(drop, key) {
			continue
		}
		out = append(out, kv)
	}
	return out
}
