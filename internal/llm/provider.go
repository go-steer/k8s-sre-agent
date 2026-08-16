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

// Package llm resolves the Claude models this agent runs on.
//
// Model access is Anthropic-on-Vertex via Application Default
// Credentials, wrapped by mast's provider so the rest of the repo talks
// only to ADK's model.LLM interface.
package llm

import (
	"context"

	"github.com/go-steer/mast/pkg/providers/anthropic"
	adkmodel "google.golang.org/adk/v2/model"
)

// Model IDs, mirroring upstream's config.py split: a strong model drives
// the main loop, a cheap one drives the read-only subagents.
//
// These are *Vertex publication names*, which differ from the first-party
// API IDs in one specific way: a current-generation model is published
// under its bare ID, while a dated snapshot uses an "@" version separator
// rather than the "-YYYYMMDD" suffix the first-party API uses. Upstream's
// SUBAGENT_MODEL is "claude-haiku-4-5-20251001"; on Vertex the same model
// is "claude-haiku-4-5@20251001". The SDK plugs the string into the Vertex
// URL path verbatim, so getting this wrong is a 404, not a fallback.
const (
	// Main is the model for the orchestrating agent. Upstream runs
	// claude-sonnet-4-6 here; we run Sonnet 5, the current equivalent tier.
	Main = "claude-sonnet-5"

	// Subagent is the model for the read-only diagnostic specialists,
	// matching upstream's SUBAGENT_MODEL.
	Subagent = "claude-haiku-4-5@20251001"
)

// Provider constructs the Vertex-backed Anthropic provider.
//
// Project and region come from the environment (ANTHROPIC_VERTEX_PROJECT_ID
// or GOOGLE_CLOUD_PROJECT; CLOUD_ML_REGION or GOOGLE_CLOUD_LOCATION). We
// pass no explicit region: "global" is Vertex's recommended setting for
// Claude and is what the operator env sets, whereas mast's fallback is the
// narrower us-east5. Overriding here would silently pin us to one region.
//
// CacheSystem is on because the SRE system prompt is long, fixed, and
// re-sent on every monitoring cycle — the case prompt caching exists for.
func Provider(ctx context.Context) (*anthropic.Provider, error) {
	return anthropic.NewVertex(ctx, anthropic.VertexOptions{CacheSystem: true})
}

// Models resolves both tiers from one provider.
func Models(ctx context.Context) (main, subagent adkmodel.LLM, err error) {
	p, err := Provider(ctx)
	if err != nil {
		return nil, nil, err
	}
	if main, err = p.Model(ctx, Main); err != nil {
		return nil, nil, err
	}
	if subagent, err = p.Model(ctx, Subagent); err != nil {
		return nil, nil, err
	}
	return main, subagent, nil
}
