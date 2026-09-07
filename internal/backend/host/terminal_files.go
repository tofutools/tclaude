package host

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/tofutools/tclaude/internal/backend/model"
	"github.com/tofutools/tclaude/internal/backend/ports"
)

// StageTerminalFile prepares only within a provider-owned root. It publishes
// user content exclusively after the exact execution permit has been consumed.
// Published files are retained, including if a later durability check fails.
func StageTerminalFile(ctx context.Context, privateRoot string, execution model.ExecutionID, in ports.StageTerminalFileRequest) (ports.StageTerminalFileResult, error) {
	refused := ports.StageTerminalFileResult{Disposition: ports.EffectRefused}
	if !filepath.IsAbs(privateRoot) || in.Permit == nil || in.ExecutionID != execution || in.Permit.ExecutionID() != execution || in.Permit.OperationID() != in.OperationID || execution.Validate() != nil || in.OperationID.Validate() != nil || len(in.Content) == 0 || len(in.Content) > 8<<20 {
		return refused, fmt.Errorf("invalid terminal upload")
	}
	root, err := os.OpenRoot(privateRoot)
	if err != nil {
		return refused, err
	}
	defer func() { _ = root.Close() }()
	if err = root.Mkdir("uploads", 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return refused, err
	}
	info, err := root.Lstat("uploads")
	if err != nil {
		return refused, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return refused, fmt.Errorf("upload root is not a directory")
	}
	files, err := root.OpenRoot("uploads")
	if err != nil {
		return refused, err
	}
	defer func() { _ = files.Close() }()
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return refused, err
	}
	temp := ".prepare-" + hex.EncodeToString(random[:])
	file, err := files.OpenFile(temp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return refused, err
	}
	defer func() { _ = file.Close(); _ = files.Remove(temp) }()
	if _, err = file.Write(in.Content); err != nil {
		return refused, err
	}
	if err = file.Sync(); err != nil {
		return refused, err
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return refused, err
	}
	if err = file.Close(); err != nil {
		return refused, err
	}
	if err = ctx.Err(); err != nil {
		return refused, err
	}
	ext := strings.ToLower(filepath.Ext(in.Filename))
	if len(ext) > 12 || strings.IndexFunc(ext, func(r rune) bool { return r != '.' && (r < 'a' || r > 'z') && (r < '0' || r > '9') }) >= 0 {
		ext = ""
	}
	name := string(execution) + "-" + string(in.OperationID) + ext
	if err = in.Permit.Consume(ctx); err != nil {
		return refused, err
	}
	// EEXIST is never success: only application receipts may authorize a retry.
	if err = files.Link(temp, name); err != nil {
		if errors.Is(err, os.ErrExist) {
			return refused, err
		}
		return ports.StageTerminalFileResult{Disposition: ports.EffectUnknown}, err
	}
	unknown := ports.StageTerminalFileResult{Disposition: ports.EffectUnknown}
	directory, err := files.Open(".")
	if err != nil {
		return unknown, err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if err = errors.Join(syncErr, closeErr); err != nil {
		return unknown, err
	}
	// The uploads directory may have been created for this first file.
	parent, err := root.Open(".")
	if err != nil {
		return unknown, err
	}
	syncErr = parent.Sync()
	closeErr = parent.Close()
	if err = errors.Join(syncErr, closeErr); err != nil {
		return unknown, err
	}
	nativePath := filepath.Join(privateRoot, "uploads", name)
	current, err := os.Stat(nativePath)
	if err != nil {
		return unknown, err
	}
	if !os.SameFile(fileInfo, current) {
		return unknown, fmt.Errorf("upload resource changed during publication")
	}
	return ports.StageTerminalFileResult{Disposition: ports.EffectAccepted, NativePath: nativePath}, nil
}
