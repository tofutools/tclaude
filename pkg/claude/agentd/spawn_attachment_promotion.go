package agentd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/convops"
	"github.com/tofutools/tclaude/pkg/claude/common/db"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"github.com/tofutools/tclaude/pkg/claude/session"
	tclcommon "github.com/tofutools/tclaude/pkg/common"
)

// promoteSpawnAttachments copies daemon-staged uploads into the exact private
// path the new tclaude-layer session will see. Copying preserves the staging
// batch for a dashboard retry; the returned cleanup removes only this launch's
// private batch and then its root if still empty.
func promoteSpawnAttachments(
	sessionID string,
	attachments []string,
) ([]string, func(), error) {
	if len(attachments) == 0 {
		return nil, func() {}, nil
	}
	stagingBase, err := filepath.EvalSymlinks(spawnAttachmentsBaseDir())
	if err != nil {
		return nil, func() {}, fmt.Errorf("resolve daemon attachment staging root: %w", err)
	}
	privateRoot, privateRootCreated, err :=
		tclcommon.PrepareSpawnAttachmentsPrivateDir(sessionID)
	if err != nil {
		return nil, func() {}, err
	}
	batchDir := filepath.Join(privateRoot, convops.GenerateUUID())
	if err := os.Mkdir(batchDir, 0o700); err != nil {
		if privateRootCreated {
			_ = os.Remove(privateRoot)
		}
		return nil, func() {}, fmt.Errorf("create private attachment batch: %w", err)
	}
	cleanup := func() {
		_ = os.RemoveAll(batchDir)
		if privateRootCreated {
			// Never remove/recreate a root that acquired another live batch.
			_ = os.Remove(privateRoot)
		}
	}

	promoted := make([]string, 0, len(attachments))
	used := map[string]bool{}
	for i, stagedPath := range attachments {
		source, err := openDaemonStagedAttachment(stagingBase, stagedPath)
		if err != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf("attachment %d: %w", i, err)
		}
		name := uniqueAttachmentName(sanitizeAttachmentFilename(filepath.Base(stagedPath)), used)
		destination := filepath.Join(batchDir, name)
		dest, createErr := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if createErr != nil {
			_ = source.Close()
			cleanup()
			return nil, func() {}, fmt.Errorf("attachment %d: create private copy: %w", i, createErr)
		}
		_, copyErr := io.Copy(dest, io.LimitReader(source, spawnAttachmentMaxFileBytes+1))
		sourceCloseErr := source.Close()
		destCloseErr := dest.Close()
		if copyErr != nil || sourceCloseErr != nil || destCloseErr != nil {
			cleanup()
			return nil, func() {}, fmt.Errorf(
				"attachment %d: copy private attachment: %v",
				i,
				firstNonNil(copyErr, sourceCloseErr, destCloseErr),
			)
		}
		info, statErr := os.Stat(destination)
		if statErr != nil || info.Size() > spawnAttachmentMaxFileBytes {
			cleanup()
			return nil, func() {}, fmt.Errorf(
				"attachment %d: private copy exceeds upload limit or is unreadable",
				i,
			)
		}
		used[name] = true
		promoted = append(promoted, destination)
	}
	return promoted, cleanup, nil
}

