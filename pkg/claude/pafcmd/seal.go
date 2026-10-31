package pafcmd

import (
	"bufio"
	"crypto/ed25519"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"

	"github.com/tofutools/tclaude/pkg/claude/common/config"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
	"github.com/tofutools/tclaude/pkg/paf"
	"github.com/tofutools/tclaude/pkg/paf/legacy"
	"github.com/tofutools/tclaude/pkg/paf/seal"
)

func identityPath() string { return filepath.Join(config.DataDir(), "federation", "instance.key") }

type encryptParams struct {
	File string   `descr:"PAF file to encrypt" positional:"true"`
	Out  string   `long:"out" short:"o" help:"Write the encrypted PAF here (conventionally <name>.paf.age)."`
	To   []string `long:"to" help:"A recipient: a federation peer's instance id, name or label, or an age1… recipient. Repeatable."`
}

func encryptCmd() *cobra.Command {
	return boa.CmdT[encryptParams]{
		Use:   "encrypt",
		Short: "Encrypt a PAF to named recipients with age",
		Long: "Encrypt a whole PAF with age (https://age-encryption.org) to one or more\n" +
			"recipients. A federation peer is addressed by the age key derived from its\n" +
			"instance key, so any age implementation holding that instance's identity can\n" +
			"decrypt it. This node is always a recipient, so you can read back what you\n" +
			"sent; it needs a federation identity.\n\n" +
			"  tclaude paf encrypt reviewer.paf --to lab -o reviewer.paf.age",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *encryptParams, cmd *cobra.Command, _ []string) {
			if err := runEncrypt(cmd.OutOrStdout(), p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runEncrypt(stdout io.Writer, p *encryptParams) error {
	if p.Out == "" {
		return errors.New("--out is required")
	}
	if len(p.To) == 0 {
		return errors.New("name at least one recipient with --to")
	}
	self, err := proto.LoadIdentity(identityPath())
	if err != nil {
		return fmt.Errorf("this node has no federation identity to include as a recipient: %w", err)
	}
	selfRec, err := seal.Recipient(self.Pub)
	if err != nil {
		return err
	}
	recipients := []string{selfRec}
	for _, to := range p.To {
		r, err := resolveRecipient(to)
		if err != nil {
			return err
		}
		recipients = append(recipients, r)
	}
	in, err := os.Open(p.File)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	// Only a valid PAF is encrypted, the bytes validated are the bytes
	// encrypted: both read the same descriptor.
	st, err := in.Stat()
	if err != nil {
		return err
	}
	if _, err := paf.Open(in, st.Size(), paf.Options{Purpose: paf.PurposeInspect}); err != nil {
		return fmt.Errorf("%s is not a valid PAF (export it as a PAF first): %w", p.File, err)
	}
	if err := writeAtomic(p.File, p.Out, func(w io.Writer) error {
		return seal.Encrypt(w, io.NewSectionReader(in, 0, st.Size()), recipients...)
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s for %d recipient(s) including this node\n", termSafe(p.Out), len(recipients))
	return nil
}

// resolveRecipient maps --to to an age recipient: an age1… recipient as
// is, or a trusted federation peer by exact instance id or operator-set
// label, else by an instance id prefix of at least 8 characters. The
// hub-reported name is never used: the peer or hub chooses it, so it must
// not be able to capture a PAF meant for another peer (the rule message
// addressing uses).
func resolveRecipient(to string) (string, error) {
	to = strings.TrimSpace(to)
	if strings.HasPrefix(to, "age1") {
		return to, nil
	}
	var peers []db.FederationPeer
	if _, err := os.Stat(db.DBPath()); err == nil {
		if peers, err = db.ListFederationPeers(); err != nil {
			return "", fmt.Errorf("read trusted peers: %w", err)
		}
	}
	var hits []db.FederationPeer
	for _, p := range peers {
		switch {
		case p.InstanceID == to, p.Label != "" && p.Label == to:
			return seal.Recipient(ed25519.PublicKey(p.PubKey))
		case len(to) >= 8 && (strings.HasPrefix(p.InstanceID, to) || strings.HasPrefix(p.InstanceID, proto.InstanceIDPrefix+to)):
			hits = append(hits, p)
		}
	}
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("no trusted federation peer %q; name a peer by instance id or label, or give an age1… recipient", to)
	case 1:
		return seal.Recipient(ed25519.PublicKey(hits[0].PubKey))
	}
	return "", fmt.Errorf("%q matches %d trusted peers; use the label or instance id", to, len(hits))
}

type decryptParams struct {
	File string `descr:"Encrypted PAF (.paf.age)" positional:"true"`
	Out  string `long:"out" short:"o" help:"Write the decrypted PAF here."`
}

func decryptCmd() *cobra.Command {
	return boa.CmdT[decryptParams]{
		Use:   "decrypt",
		Short: "Decrypt a PAF addressed to this node",
		Long: "Decrypt an age-encrypted PAF with this node's federation identity and verify\n" +
			"the result. validate, inspect, render and redact also read an encrypted PAF\n" +
			"directly; decrypt it to import it.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *decryptParams, cmd *cobra.Command, _ []string) {
			if err := runDecrypt(cmd.OutOrStdout(), p); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}
		},
	}.ToCobra()
}

func runDecrypt(stdout io.Writer, p *decryptParams) error {
	if p.Out == "" {
		return errors.New("--out is required")
	}
	in, err := os.Open(p.File)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	r, err := decryptReader(in)
	if err != nil {
		return err
	}
	// The plaintext is verified before it is renamed into place, so a
	// failure leaves nothing at --out.
	if err := writeAtomic(p.File, p.Out, func(w io.Writer) error {
		f := w.(*os.File)
		n, err := io.Copy(f, r)
		if err != nil {
			return err
		}
		if _, err := paf.Open(f, n, paf.Options{Purpose: paf.PurposeInspect}); err != nil {
			return fmt.Errorf("decrypted, but the result is not a valid PAF: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "wrote %s\n", termSafe(p.Out))
	return nil
}

// decryptReader decrypts an age file with this node's identity.
func decryptReader(r io.Reader) (io.Reader, error) {
	id, err := proto.LoadIdentity(identityPath())
	if err != nil {
		return nil, fmt.Errorf("decrypting needs this node's federation identity: %w", err)
	}
	ageID, err := seal.Identity(id.Priv)
	if err != nil {
		return nil, err
	}
	return seal.Decrypt(r, ageID)
}

// openMaybeEncrypted opens a PAF, legacy bundle or encrypted PAF. An
// encrypted file is decrypted to a private temporary file, which is
// unlinked once open; the archive reads it through the open descriptor.
func openMaybeEncrypted(path string, opts paf.Options) (*legacy.Opened, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	head, _ := bufio.NewReader(f).Peek(64)
	if !seal.IsEncrypted(head) {
		_ = f.Close()
		return legacy.OpenFile(path, "", producer(), opts)
	}
	defer func() { _ = f.Close() }()
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	r, err := decryptReader(f)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", ".paf-decrypted-")
	if err != nil {
		return nil, err
	}
	defer func() { _ = tmp.Close(); _ = os.Remove(tmp.Name()) }()
	if _, err := io.Copy(tmp, r); err != nil {
		return nil, err
	}
	return legacy.OpenFile(tmp.Name(), "", producer(), opts)
}

// writeAtomic writes out through a temporary file beside it, refusing to
// write over in.
func writeAtomic(in, out string, write func(io.Writer) error) error {
	if st, err := os.Stat(in); err == nil {
		if dst, err := os.Stat(out); err == nil && os.SameFile(st, dst) {
			return errors.New("--out is the input file")
		}
	}
	f, err := os.CreateTemp(filepath.Dir(out), ".paf-")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	err = write(f)
	if err == nil {
		err = f.Chmod(0o600)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp, out)
}
