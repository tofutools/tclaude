package agent

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// export.go is the agent face of the per-agent "📋 summary…" export
// (JOH-265). The dashboard creates an export job and nudges this agent's
// pane to run `export show`; the agent produces its file(s) and delivers
// them with `export submit`. Both are thin clients over the daemon's
// /v1/export-jobs endpoints, which gate each call on owning the job.

// maxExportArtifactBytes mirrors the daemon's per-artifact cap (see
// agentd/export.go) so `submit` can reject an oversize payload locally instead
// of streaming it only to be 413'd. Keep the two in sync.
const maxExportArtifactBytes = 256 << 20 // 256 MiB

var errExportArtifactTooLarge = errors.New("artifact exceeds the 256 MiB limit")

func exportCmd() *cobra.Command {
	return boa.CmdT[struct{}]{
		Use:   "export",
		Short: "Deliver a shareable export the human requested from the dashboard",
		Long: "Respond to a dashboard export request (the \"📋 summary…\" action). " +
			"When the human asks for an export, the daemon nudges your pane with a " +
			"request id; run `tclaude agent export show <id>` to read what to produce, " +
			"then `tclaude agent export submit <id> <files…>` to deliver it. The " +
			"dashboard shows a spinner until your files arrive, then downloads them " +
			"(multiple files are zipped automatically).",
		ParamEnrich: common.DefaultParamEnricher(),
		SubCmds: []*cobra.Command{
			exportShowCmd(),
			exportSubmitCmd(),
		},
	}.ToCobra()
}

// --- export show ---

type exportShowParams struct {
	Job  string `pos:"true" name:"job" help:"The export request id from the nudge."`
	JSON bool   `long:"json" help:"Output the brief as JSON."`
}

