package agentd

import (
	"encoding/json"
	"errors"
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
	"github.com/tofutools/tclaude/pkg/pwf"
)

// Archive formats an agent transfer can be written in.
const (
	agentArchiveBundle = "bundle"
	agentArchivePAF    = "paf"
	// agentArchiveV1 is what users ask for agent bundle v1 by.
	agentArchiveV1 = "v1"
)

// neutralContextMaxHistory is the largest native history a transfer decodes
// into a neutral context.
const neutralContextMaxHistory = 256 << 20

// pafContextRecordLimit bounds one PAF context line on import. Validating a
// line costs several times its size, so it is held below the native
// history record limit that the bundle itself enforces. Senders hold their
// context lines to the same bound.
func pafContextRecordLimit() int { return min(agentRecordLimit(), 64<<20) }

// decodeAgentArchive reads an agent bundle v1 or an agent PAF into the
// bundle model every import, share, move and teleport path uses.
func decodeAgentArchive(f *os.File) (*agentbundle.Bundle, error) {
	b, _, err := legacy.DecodeBundleFile(f, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit()}, "")
	return b, err
}

// decodeAgentOffer is decodeAgentArchive for an archive from a peer. It also
// returns the loss report the archive carries, cut to what may cross a node
// whatever the sender wrote.
func decodeAgentOffer(f *os.File) (*agentbundle.Bundle, *paf.Losses, error) {
	b, info, err := legacy.DecodeBundleFile(f, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit()}, "")
	if err != nil || info.Losses == nil {
		return b, nil, err
	}
	l := paf.CountsOnly(*info.Losses)
	return b, &l, nil
}

func decodeAgentArchiveBytes(raw []byte) (*agentbundle.Bundle, error) {
	b, _, err := legacy.DecodeBundleBytes(raw, legacy.DecodeLimits{MaxBytes: agentTransferLimit(), MaxRecordBytes: pafContextRecordLimit()}, "")
	return b, err
}

// peerSupportsPAFBundles reports whether a peer advertised a PAF version
// this instance writes for agent transfers. Other peers keep receiving
// bundle v1.
func peerSupportsPAFBundles(peer string) bool {
	raw, _, err := db.GetFederationCatalog(peer)
	var cat proto.CatalogPayload
	return err == nil && json.Unmarshal([]byte(raw), &cat) == nil && paf.NegotiateVersion(cat.PAFVersions) != ""
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
// it so a receiver without that harness can still land the agent. A signed
// transfer is leaving for a peer.
func archiveAgentPAF(b *agentbundle.Bundle, sign bool) (*os.File, error) {
	return archiveAgentPAFWith(b, sign, pafHistoryParts{LossCountsOnly: sign})
}

// pafHistoryParts selects which forms of the conversation a PAF export
// carries. By default both travel when the harness can decode its history.
type pafHistoryParts struct {
	// NoNative leaves the native history out; the neutral context must
	// then carry the conversation.
	NoNative bool
	// NoContext leaves the neutral context out.
	NoContext bool
	// AllowedFindings records a human's explicit allowance of the scan
	// findings in privacy.json.
	AllowedFindings bool
	// Snapshot and Lineage mark a snapshot capture in agent.json.
	Snapshot *paf.Snapshot
	Lineage  []paf.LineageStep
	// Handoff is the agent's own summary of its earlier work.
	Handoff string
	// LossCountsOnly keeps free text out of the loss report, for a PAF
	// leaving this node.
	LossCountsOnly bool
	// Workflows are carried beside the agent, linked to it.
	Workflows []pwf.Bundle
}

func archiveAgentPAFWith(b *agentbundle.Bundle, sign bool, parts pafHistoryParts) (*os.File, error) {
	opts := legacy.WriteOptions{Producer: paf.Producer{Name: "tclaude", Version: buildversion.AppVersion()}, MaxRecordBytes: pafContextRecordLimit(), AllowedFindings: parts.AllowedFindings, Snapshot: parts.Snapshot, Lineage: parts.Lineage, Handoff: parts.Handoff, LossCountsOnly: parts.LossCountsOnly}
	if !parts.NoContext {
		opts.Context = neutralContext(b)
	}
	if len(parts.Workflows) > 0 {
		opts.AddContent = func(w *paf.Writer, agents []string) error {
			return writeCarriedWorkflows(w, parts.Workflows, agents, true)
		}
	}
	if b.Manifest.History != nil {
		opts.EarlierLosses = storedConvLosses(b.Manifest.History.SourceConvID)
	}
	if parts.NoNative && b.Manifest.History != nil {
		if opts.Context == nil {
			return nil, errors.New("the conversation has no neutral form to carry without its native history")
		}
		opts.OmitNative = true
	}
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
	err = legacy.WriteAgentBundle(b, f, opts)
	if err != nil && opts.Context != nil && !opts.OmitNative {
		// The neutral context is optional and the native history is
		// lossless, so a context that does not fit the archive or record
		// limits is left out rather than failing the transfer.
		slog.Warn("paf: neutral context left out", "error", err)
		opts.Context = nil
		if err = f.Truncate(0); err == nil {
			if _, err = f.Seek(0, io.SeekStart); err == nil {
				err = legacy.WriteAgentBundle(b, f, opts)
			}
		}
	}
	if err != nil {
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
	// The decoded transcript is held in memory and travels beside the
	// native history, so it is carried only for histories well inside the
	// transfer limit.
	if b.Manifest.History == nil || b.Manifest.History.Bytes > min(b.Limit()/3, neutralContextMaxHistory) {
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
