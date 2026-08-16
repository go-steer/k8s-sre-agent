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

// Package monitor turns a stateless health check into an incident tracker:
// stable finding identity and run-to-run diffing.
//
// Ported from the upstream Python project's monitor_state.py. Everything here
// is a pure function over plain data so it is testable without a cluster or a
// database.
//
// Why it exists upstream: the scheduled check had no memory, so a cluster
// problem that persisted for a week was re-reported identically every
// interval. That is what made the scheduler too noisy to leave enabled.
package monitor

import (
	"regexp"
	"strings"

	"github.com/go-steer/k8s-sre-agent/internal/schema"
)

// Kubernetes generates the random part of a pod name from a deliberately
// vowel-free alphabet (k8s.io/apimachinery/pkg/util/rand: "bcdfghjklmnpqrstvwxz"
// plus "2456789"), so a word like "redis" or "cache" can never be mistaken for
// a generated suffix. Matching that exact alphabet strips the churn without
// eating meaningful name segments.
const randAlphabet = `[bcdfghjklmnpqrstvwxz2456789]`

var (
	// Deployment pod: <base>-<replicaset-hash>-<pod-suffix>
	podDeploy = regexp.MustCompile(`^(.+?)-` + randAlphabet + `{5,10}-` + randAlphabet + `{5}$`)
	// DaemonSet / Job / bare-ReplicaSet pod: <base>-<pod-suffix>
	podShort = regexp.MustCompile(`^(.+?)-` + randAlphabet + `{5}$`)

	nonAlnum = regexp.MustCompile(`[^a-z0-9]+`)
)

// NormalizeResourceName strips the generated suffix from a pod name so
// restarts keep one identity.
//
// A CrashLoopBackOff pod gets a new random name on every restart.
// Fingerprinting the raw name would make one ongoing incident look like an
// endless stream of new-and-resolved pairs, so pod names collapse to their
// controller-stable base:
//
//	api-7f9d8b6c4-xk2p1  -> api
//	log-shipper-4tzvn    -> log-shipper
//
// StatefulSet pods are already stable ("web-0") and the ordinal is part of the
// object's identity, so they are left alone — a digits-only ordinal cannot
// match the random alphabet above. Non-pod kinds are returned unchanged.
func NormalizeResourceName(kind, name string) string {
	if name == "" {
		return ""
	}
	switch strings.ToLower(kind) {
	case "pod", "pods":
	default:
		return name
	}
	if m := podDeploy.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	if m := podShort.FindStringSubmatch(name); m != nil {
		return m[1]
	}
	return name
}

// slug collapses free text to a coarse, stable-ish token.
func slug(text string) string {
	s := nonAlnum.ReplaceAllString(strings.ToLower(text), "-")
	s = strings.Trim(s, "-")
	if len(s) > 60 {
		s = s[:60]
	}
	return s
}

// Fingerprint derives a stable identity for a Finding.
//
// Built from the constrained identity fields (namespace / kind / normalized
// name / reason) rather than the free-text title, which the model rewords
// between runs. Findings carrying no identity fields at all fall back to a
// slug of the title — less stable, but it never fails.
//
// This is what lets one ongoing incident report as "ongoing 6h · seen 12x"
// instead of as a fresh alert every interval.
func Fingerprint(f schema.Finding) string {
	ns := strings.ToLower(strings.TrimSpace(f.Namespace))
	if ns == "" {
		ns = "-"
	}
	kind := strings.TrimSpace(f.Kind)
	name := NormalizeResourceName(kind, strings.TrimSpace(f.ResourceName))
	reason := strings.TrimSpace(f.Reason)

	if kind == "" && name == "" && reason == "" {
		return ns + "/~/" + slug(f.Title)
	}
	return ns + "/" + strings.ToLower(kind) + "/" + strings.ToLower(name) + ":" + slug(reason)
}
