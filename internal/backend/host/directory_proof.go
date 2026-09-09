package host

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/tofutools/tclaude/internal/backend/ports"
)

// DirectoryProof implements the filesystem half of the v1 caller write proof.
// It receives explicit launch paths, never an agent-reported editing directory.
type DirectoryProof struct{}

var _ ports.DirectoryWriteProof = DirectoryProof{}

func (DirectoryProof) ResolveProofDirectories(ctx context.Context, paths []string) ([]string, error) {
	dirs := make([]string, 0, len(paths))
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !filepath.IsAbs(path) {
			return nil, fmt.Errorf("proof directory must be absolute")
		}
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return nil, fmt.Errorf("resolve proof directory: %w", err)
		}
		info, err := os.Stat(resolved)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("proof path is not a directory")
		}
		dirs = append(dirs, filepath.Clean(resolved))
	}
	slices.Sort(dirs)
	return slices.Compact(dirs), nil
}

func (p DirectoryProof) ReassertProofDirectories(ctx context.Context, dirs []string) error {
	for _, dir := range dirs {
		resolved, err := p.ResolveProofDirectories(ctx, []string{dir})
		if err != nil {
			return err
		}
		if resolved[0] != dir {
			return fmt.Errorf("proof directory changed after verification")
		}
	}
	return ctx.Err()
}

func proofFilename(token string) (string, error) {
	decoded, err := hex.DecodeString(token)
	if err != nil || len(decoded) != 16 || hex.EncodeToString(decoded) != token {
		return "", fmt.Errorf("invalid directory proof token")
	}
	return ports.DirectoryWriteProofPrefix + token, nil
}

func (p DirectoryProof) VerifyProofMarkers(ctx context.Context, dirs []string, token string) error {
	filename, err := proofFilename(token)
	if err != nil {
		return err
	}
	if err := p.ReassertProofDirectories(ctx, dirs); err != nil {
		return err
	}
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return err
		}
		info, err := os.Lstat(filepath.Join(dir, filename))
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("caller write proof missing in %s", dir)
		}
	}
	return nil
}

func (p DirectoryProof) RemoveProofMarkers(ctx context.Context, dirs []string, token string) error {
	filename, err := proofFilename(token)
	if err != nil {
		return err
	}
	if err := p.ReassertProofDirectories(ctx, dirs); err != nil {
		return err
	}
	var failures []error
	for _, dir := range dirs {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(failures, err)...)
		}
		if err := os.Remove(filepath.Join(dir, filename)); err != nil && !os.IsNotExist(err) {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