func exportShowCmd() *cobra.Command {
	return boa.CmdT[exportShowParams]{
		Use:   "show",
		Short: "Show what a dashboard export request wants you to produce",
		Long: "Fetch the brief for an export request the human made from the dashboard: " +
			"the title, the format preset, and the free-text instructions. Fetching it " +
			"tells the dashboard you have picked up the request. Then produce the file(s) " +
			"and deliver them with `tclaude agent export submit <id> <files…>`.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *exportShowParams, _ *cobra.Command, _ []string) {
			os.Exit(runExportShow(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExportShow(p *exportShowParams, stdout, stderr io.Writer) int {
	id, rc := parseExportJobID(p.Job, stderr)
	if rc != rcOK {
		return rc
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	var resp struct {
		ID           int64  `json:"id"`
		ConvID       string `json:"conv_id"`
		Status       string `json:"status"`
		Title        string `json:"title"`
		Instructions string `json:"instructions"`
		Preset       string `json:"preset"`
		SubmitHint   string `json:"submit_hint"`
	}
	if err := DaemonGet("/v1/export-jobs/"+strconv.FormatInt(id, 10), &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}

	if p.JSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(resp); err != nil {
			return rcIOFailure
		}
		return rcOK
	}

	fmt.Fprintf(stdout, "Export request #%d  (status: %s)\n", resp.ID, resp.Status)
	if resp.Title != "" {
		fmt.Fprintf(stdout, "Title:  %s\n", resp.Title)
	}
	if resp.Preset != "" {
		fmt.Fprintf(stdout, "Format: %s\n", resp.Preset)
	}
	fmt.Fprintln(stdout)
	if strings.TrimSpace(resp.Instructions) != "" {
		fmt.Fprintln(stdout, "The human asked you to produce a shareable export of this conversation,")
		fmt.Fprintln(stdout, "with these instructions:")
		fmt.Fprintln(stdout, "------------------------------------------------------------")
		fmt.Fprintln(stdout, resp.Instructions)
		fmt.Fprintln(stdout, "------------------------------------------------------------")
	} else {
		fmt.Fprintln(stdout, "The human asked you to produce a shareable export of this conversation")
		fmt.Fprintln(stdout, "(no extra instructions — use your judgement to make it clear and useful).")
	}
	fmt.Fprintln(stdout)
	hint := resp.SubmitHint
	if hint == "" {
		hint = fmt.Sprintf("tclaude agent export submit %d <file> [more files…]", resp.ID)
	}
	fmt.Fprintln(stdout, "When the file(s) are ready, deliver them with:")
	fmt.Fprintf(stdout, "  %s\n", hint)
	fmt.Fprintln(stdout, "Multiple files are zipped automatically. Use --name to set the download filename.")
	return rcOK
}

// --- export submit ---

type exportSubmitParams struct {
	Job   string   `pos:"true" name:"job" help:"The export request id from the nudge / export show."`
	Files []string `pos:"true" name:"files" help:"One or more files to deliver. Multiple files are zipped automatically."`
	Name  string   `long:"name" short:"n" optional:"true" help:"Override the download filename the human gets (e.g. 'research-summary.zip')."`
}

func exportSubmitCmd() *cobra.Command {
	return boa.CmdT[exportSubmitParams]{
		Use:   "submit",
		Short: "Deliver the export file(s) back to the dashboard",
		Long: "Upload the file(s) you produced for a dashboard export request. A single " +
			"file is delivered as-is (keeping its name); multiple files are zipped into " +
			"one archive automatically. The dashboard removes its spinner and downloads " +
			"the result. Use --name to choose the download filename.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *exportSubmitParams, _ *cobra.Command, _ []string) {
			os.Exit(runExportSubmit(p, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runExportSubmit(p *exportSubmitParams, stdout, stderr io.Writer) int {
	id, rc := parseExportJobID(p.Job, stderr)
	if rc != rcOK {
		return rc
	}
	if len(p.Files) == 0 {
		fmt.Fprintln(stderr, "Error: at least one file is required")
		return rcInvalidArg
	}
	// Check the daemon is reachable before doing the (potentially large) read
	// + zip — no point building an artifact we can't deliver.
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	data, name, contentType, rc := buildExportArtifact(p.Files, p.Name, stderr)
	if rc != rcOK {
		return rc
	}
	// Reject oversize artifacts locally rather than streaming the whole thing
	// only for the daemon to 413 it. Mirrors the daemon's cap (export.go).
	if len(data) > maxExportArtifactBytes {
		fmt.Fprintf(stderr, "Error: artifact is %s, over the %d MiB limit\n",
			humanBytes(len(data)), maxExportArtifactBytes>>20)
		return rcInvalidArg
	}

	path := "/v1/export-jobs/" + strconv.FormatInt(id, 10) + "/artifact?name=" + url.QueryEscape(name)
	var resp struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
		Name   string `json:"name"`
		Size   int64  `json:"size"`
	}
	if err := DaemonPostRaw(path, contentType, data, &resp); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Delivered export #%d: %s (%s). The dashboard can now download it.\n",
		resp.ID, resp.Name, humanBytes(int(resp.Size)))
	return rcOK
}

// buildExportArtifact reads the given paths and produces the upload payload:
// a single file is sent as-is; directories and path sets become zip archives.
// nameOverride, when set, becomes the download filename.
func buildExportArtifact(files []string, nameOverride string, stderr io.Writer) ([]byte, string, string, int) {
	// Validate every path up front so a bad path fails before any upload.
	infos := make([]os.FileInfo, len(files))
	for i, f := range files {
		info, err := os.Lstat(f)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, "", "", rcInvalidArg
		}
		if info.Mode()&os.ModeSymlink != 0 {
			fmt.Fprintf(stderr, "Error: %q is a symlink — attach its resolved file or directory explicitly\n", f)
			return nil, "", "", rcInvalidArg
		}
		if !info.IsDir() && !info.Mode().IsRegular() {
			fmt.Fprintf(stderr, "Error: %q is not a regular file or directory\n", f)
			return nil, "", "", rcInvalidArg
		}
		infos[i] = info
	}

	if len(files) == 1 && !infos[0].IsDir() {
		data, err := readFileCapped(files[0])
		if err != nil {
			if errors.Is(err, errExportArtifactTooLarge) {
				fmt.Fprintf(stderr, "Error: attachment is over the %d MiB limit\n", maxExportArtifactBytes>>20)
				return nil, "", "", rcInvalidArg
			}
			fmt.Fprintf(stderr, "Error: reading %q: %v\n", files[0], err)
			return nil, "", "", rcIOFailure
		}
		name := strings.TrimSpace(nameOverride)
		if name == "" {
			name = filepath.Base(files[0])
		}
		return data, name, contentTypeForName(name), rcOK
	}

	data, err := zipPaths(files)
	if err != nil {
		if errors.Is(err, errExportArtifactTooLarge) {
			fmt.Fprintf(stderr, "Error: archive is over the %d MiB limit\n", maxExportArtifactBytes>>20)
			return nil, "", "", rcInvalidArg
		}
		fmt.Fprintf(stderr, "Error: building zip: %v\n", err)
		return nil, "", "", rcIOFailure
	}
	name := strings.TrimSpace(nameOverride)
	if name == "" {
		if len(files) == 1 && infos[0].IsDir() {
			name = filepath.Base(filepath.Clean(files[0])) + ".zip"
		} else {
			name = "export.zip"
		}
	}
	return data, name, "application/zip", rcOK
}

// zipFiles deflates the given files into an in-memory .zip. Entries are named
// by base name; a collision is disambiguated with a numeric suffix so two
// inputs sharing a base name don't overwrite each other in the archive.
func zipFiles(files []string) ([]byte, error) {
	return zipPaths(files)
}

// zipPaths packages files and directories. Directory symlinks are skipped:
// an artifact should contain the selected tree, not silently escape it.
func zipPaths(paths []string) ([]byte, error) {
	buf := &cappedArtifactBuffer{limit: maxExportArtifactBytes}
	zw := zip.NewWriter(buf)
	seen := make(map[string]int)
	var uncompressedBytes int64
	for _, root := range paths {
		info, err := os.Lstat(root)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("%q is a symlink", root)
		}
		archiveRoot := uniqueZipName(safeZipComponent(filepath.Base(filepath.Clean(root))), seen)
		if !info.IsDir() {
			remaining := int64(maxExportArtifactBytes) - uncompressedBytes
			if info.Size() > remaining {
				return nil, errExportArtifactTooLarge
			}
			copied, err := addZipFile(zw, root, archiveRoot, remaining)
			uncompressedBytes += copied
			if err != nil {
				return nil, err
			}
			continue
		}
		err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				if entry.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("%q is not a regular file", path)
			}
			remaining := int64(maxExportArtifactBytes) - uncompressedBytes
			if info.Size() > remaining {
				return errExportArtifactTooLarge
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			entryName, err := safeZipEntry(archiveRoot, rel)
			if err != nil {
				return err
			}
			copied, err := addZipFile(zw, path, entryName, remaining)
			uncompressedBytes += copied
			return err
		})
		if err != nil {
			return nil, fmt.Errorf("archiving %q: %w", root, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func addZipFile(zw *zip.Writer, path, entry string, budget int64) (int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("opening %q: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if info, err := f.Stat(); err != nil {
		return 0, err
	} else if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("%q is not a regular file", path)
	} else if info.Size() > budget {
		return 0, errExportArtifactTooLarge
	}
	w, err := zw.CreateHeader(&zip.FileHeader{Name: entry, Method: zip.Deflate})
	if err != nil {
		return 0, err
	}
	copied, err := copyZipFileCapped(w, f, budget)
	return copied, err
}

func copyZipFileCapped(dst io.Writer, src io.Reader, budget int64) (int64, error) {
	copied, err := io.Copy(dst, io.LimitReader(src, budget+1))
	if err != nil {
		return copied, err
	}
	if copied > budget {
		return copied, errExportArtifactTooLarge
	}
	return copied, nil
}

func readFileCapped(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxExportArtifactBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxExportArtifactBytes {
		return nil, errExportArtifactTooLarge
	}
	return data, nil
}

type cappedArtifactBuffer struct {
	buf   bytes.Buffer
	limit int
}

func (b *cappedArtifactBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		return 0, errExportArtifactTooLarge
	}
	if len(p) > remaining {
		n, _ := b.buf.Write(p[:remaining])
		return n, errExportArtifactTooLarge
	}
	return b.buf.Write(p)
}

func (b *cappedArtifactBuffer) Bytes() []byte { return b.buf.Bytes() }

func safeZipComponent(name string) string {
	name = strings.TrimLeft(strings.ReplaceAll(name, `\`, "_"), "/")
	if name == "" || name == "." || name == ".." {
		return "artifact"
	}
	return name
}

func safeZipEntry(root, rel string) (string, error) {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if part == "" || part == "." || part == ".." {
			return "", fmt.Errorf("unsafe archive path %q", rel)
		}
		parts[i] = safeZipComponent(part)
	}
	entry := pathpkg.Join(safeZipComponent(root), strings.Join(parts, "/"))
	if entry == "." || entry == ".." || strings.HasPrefix(entry, "/") || strings.HasPrefix(entry, "../") {
		return "", fmt.Errorf("unsafe archive path %q", entry)
	}
	return entry, nil
}

// uniqueZipName returns base, or base with a "-N" suffix before the extension
// if base was already used, recording the chosen name in seen.
func uniqueZipName(base string, seen map[string]int) string {
	if seen[base] == 0 {
		seen[base] = 1
		return base
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	for {
		n := seen[base]
		seen[base] = n + 1
		candidate := fmt.Sprintf("%s-%d%s", stem, n, ext)
		if seen[candidate] == 0 {
			seen[candidate] = 1
			return candidate
		}
	}
}

// contentTypeForName guesses a MIME type from a filename's extension, with a
// markdown special-case (not in Go's built-in table) and an octet-stream
// fallback. The browser download is forced via Content-Disposition regardless,
// so this only affects how a re-opened artifact is interpreted.
func contentTypeForName(name string) string {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".md", ".markdown":
		return "text/markdown; charset=utf-8"
	case ".txt", ".log":
		return "text/plain; charset=utf-8"
	case ".zip":
		return "application/zip"
	}
	if ct := mime.TypeByExtension(filepath.Ext(name)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// parseExportJobID parses a positive export job id, writing a usage error and
// returning rcInvalidArg on failure.
func parseExportJobID(s string, stderr io.Writer) (int64, int) {
	id, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || id <= 0 {
		fmt.Fprintf(stderr, "Error: %q is not a valid export request id\n", s)
		return 0, rcInvalidArg
	}
	return id, rcOK
}
