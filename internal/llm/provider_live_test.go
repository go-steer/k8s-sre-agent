package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

// EnvLive gates every test in this repo that spends money or touches a
// remote. Unset means skip: `go test ./...` must stay free and offline.
const EnvLive = "SRE_LIVE_MODEL"

// TestVertexReachable is the credential smoke test. It proves three things
// that documentation alone cannot: that ADC resolves, that region "global"
// serves Claude for this project, and that both Vertex publication names in
// provider.go are real. A wrong model ID fails here as a 404 rather than as
// a confusing runtime error deep inside the agent loop.
//
// Run with: source ~/scripts/claude-env.sh && SRE_LIVE_MODEL=1 go test ./internal/llm/ -v
func TestVertexReachable(t *testing.T) {
	if os.Getenv(EnvLive) == "" {
		t.Skipf("set %s=1 to run (makes a real Vertex call)", EnvLive)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	main, subagent, err := Models(ctx)
	if err != nil {
		t.Fatalf("resolve models: %v", err)
	}

	for _, tc := range []struct {
		tier string
		llm  adkmodel.LLM
	}{
		{"main/" + Main, main},
		{"subagent/" + Subagent, subagent},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			text, err := say(ctx, tc.llm, "Reply with the single word: pong")
			if err != nil {
				t.Fatalf("%s: %v", tc.llm.Name(), err)
			}
			if !strings.Contains(strings.ToLower(text), "pong") {
				t.Errorf("%s: unexpected reply %q", tc.llm.Name(), text)
			}
			t.Logf("%s -> %q", tc.llm.Name(), strings.TrimSpace(text))
		})
	}
}

// say issues one non-streaming turn and returns the concatenated text.
//
// Deliberately sends no temperature, top_p, top_k or thinking budget:
// Sonnet 5 rejects all of those with a 400, and Haiku 4.5 — a pre-4.6
// model — rejects the effort parameter the newer tier uses. Sending
// nothing is the only configuration both tiers accept.
func say(ctx context.Context, m adkmodel.LLM, prompt string) (string, error) {
	req := &adkmodel.LLMRequest{
		Contents: []*genai.Content{genai.NewContentFromText(prompt, genai.RoleUser)},
		Config:   &genai.GenerateContentConfig{MaxOutputTokens: 64},
	}
	var sb strings.Builder
	for resp, err := range m.GenerateContent(ctx, req, false) {
		if err != nil {
			return "", err
		}
		if resp.Content == nil {
			continue
		}
		for _, p := range resp.Content.Parts {
			sb.WriteString(p.Text)
		}
	}
	return sb.String(), nil
}
