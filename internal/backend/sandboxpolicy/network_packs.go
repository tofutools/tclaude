package sandboxpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/model"
)

// NetworkPack is a release-owned authoring convenience, not a connectivity or
// enforcement certificate. ContentHash identifies exactly the displayed entries;
// launch materialization must pin the expanded content rather than trust an ID.
type NetworkPack struct {
	ID          string
	Label       string
	Entries     []model.SandboxDestination
	ContentHash string
}

// These are the retained legacy destination sets. They are deliberately narrow;
// no unobserved provider endpoints or implicit subdomains are added here.
var networkPacks = []NetworkPack{
	{ID: "net-local", Label: "Local access", Entries: []model.SandboxDestination{{Loopback: true}}},
	{ID: "net-anthropic", Label: "Anthropic API", Entries: []model.SandboxDestination{{Domain: "api.anthropic.com", Ports: []uint16{443}}}},
	{ID: "net-openai-codex", Label: "OpenAI API", Entries: []model.SandboxDestination{{Domain: "api.openai.com", Ports: []uint16{443}}}},
	{ID: "net-openai-chatgpt", Label: "ChatGPT (Codex sign-in)", Entries: []model.SandboxDestination{{Domain: "chatgpt.com", Ports: []uint16{443}}, {Domain: "auth.openai.com", Ports: []uint16{443}}}},
	{ID: "net-github-copilot", Label: "GitHub Copilot (first-party)", Entries: []model.SandboxDestination{{Domain: "api.githubcopilot.com", Ports: []uint16{443}}, {Domain: "api.individual.githubcopilot.com", Ports: []uint16{443}}, {Domain: "api.github.com", Ports: []uint16{443}}}},
	{ID: "net-github", Label: "GitHub essentials", Entries: []model.SandboxDestination{{Domain: "github.com"}, {Domain: "api.github.com"}, {Domain: "codeload.github.com"}}},
	{ID: "net-go-modules", Label: "Public Go modules", Entries: []model.SandboxDestination{{Domain: "proxy.golang.org"}, {Domain: "sum.golang.org"}}},
	{ID: "net-npm", Label: "Public npm registry", Entries: []model.SandboxDestination{{Domain: "registry.npmjs.org"}}},
}

func NetworkPackCatalog() []NetworkPack {
	out := slices.Clone(networkPacks)
	for i := range out {
		out[i].Entries = slices.Clone(out[i].Entries)
		for j := range out[i].Entries {
			out[i].Entries[j].Ports = slices.Clone(out[i].Entries[j].Ports)
		}
		// This fixed DTO contains only strings, booleans and integer slices.
		encoded, _ := json.Marshal(out[i].Entries)
		digest := sha256.Sum256(encoded)
		out[i].ContentHash = hex.EncodeToString(digest[:])
	}
	return out
}
