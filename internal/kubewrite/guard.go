package kubewrite

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// guard holds the refusals that happen *before* a human is asked.
//
// The ordering is the point. A refused call never becomes an approval prompt,
// so the operator is never shown a command that this package was going to
// decline anyway, and never gets into the habit of approving things that do
// not run.
type guard struct {
	protected []string
	bulkMax   int
}

// Kubernetes object names are DNS subdomains: lowercase alphanumerics, dashes
// and dots, starting and ending alphanumeric. Enforcing that here is partly a
// better error message than kubectl's and mostly a syntactic guarantee — a
// value that matches cannot begin with a dash, so no argument this package
// interpolates can turn into a flag. `kubectl delete pod --all -n prod` is a
// real command and "--all" is a name-shaped string.
var (
	nameRE = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)
	// Resource types and API groups admit uppercase (Deployment) and dots
	// (deployments.apps), but the same no-leading-dash property holds.
	typeRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9.-]*$`)
	// Kubernetes quantities: 500m, 0.5, 512Mi, 20Gi, 2, 1k.
	quantityRE = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(m|k|[KMGTPE]i?)?$`)
	// A data key in a ConfigMap is more permissive than an object name.
	dataKeyRE = regexp.MustCompile(`^[-._a-zA-Z0-9]+$`)
)

const maxNameLen = 253

// refusef builds the refusal a tool returns to the model. Phrased as a
// statement of what did not happen, because the model's next move should be to
// report the refusal, not to retry the same call with the same argument.
func refusef(format string, args ...any) error {
	// errors.New over fmt.Errorf: the formatted text routinely contains a
	// manifest or a percentage, and a second pass through a format verb would
	// mangle it.
	return errors.New("refused: " + fmt.Sprintf(format, args...) + " Nothing was changed.")
}

// name validates an object name. what names the field for the error message.
func (g guard) name(what, v string) error {
	if strings.TrimSpace(v) == "" {
		return refusef("%s is empty.", what)
	}
	if len(v) > maxNameLen {
		return refusef("%s %q is longer than %d characters.", what, v, maxNameLen)
	}
	if !nameRE.MatchString(v) {
		return refusef("%s %q is not a valid Kubernetes object name "+
			"(lowercase letters, digits, '-' and '.', starting and ending alphanumeric).", what, v)
	}
	return nil
}

// namespace validates a namespace and refuses the protected ones.
func (g guard) namespace(v string) error {
	if err := g.name("namespace", v); err != nil {
		return err
	}
	if slices.Contains(g.protected, v) {
		return refusef("namespace %q is protected; changing it risks the control plane "+
			"or this agent itself. Protected namespaces are %s.",
			v, strings.Join(g.protected, ", "))
	}
	return nil
}

// resourceType validates a kubectl resource type or a plural.group.
func (g guard) resourceType(v string) error {
	if strings.TrimSpace(v) == "" {
		return refusef("resource type is empty.")
	}
	if !typeRE.MatchString(v) {
		return refusef("resource type %q is not a valid Kubernetes resource type.", v)
	}
	return nil
}

// quantity validates a resource quantity such as 500m or 512Mi.
func (g guard) quantity(what, v string) error {
	if !quantityRE.MatchString(v) {
		return refusef("%s %q is not a Kubernetes quantity (e.g. 500m, 0.5, 512Mi, 20Gi).", what, v)
	}
	return nil
}

// dataKey validates a ConfigMap key.
func (g guard) dataKey(v string) error {
	if !dataKeyRE.MatchString(v) {
		return refusef("ConfigMap key %q may only contain letters, digits, '-', '_' and '.'.", v)
	}
	return nil
}

// replicas bounds a scale. See MaxReplicas for why a bound exists at all.
func (g guard) replicas(n int) error {
	if n < 0 {
		return refusef("replica count %d is negative.", n)
	}
	if n > MaxReplicas {
		return refusef("replica count %d exceeds the %d-replica ceiling this agent will "+
			"request. If that is genuinely the intent, scale it by hand — an approval "+
			"prompt is not a good place to notice an extra digit.", n, MaxReplicas)
	}
	return nil
}

// bulk bounds a batch. Upstream's rationale, kept verbatim in spirit: a single
// approval should not act on more objects than a person can read.
func (g guard) bulk(n int) error {
	if n == 0 {
		return refusef("no targets were given.")
	}
	if n > g.bulkMax {
		return refusef("%d targets exceeds the %d-target ceiling for one approval. "+
			"Split the request into smaller batches so each one can actually be reviewed.",
			n, g.bulkMax)
	}
	return nil
}
