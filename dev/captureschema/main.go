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

// Command captureschema refreshes internal/lookout/tools.json from a live
// `lookout mcp` handshake.
//
// The captured surface is what the fixed eval tier presents to the model in
// place of a cluster. Run it after upgrading lookout; internal/lookout's
// TestOfflineMatchesLiveSurface fails when the two drift.
//
// Usage: go run ./dev/captureschema [-bin lookout] [-out internal/lookout/tools.json]
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sort"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func main() {
	bin := flag.String("bin", "lookout", "lookout executable")
	out := flag.String("out", "internal/lookout/tools.json", "destination file")
	flag.Parse()

	if err := run(*bin, *out); err != nil {
		log.Fatal(err)
	}
}

func run(bin, out string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	cmd := exec.Command(bin, "mcp")
	// An empty environment: listing tools needs no cluster, and this command
	// must never be the thing that touches one.
	cmd.Env = []string{}
	cmd.Stderr = os.Stderr

	client := mcp.NewClient(&mcp.Implementation{Name: "captureschema", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.CommandTransport{Command: cmd}, nil)
	if err != nil {
		return fmt.Errorf("connect to %s mcp: %w", bin, err)
	}
	defer func() { _ = sess.Close() }()

	res, err := sess.ListTools(ctx, nil)
	if err != nil {
		return fmt.Errorf("list tools: %w", err)
	}

	// Annotations are captured because readOnlyHint is a safety input, not
	// documentation: cmd/sre-agent refuses to run against a toolset that
	// declares a mutating tool, and lookout grew one (k8s_findings_diff
	// advances persisted state, so it advertises ReadOnlyHint:false). ADK's
	// mcptoolset drops annotations when it converts an MCP tool to a
	// tool.Tool, so the captured surface is where the hint is visible.
	type spec struct {
		Name        string               `json:"name"`
		Description string               `json:"description"`
		InputSchema any                  `json:"inputSchema"`
		Annotations *mcp.ToolAnnotations `json:"annotations,omitempty"`
	}
	specs := make([]spec, 0, len(res.Tools))
	for _, t := range res.Tools {
		specs = append(specs, spec{t.Name, t.Description, t.InputSchema, t.Annotations})
	}
	sort.Slice(specs, func(i, j int) bool { return specs[i].Name < specs[j].Name })

	blob, err := json.MarshalIndent(specs, "", " ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(out, append(blob, '\n'), 0o644); err != nil {
		return err
	}
	fmt.Printf("wrote %d tools to %s\n", len(specs), out)
	return nil
}