func openDaemonStagedAttachment(
	resolvedStagingBase string,
	rawPath string,
) (*os.File, error) {
	rawPath = strings.TrimSpace(rawPath)
	issuedInfo, issued := daemonStagedAttachmentIdentity(rawPath)
	if !issued {
		return nil, fmt.Errorf("path was not issued by the daemon attachment endpoint")
	}
	if !filepath.IsAbs(rawPath) || filepath.Clean(rawPath) != rawPath {
		return nil, fmt.Errorf("path is not a clean absolute daemon-staged path")
	}
	configuredBase := filepath.Clean(spawnAttachmentsBaseDir())
	relative, err := filepath.Rel(configuredBase, rawPath)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("path is outside the daemon attachment staging root")
	}
	parts := strings.Split(relative, string(filepath.Separator))
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return nil, fmt.Errorf("path is not a daemon-issued batch file")
	}
	current := configuredBase
	for i, part := range parts {
		current = filepath.Join(current, part)
		info, lstatErr := os.Lstat(current)
		if lstatErr != nil {
			return nil, fmt.Errorf("inspect staged path: %w", lstatErr)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("staged path contains a symlink")
		}
		if i == 0 && !info.IsDir() {
			return nil, fmt.Errorf("staged batch is not a directory")
		}
		if i == 1 && !info.Mode().IsRegular() {
			return nil, fmt.Errorf("staged attachment is not a regular file")
		}
	}

	resolvedPath, err := filepath.EvalSymlinks(rawPath)
	if err != nil || !sandboxpolicy.PathContainsOrEqual(resolvedStagingBase, resolvedPath) ||
		resolvedPath == resolvedStagingBase {
		return nil, fmt.Errorf("resolved path escapes the daemon attachment staging root")
	}
	source, err := os.Open(rawPath)
	if err != nil {
		return nil, fmt.Errorf("open staged attachment: %w", err)
	}
	fdInfo, err := source.Stat()
	if err != nil || !fdInfo.Mode().IsRegular() || !os.SameFile(issuedInfo, fdInfo) {
		_ = source.Close()
		return nil, fmt.Errorf("opened staged attachment is not the daemon-issued regular file")
	}

	// Re-resolve after open, then prove the descriptor still names that
	// in-root regular file. This closes path-swap races without trusting a
	// pre-open string check as read authority.
	postResolved, evalErr := filepath.EvalSymlinks(rawPath)
	postInfo, lstatErr := os.Lstat(postResolved)
	if evalErr != nil || lstatErr != nil ||
		!sandboxpolicy.PathContainsOrEqual(resolvedStagingBase, postResolved) ||
		postResolved == resolvedStagingBase ||
		!postInfo.Mode().IsRegular() ||
		!os.SameFile(fdInfo, postInfo) {
		_ = source.Close()
		return nil, fmt.Errorf("staged attachment changed during verification")
	}
	return source, nil
}

func firstNonNil(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

// sweepStalePrivateAttachmentRoots removes expired batches and inactive,
// empty session roots. A failed liveness snapshot fails closed: without a
// reliable live set it removes no roots, so it can never disconnect a path
// already bind-mounted into a running sandbox.
func sweepStalePrivateAttachmentRoots() {
	rows, err := db.ListSessions()
	if err != nil {
		return
	}
	liveTmux, err := session.LiveTmuxSessions()
	if err != nil {
		return
	}
	liveRoots := make(map[string]bool)
	for _, row := range rows {
		implementation, normalizeErr := sandboxpolicy.NormalizeImplementation(
			row.SandboxImplementation,
		)
		if normalizeErr != nil ||
			implementation != sandboxpolicy.ImplementationTclaudeLayer {
			continue
		}
		if _, live := liveTmux[row.TmuxSession]; live && row.TmuxSession != "" {
			liveRoots[tclcommon.SpawnAttachmentsPrivateDir(row.ID)] = true
			liveRoots[tclcommon.LegacySpawnAttachmentsPrivateDir(row.ID)] = true
		}
	}
	sweepPrivateAttachmentRootsAt(time.Now(), liveRoots)
}

func sweepPrivateAttachmentRootsAt(now time.Time, liveRoots map[string]bool) {
	for _, base := range []string{
		tclcommon.SpawnAttachmentsPrivateBase(),
		tclcommon.LegacySpawnAttachmentsPrivateBase(),
	} {
		sweepPrivateAttachmentRootsBaseAt(base, now, liveRoots)
	}
}

func sweepPrivateAttachmentRootsBaseAt(base string, now time.Time, liveRoots map[string]bool) {
	roots, err := os.ReadDir(base)
	if err != nil {
		return
	}
	cutoff := now.Add(-spawnAttachmentBatchTTL)
	for _, rootEntry := range roots {
		if !rootEntry.IsDir() {
			continue
		}
		root := filepath.Join(base, rootEntry.Name())
		sweepStaleAttachmentBatches(root)
		if liveRoots[root] {
			continue
		}
		children, readErr := os.ReadDir(root)
		if readErr != nil || len(children) != 0 {
			continue
		}
		info, infoErr := rootEntry.Info()
		if infoErr != nil || info.ModTime().After(cutoff) {
			continue
		}
		// os.Remove, never RemoveAll: a concurrent upload that populated the
		// root wins with ENOTEMPTY instead of being destroyed.
		_ = os.Remove(root)
	}
}
