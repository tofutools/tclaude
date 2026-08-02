package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
)

// `tclaude agent notify-human` — send the human a notification that
// lands in the dashboard Messages tab. Permission-gated on human.notify
// (group owners always pass); the human reads it on the dashboard
// instead of scrolling the PO's busy terminal.

type notifyHumanParams struct {
	Body     string   `pos:"true" optional:"true" help:"Notification text (or use --file)."`
	Subject  string   `long:"subject" short:"s" optional:"true" help:"Optional one-line subject."`
	File     string   `long:"file" short:"f" optional:"true" help:"Read the body from this file ('-' reads stdin). Sidesteps shell quoting — best for long, multi-line, or backtick-containing bodies."`
	Attach   []string `long:"attach" short:"a" optional:"true" help:"Publish a file or directory for the human to download. Repeat for several paths; a few files arrive as separate downloads, a large set or a directory as one zip."`
	Name     string   `long:"name" optional:"true" help:"Download filename override. Implies --zip when several paths are attached."`
	Zip      bool     `long:"zip" optional:"true" help:"Always package the attached paths as one zip."`
	Separate bool     `long:"separate" optional:"true" help:"Always publish the attached files separately (fails on a directory or more than 20 files)."`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
}

func notifyHumanCmd() *cobra.Command {
	return boa.CmdT[notifyHumanParams]{
		Use:   "notify-human",
		Short: "Send the human a notification (shown in the dashboard Messages tab)",
		Long: "Sends a message to the human — it lands in the agentd dashboard's Messages tab, letting a coordinating agent reach the human off the busy terminal.\n\n" +
			"Sending is gated: it passes for the human, for holders of the `human.notify` permission (which the human grants to a trusted coordinating agent such as the PO), and for any group owner (owning a group is a trusted coordinating role, so an owner may send slug or not). Agents with none of these are refused.\n\n" +
			"Give the body inline or with --file (--file - reads stdin). Add --attach to publish a file or directory through agentd; the human gets a download button on the message. Repeat --attach for several files: up to 20 arrive as separate downloads (so an image stays viewable), while a directory or a larger set is packaged as one zip. --zip / --separate force either shape.",
		ParamEnrich: common.DefaultParamEnricher(),
		InitFuncCtx: func(ctx *boa.HookContext, p *notifyHumanParams, _ *cobra.Command) error {
			boa.GetParamT(ctx, &p.AskHuman).SetAlternativesFunc(completeAskHumanDurations)
			return nil
		},
		RunFunc: func(p *notifyHumanParams, _ *cobra.Command, _ []string) {
			os.Exit(runNotifyHuman(p, os.Stdin, os.Stdout, os.Stderr))
		},
	}.ToCobra()
}

func runNotifyHuman(p *notifyHumanParams, stdin io.Reader, stdout, stderr io.Writer) int {
	body, rc := resolveBodyInput(p.Body, p.File, "the body argument", stdin, stderr)
	if rc != rcOK {
		return rc
	}
	if strings.TrimSpace(body) == "" {
		fmt.Fprintln(stderr, "Error: a notification body is required — pass it inline or via --file")
		return rcInvalidArg
	}
	ask, err := ParseAskHuman(p.AskHuman)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}

	payload := map[string]any{"body": body}
	if s := strings.TrimSpace(p.Subject); s != "" {
		payload["subject"] = s
	}
	var resp struct {
		ID int64 `json:"id"`
	}
	if len(p.Attach) == 0 {
		if strings.TrimSpace(p.Name) != "" {
			fmt.Fprintln(stderr, "Error: --name requires at least one --attach path")
			return rcInvalidArg
		}
		if err := DaemonRequest(http.MethodPost, "/v1/notify-human", payload, &resp, DaemonOpts{AskHuman: ask}); err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return MapDaemonErrorToRC(err)
		}
		fmt.Fprintf(stdout, "Notified the human (message #%d) — it will show in the dashboard Messages tab.\n", resp.ID)
		return rcOK
	}
	mode, modeRC := resolveNotifyHumanAttachMode(p, stderr)
	if modeRC != rcOK {
		return modeRC
	}
	data, name, contentType, summary, buildRC := buildNotifyHumanPayload(p, mode, stderr)
	if buildRC != rcOK {
		return buildRC
	}
	if len(data) > maxExportArtifactBytes {
		fmt.Fprintf(stderr, "Error: attachment is %s, over the %d MiB limit\n",
			humanBytes(len(data)), maxExportArtifactBytes>>20)
		return rcInvalidArg
	}
	metadata, err := json.Marshal(map[string]string{"body": body, "subject": strings.TrimSpace(p.Subject), "name": name})
	if err != nil {
		fmt.Fprintf(stderr, "Error: encode attachment metadata: %v\n", err)
		return rcIOFailure
	}
	headers := make(http.Header)
	headers.Set("X-Tclaude-Notify-Metadata", base64.RawURLEncoding.EncodeToString(metadata))
	if err := DaemonPostRawWithOptions("/v1/notify-human/attachment", contentType, data, headers, &resp, DaemonOpts{AskHuman: ask}); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	fmt.Fprintf(stdout, "Notified the human (message #%d) with %s ready to download.\n", resp.ID, summary)
	return rcOK
}

// notifyHumanAutoZipFileCount is where automatic packaging kicks in. Below it,
// the human gets each file as its own download — which is what makes an
// attached image viewable in the dashboard instead of buried in an archive.
// Above it a message would turn into a file listing, so one zip is kinder. It
// also matches the daemon's per-message file cap
// (maxNotifyHumanAttachmentsPerMessage).
const notifyHumanAutoZipFileCount = 20

type notifyAttachMode int

