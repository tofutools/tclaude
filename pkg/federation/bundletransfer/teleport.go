package bundletransfer

import (
	"errors"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

type TeleportHop struct {
	Offer        string    `json:"offer"`
	FromInstance string    `json:"from_instance"`
	FromAgent    string    `json:"from_agent"`
	ToInstance   string    `json:"to_instance"`
	ToGroup      string    `json:"to_group"`
	At           time.Time `json:"at"`
}

// The signed immediate peer attests this bounded provenance. Older hops are
// explanatory history, never permission or proof of a remote principal.
type TeleportIntent struct {
	Version        int           `json:"version"`
	Chain          string        `json:"chain"`
	OriginInstance string        `json:"origin_instance"`
	OriginAgent    string        `json:"origin_agent"`
	SourceAgent    string        `json:"source_agent"`
	SourceConv     string        `json:"source_conv"`
	Clone          bool          `json:"clone,omitempty"`
	Home           bool          `json:"home,omitempty"`
	Note           string        `json:"note,omitempty"`
	Credentials    string        `json:"credentials,omitempty"`
	Require        string        `json:"require,omitempty"`
	GitRef         string        `json:"git_ref,omitempty"`
	Hops           []TeleportHop `json:"hops"`
}

func ValidCredentials(mode string) bool {
	if mode == "" || mode == "local" {
		return true
	}
	ref, ok := strings.CutPrefix(mode, "proxy:")
	if !ok {
		return false
	}
	name, peer, ok := strings.Cut(ref, "@")
	if !ok || name == "" || peer == "" || len(ref) > 256 || strings.Contains(peer, "@") {
		return false
	}
	for _, c := range ref {
		allowed := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_.-@", c)
		if !allowed {
			return false
		}
	}
	return true
}
func (t *TeleportIntent) Validate() error {
	if t.Version != 1 || !proto.ValidStreamID(t.Chain) || !proto.ValidAgentRef(t.OriginAgent) || !proto.ValidAgentRef(t.SourceAgent) || len(t.SourceConv) != 36 || !proto.ValidInstanceID(t.OriginInstance) || len(t.Hops) == 0 || len(t.Hops) > 128 || len(t.Note) > 4096 || !ValidCredentials(t.Credentials) || len(t.GitRef) > 512 || len(t.Require) > 1024 {
		return errors.New("invalid teleport metadata")
	}
	if _, err := proto.ParseNodeMatch(t.Require); err != nil {
		return err
	}
	for i, h := range t.Hops {
		if !proto.ValidStreamID(h.Offer) || !proto.ValidAgentRef(h.FromAgent) || !proto.ValidInstanceID(h.FromInstance) || !proto.ValidInstanceID(h.ToInstance) || h.ToGroup == "" || len(h.ToGroup) > 256 || h.At.IsZero() || h.At.After(time.Now().Add(5*time.Minute)) {
			return errors.New("invalid teleport hop")
		}
		if i == 0 && (h.FromInstance != t.OriginInstance || h.FromAgent != t.OriginAgent) {
			return errors.New("teleport origin does not match first hop")
		}
		if i > 0 && (t.Hops[i-1].ToInstance != h.FromInstance || h.At.Before(t.Hops[i-1].At)) {
			return errors.New("teleport hop chain is discontinuous")
		}
	}
	last := t.Hops[len(t.Hops)-1]
	if last.FromAgent != t.SourceAgent {
		return errors.New("teleport source does not match last hop")
	}
	return nil
}
