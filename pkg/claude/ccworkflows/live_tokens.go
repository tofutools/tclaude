package ccworkflows

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// Live token accrual for in-flight runs.
//
// The journal carries no token data, and the completed-run JSON (which does)
// only exists once a run finishes. While a run is in flight, each agent's
// transcript — agent-<id>.jsonl in the run dir — is the only on-disk source of
// token usage. CC's per-agent `tokens` figure in the completed record is a
// context-size high-water mark, not a cumulative sum; the closest live proxy is
// the most recent assistant turn's total context tokens
// (input + output + cache-read + cache-creation). It is an ESTIMATE: the
// completed-run record supersedes it with CC's authoritative figure on finish.

// agentUsage is the subset of an agent-transcript turn we read for live tokens.
type agentUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

func (u agentUsage) contextTotal() int {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

type agentTranscriptLine struct {
	Message struct {
		Usage *agentUsage `json:"usage"`
	} `json:"message"`
}

// LiveAgentTokens returns a best-effort live token estimate for one in-flight
// agent: the HIGH-WATER total context tokens across the usage-bearing turns of
// its transcript (agent-<agentID>.jsonl under journalDir). The high-water (max)
// rather than the last turn is used deliberately — context size can drop turn
// to turn (cache eviction, a /compact), and a number that ticks backwards under
// the live poll reads as a bug; the max is monotonic across reads and is also
// closer to CC's own per-agent figure (itself a context high-water, not a
// cumulative sum). Returns 0 when the transcript is missing, empty, or has no
// usage yet. A partially-written final line (live append) is tolerated.
func LiveAgentTokens(journalDir, agentID string) int {
	path := filepath.Join(journalDir, "agent-"+agentID+".jsonl")
	f, err := os.Open(path)
	if err != nil {
		return 0
	}
	defer func() { _ = f.Close() }()

	br := bufio.NewReader(f)
	best := 0
	for {
		line, readErr := br.ReadString('\n')
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			var l agentTranscriptLine
			if json.Unmarshal([]byte(trimmed), &l) == nil && l.Message.Usage != nil {
				if total := l.Message.Usage.contextTotal(); total > best {
					best = total
				}
			}
		}
		if readErr != nil {
			// EOF or any read error: best-effort — return the high-water seen so
			// far (a half-written trailing line just failed to parse, above).
			return best
		}
	}
}

// enrichLiveTokens fills in each agent's live token estimate from its transcript
// for agents that have none yet (the journal-reconstructed case). Completed-run
// agents already carry CC's authoritative figure and are left untouched.
func enrichLiveTokens(journalDir string, agents []Agent) {
	for i := range agents {
		if agents[i].Tokens == 0 {
			agents[i].Tokens = LiveAgentTokens(journalDir, agents[i].ID)
		}
	}
}
