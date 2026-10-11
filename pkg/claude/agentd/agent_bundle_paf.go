package agentd

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/tofutools/tclaude/pkg/claude/common/agentbundle"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/harness"
	"github.com/tofutools/tclaude/pkg/claude/harness/transcript"
	"github.com/tofutools/tclaude/pkg/common/buildversion"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
)

// Archive formats an agent transfer can be written in.
const (
	agentArchiveBundle = "bundle"
	agentArchivePAF    = "paf"
)

// pafContextRecordLimit bounds one PAF context line on import. Validating a
// line costs several times its size, so it is held below the native
// history record limit that the bundle itself enforces.
func pafContextRecordLimit() int { return min(agentRecordLimit(), 64<<20) }

// decodeAgentArchive reads an agent bundle v1 or an agent PAF into the
// bundle model every import, share, move and teleport path uses.
func decodeAgentArchive(f *os.File) (*agentbundle.Bundle, error) {
	b, _, err := legacy.DecodeBundleFile(f, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit()}, "")
	return b, err
}

func decodeAgentArchiveBytes(raw []byte) (*agentbundle.Bundle, error) {
	b, _, err := legacy.DecodeBundleBytes(raw, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit()}, "")
	return b, err
}

// peerSupportsPAFBundles reports whether a peer advertised that it imports
// agent transfers written as PAF. Other peers keep receiving bundle v1.
func peerSupportsPAFBundles(peer string) bool {
	raw, _, err := db.GetFederationCatalog(peer)
	var cat proto.CatalogPayload
	return err == nil && json.Unmarshal([]byte(raw), &cat) == nil && cat.PAFAgentBundles
}

// archiveAgentTransfer writes b in the requested format to a private
// temporary file positioned at its start.
func archiveAgentTransfer(b *agentbundle.Bundle, format string, sign bool) (*os.File, error) {
	if format != agentArchivePAF {
		return archiveAgentBundle(b)
	}
	return archiveAgentPAF(b, sign)
}

// archiveAgentPAF writes b as a PAF. When the bundle carries native history
// in a harness with a transcript codec, the neutral context travels beside
// it so a receiver without that harness can still land the agent.
func archiveAgentPAF(b *agentbundle.Bundle, sign bool) (*os.File, error) {
	opts := legacy.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}}
	opts.Context = neutralContext(b)
	if sign {
		if id, err := federationIdentity(); err == nil {
			opts.Keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
		} else {
			slog.Warn("paf: transfer left unsigned", "error", err)
		}
	} else if id, err := proto.LoadIdentity(FederationKeyPath()); err == nil {
		// A local export is signed only when this node already has a
		// federation identity; exporting never creates one.
		opts.Keys = []paf.SigningKey{{Key: id.Priv, KeyID: id.ID(), Signer: &paf.Signer{Kind: "instance"}}}
	}
	f, err := os.CreateTemp("", ".agent-export-archive-")
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) {
		_ = f.Close()
		_ = os.Remove(f.Name())
		return nil, err
	}
	if err := legacy.WriteAgentBundle(b, f, opts); err != nil {
		return fail(fmt.Errorf("archive exceeds or cannot be encoded within federation.agent_transfer_max_bytes=%d: %w", b.Limit(), err))
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fail(err)
	}
	return f, nil
}

// neutralContext decodes the bundle's native history into the neutral
// transcript, or returns nil when the harness has no codec or the history
// cannot be decoded. The native blob still travels either way.
func neutralContext(b *agentbundle.Bundle) *transcript.Transcript {
	if b.Manifest.History == nil {
		return nil
	}
	h, err := harness.Resolve(b.Manifest.Agent.Harness)
	if err != nil || h.Transcript == nil {
		return nil
	}
	r, err := b.OpenHistory()
	if err != nil {
		return nil
	}
	defer r.Close()
	t, err := h.Transcript.Decode(boundedHistoryReader{r, agentRecordLimit()}, b.Manifest.History.SourceConvID)
	if err != nil {
		slog.Warn("paf: neutral context not carried", "harness", h.Name, "error", err)
		return nil
	}
	return t
}
