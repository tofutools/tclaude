package agentd

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

// Board snapshot limits. The payload is the recipient-sealed PAF; it is
// split into chunks that each fit one board blob.
const (
	boardSnapshotMaxChunks = 4
	boardSnapshotMaxBytes  = boardSnapshotMaxChunks * proto.MaxBoardItemBytes
)

// boardMember is a board member as members.list reports it.
type boardMember struct {
	Instance string `json:"instance"`
	Pub      []byte `json:"pubkey"`
}

// boardSnapshotRecipients resolves --to names to board members and their
// age recipients. A name is a member's instance id, or the operator-set
// label of a trusted peer that is a member; the hub-reported name is
// never used. A member's key must match its instance id, so the hub
// cannot substitute one. This node is always a recipient, so the
// publisher can read back what it shared. The instances are sorted and
// unique.
func boardSnapshotRecipients(self *proto.Identity, members []boardMember, to []string, peers []db.FederationPeer) ([]string, []string, error) {
	if len(to) == 0 {
		return nil, nil, errors.New("name at least one recipient: a board member's instance id or peer label")
	}
	byInstance := map[string]ed25519.PublicKey{}
	for _, m := range members {
		if proto.InstanceID(m.Pub) == m.Instance {
			byInstance[m.Instance] = m.Pub
		}
	}
	chosen := map[string]ed25519.PublicKey{self.ID(): self.Pub}
	for _, name := range to {
		name = strings.TrimSpace(name)
		instance := name
		for _, p := range peers {
			if p.Label != "" && p.Label == name {
				instance = p.InstanceID
				break
			}
		}
		pub, ok := byInstance[instance]
		if !ok {
			return nil, nil, fmt.Errorf("%q is not a member of this board; name a member by instance id or trusted peer label", name)
		}
		chosen[instance] = pub
	}
	instances := make([]string, 0, len(chosen))
	for id := range chosen {
		instances = append(instances, id)
	}
	slices.Sort(instances)
	recipients := make([]string, 0, len(instances))
	for _, id := range instances {
		r, err := seal.Recipient(chosen[id])
		if err != nil {
			return nil, nil, err
		}
		recipients = append(recipients, r)
	}
	return instances, recipients, nil
}

// boardSnapshotChunk describes one chunk's outer (board-encrypted)
// ciphertext, as the signed receipt records it.
type boardSnapshotChunk struct {
	SHA256 string
	Bytes  int64
}

// boardSealOverhead is what board content encryption adds to a chunk.
const boardSealOverhead = 40

// splitBoardSnapshot splits a sealed payload into board-sized chunks.
func splitBoardSnapshot(payload []byte) ([][]byte, error) {
	if len(payload) == 0 {
		return nil, errors.New("empty snapshot payload")
	}
	if len(payload) > boardSnapshotMaxBytes {
		return nil, fmt.Errorf("the encrypted snapshot is %d bytes; a Board holds at most %d (redact, or snapshot with --no-history)", len(payload), boardSnapshotMaxBytes)
	}
	var chunks [][]byte
	for len(payload) > 0 {
		n := min(len(payload), proto.MaxBoardItemBytes)
		chunks = append(chunks, payload[:n])
		payload = payload[n:]
	}
	return chunks, nil
}

// assembleBoardSnapshot writes the payload to w, chunk by chunk in the
// signed order. Each chunk's ciphertext is checked against its signed
// digest and size before open decrypts it, and the whole against the
// signed payload digest and size. Sizes are bounded before any chunk is
// read.
func assembleBoardSnapshot(w io.Writer, want []boardSnapshotChunk, payloadSHA256 string, payloadBytes int64, read func(i int) ([]byte, error), open func(i int, ciphertext []byte) ([]byte, error)) error {
	if len(want) == 0 || len(want) > boardSnapshotMaxChunks {
		return fmt.Errorf("a snapshot has 1 to %d chunks, not %d", boardSnapshotMaxChunks, len(want))
	}
	var total int64
	for _, c := range want {
		if c.Bytes <= boardSealOverhead || c.Bytes > proto.MaxBoardItemBytes+boardSealOverhead {
			return errors.New("snapshot chunk size out of bounds")
		}
		total += c.Bytes - boardSealOverhead
	}
	if total != payloadBytes || total > boardSnapshotMaxBytes {
		return errors.New("snapshot chunk sizes do not add up to the signed payload size")
	}
	whole := sha256.New()
	var written int64
	for i, c := range want {
		cipher, err := read(i)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(cipher)
		if int64(len(cipher)) != c.Bytes || hex.EncodeToString(sum[:]) != c.SHA256 {
			return fmt.Errorf("snapshot chunk %d differs from the signed receipt", i+1)
		}
		plain, err := open(i, cipher)
		if err != nil {
			return fmt.Errorf("snapshot chunk %d: %w", i+1, err)
		}
		written += int64(len(plain))
		if written > payloadBytes {
			return errors.New("snapshot chunks exceed the signed payload size")
		}
		_, _ = whole.Write(plain)
		if _, err := w.Write(plain); err != nil {
			return err
		}
	}
	if written != payloadBytes || hex.EncodeToString(whole.Sum(nil)) != payloadSHA256 {
		return errors.New("assembled snapshot differs from the signed payload digest")
	}
	return nil
}