const (
	notifyAttachAuto notifyAttachMode = iota
	notifyAttachZip
	notifyAttachSeparate
)

func resolveNotifyHumanAttachMode(p *notifyHumanParams, stderr io.Writer) (notifyAttachMode, int) {
	switch {
	case p.Zip && p.Separate:
		fmt.Fprintln(stderr, "Error: --zip and --separate are mutually exclusive")
		return notifyAttachAuto, rcInvalidArg
	case p.Zip:
		return notifyAttachZip, rcOK
	case p.Separate:
		if strings.TrimSpace(p.Name) != "" {
			fmt.Fprintln(stderr, "Error: --name renames a single download, so it cannot be combined with --separate")
			return notifyAttachAuto, rcInvalidArg
		}
		return notifyAttachSeparate, rcOK
	// Naming the download only makes sense for one artifact, so it selects
	// packaging rather than being rejected against the auto default.
	case strings.TrimSpace(p.Name) != "" && len(p.Attach) > 1:
		return notifyAttachZip, rcOK
	}
	return notifyAttachAuto, rcOK
}

// buildNotifyHumanPayload produces the upload body: either one artifact (raw
// body, the historical shape) or a multipart body carrying each file
// separately. summary describes what was published for the CLI's confirmation.
func buildNotifyHumanPayload(p *notifyHumanParams, mode notifyAttachMode, stderr io.Writer) (
	data []byte, name, contentType, summary string, rc int,
) {
	files, fileCount, hasDir, rc := expandAttachPaths(p.Attach, stderr)
	if rc != rcOK {
		return nil, "", "", "", rc
	}
	separate := mode == notifyAttachSeparate ||
		(mode == notifyAttachAuto && !hasDir && len(files) > 1 && fileCount <= notifyHumanAutoZipFileCount)
	if separate {
		if hasDir {
			fmt.Fprintln(stderr, "Error: --separate cannot publish a directory — attach its files, or use --zip")
			return nil, "", "", "", rcInvalidArg
		}
		if len(files) > notifyHumanAutoZipFileCount {
			fmt.Fprintf(stderr, "Error: %d files is over the %d-file limit for separate attachments — use --zip\n",
				len(files), notifyHumanAutoZipFileCount)
			return nil, "", "", "", rcInvalidArg
		}
		data, contentType, rc := buildMultipartAttachments(files, stderr)
		if rc != rcOK {
			return nil, "", "", "", rc
		}
		return data, "", contentType, fmt.Sprintf("%d files (%s)", len(files), humanBytes(len(data))), rcOK
	}
	data, name, contentType, rc = buildExportArtifact(p.Attach, p.Name, stderr)
	if rc != rcOK {
		return nil, "", "", "", rc
	}
	return data, name, contentType, fmt.Sprintf("%s (%s)", name, humanBytes(len(data))), rcOK
}

// expandAttachPaths validates the attached paths and returns the individually
// attachable regular files among them, the total file count including directory
// contents (what the auto mode weighs), and whether any path was a directory.
// A directory is never expanded into separate attachments — it is zipped so its
// layout survives — so its files are counted, not listed.
func expandAttachPaths(paths []string, stderr io.Writer) (files []string, fileCount int, hasDir bool, rc int) {
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return nil, 0, false, rcInvalidArg
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			fmt.Fprintf(stderr, "Error: %q is a symlink — attach its resolved file or directory explicitly\n", p)
			return nil, 0, false, rcInvalidArg
		case info.IsDir():
			hasDir = true
			n, err := countFilesUnder(p)
			if err != nil {
				fmt.Fprintf(stderr, "Error: reading %q: %v\n", p, err)
				return nil, 0, false, rcIOFailure
			}
			fileCount += n
		case info.Mode().IsRegular():
			files = append(files, p)
			fileCount++
		default:
			fmt.Fprintf(stderr, "Error: %q is not a regular file or directory\n", p)
			return nil, 0, false, rcInvalidArg
		}
	}
	return files, fileCount, hasDir, rcOK
}

func countFilesUnder(root string) (int, error) {
	n := 0
	err := filepath.WalkDir(root, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			n++
		}
		return nil
	})
	return n, err
}

// buildMultipartAttachments packages each file as its own part, so the
// dashboard shows (and can preview) them individually. Base names collide
// across directories, so duplicates are disambiguated the same way zip entries
// are.
func buildMultipartAttachments(files []string, stderr io.Writer) ([]byte, string, int) {
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	seen := make(map[string]int)
	for _, file := range files {
		data, err := readFileCapped(file)
		if err != nil {
			if errors.Is(err, errExportArtifactTooLarge) {
				fmt.Fprintf(stderr, "Error: attachment is over the %d MiB limit\n", maxExportArtifactBytes>>20)
				return nil, "", rcInvalidArg
			}
			fmt.Fprintf(stderr, "Error: reading %q: %v\n", file, err)
			return nil, "", rcIOFailure
		}
		name := uniqueZipName(safeZipComponent(filepath.Base(filepath.Clean(file))), seen)
		header := make(textproto.MIMEHeader)
		header.Set("Content-Disposition",
			mime.FormatMediaType("form-data", map[string]string{"name": "files", "filename": name}))
		header.Set("Content-Type", contentTypeForName(name))
		part, err := writer.CreatePart(header)
		if err == nil {
			_, err = part.Write(data)
		}
		if err != nil {
			fmt.Fprintf(stderr, "Error: building upload: %v\n", err)
			return nil, "", rcIOFailure
		}
	}
	if err := writer.Close(); err != nil {
		fmt.Fprintf(stderr, "Error: building upload: %v\n", err)
		return nil, "", rcIOFailure
	}
	return buf.Bytes(), writer.FormDataContentType(), rcOK
}
