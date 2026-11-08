package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GiGurra/boa/pkg/boa"
	"github.com/spf13/cobra"
	"github.com/tofutools/tclaude/pkg/common"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// Files on a task (docs/taskboard.md): `taskboard attach`, `detach`,
// `attachments [--save DIR]`, and --attach on add and ask. The CLI reads
// each file as the caller, so an agent attaches only what it can read; the
// daemon never opens a caller-supplied path.

const (
	// taskAttachmentMaxFiles and taskAttachmentMaxBytes mirror the daemon's
	// caps, so a refusal comes before the upload.
	taskAttachmentMaxFiles = 10
	taskAttachmentMaxBytes = 4 << 20
)

// taskboardAttachment is the daemon's wire shape for one file of a task.
type taskboardAttachment struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Size        int64  `json:"size"`
	ContentType string `json:"content_type"`
	SHA256      string `json:"sha256"`
	Image       bool   `json:"image,omitempty"`
	AddedBy     string `json:"added_by"`
	AddedAt     string `json:"added_at"`
	// Drawing names the drawing board a drawing-board export shows.
	Drawing *struct {
		Board    string          `json:"board"`
		Version  int64           `json:"version"`
		Elements []string        `json:"elements"`
		Region   json.RawMessage `json:"region"`
	} `json:"drawing,omitempty"`
}

// taskboardAttachmentData is a file's bytes as GET …/attachments/{id}
// ?format=json answers them.
type taskboardAttachmentData struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Data   []byte `json:"data"`
}

// taskboardFile is one file of an upload.
type taskboardFile struct {
	Name string
	Data []byte
}

// readTaskAttachmentFiles reads the files to attach, under the caps.
func readTaskAttachmentFiles(paths []string) ([]taskboardFile, error) {
	if len(paths) > taskAttachmentMaxFiles {
		return nil, fmt.Errorf("a task holds at most %d files", taskAttachmentMaxFiles)
	}
	out := make([]taskboardFile, 0, len(paths))
	for _, path := range paths {
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() {
			_ = f.Close()
			return nil, fmt.Errorf("%s: not a regular file", path)
		}
		b, err := io.ReadAll(io.LimitReader(f, taskAttachmentMaxBytes+1))
		_ = f.Close()
		if err != nil {
			return nil, err
		}
		if len(b) > taskAttachmentMaxBytes {
			return nil, fmt.Errorf("%s is larger than %d MiB", path, taskAttachmentMaxBytes>>20)
		}
		out = append(out, taskboardFile{Name: filepath.Base(path), Data: b})
	}
	return out, nil
}

// taskboardMultipart builds a request of a "task" part (task's JSON, when
// task is not nil) and one "file" part per file.
func taskboardMultipart(task any, files []taskboardFile) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if task != nil {
		raw, err := json.Marshal(task)
		if err != nil {
			return nil, "", err
		}
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", `form-data; name="task"`)
		h.Set("Content-Type", "application/json")
		part, err := mw.CreatePart(h)
		if err == nil {
			_, err = part.Write(raw)
		}
		if err != nil {
			return nil, "", err
		}
	}
	for _, f := range files {
		h := textproto.MIMEHeader{}
		h.Set("Content-Disposition", mime.FormatMediaType("form-data", map[string]string{"name": "file", "filename": f.Name}))
		h.Set("Content-Type", "application/octet-stream")
		part, err := mw.CreatePart(h)
		if err == nil {
			_, err = part.Write(f.Data)
		}
		if err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// peerViewDirectMax is the most a request may carry straight through one
// peer view request (the daemon's fedPeerViewBodyLimit).
const peerViewDirectMax = 32 << 10

// taskboardUpload posts a multipart request to path on this node or, with
// peer, to that linked node: straight through the peer view when it fits
// one request (an agent's view too, under its peer grants), through this
// node's staged upload otherwise (the operator's). A peer's answer is
// cleaned as remoteRequest cleans it.
func taskboardUpload(peer, path string, task any, files []taskboardFile, out any, ask time.Duration) error {
	body, contentType, err := taskboardMultipart(task, files)
	if err != nil {
		return err
	}
	opts := DaemonOpts{AskHuman: ask}
	peer = strings.TrimSpace(peer)
	if peer == "" {
		return DaemonPostRawWithOptions(path, contentType, body, nil, out, opts)
	}
	var raw any
	target := remotePath(peer, path)
	if len(body) > peerViewDirectMax {
		target = "/v1/federation/peer-upload/" + url.PathEscape(peer) + "?" + url.Values{"path": {path}}.Encode()
	}
	if err := DaemonPostRawWithOptions(target, contentType, body, nil, &raw, opts); err != nil {
		var de *DaemonError
		if len(body) > peerViewDirectMax && errors.As(err, &de) && de.Status == http.StatusForbidden && de.Code == "auth" {
			return fmt.Errorf("files over %d KiB in all reach a linked node only through the operator's staged upload; ask the operator, or send smaller files: %w",
				peerViewDirectMax>>10, err)
		}
		if clean := proto.StripControls(err.Error()); clean != err.Error() {
			return fmt.Errorf("%s", clean)
		}
		return err
	}
	return decodeRemoteAnswer(raw, out)
}

// taskboardUploadFiles reads paths and uploads them with task.
func taskboardUploadFiles(peer, path string, task any, paths []string, out any, ask time.Duration) error {
	files, err := readTaskAttachmentFiles(paths)
	if err != nil {
		return err
	}
	return taskboardUpload(peer, path, task, files, out, ask)
}

// listTaskboardAttachments reads a task's files.
func listTaskboardAttachments(peer, task string) ([]taskboardAttachment, error) {
	var resp struct {
		Attachments []taskboardAttachment `json:"attachments"`
	}
	if err := remoteRequest(peer, http.MethodGet, taskboardPath(task, "/attachments"), nil, &resp); err != nil {
		return nil, err
	}
	return resp.Attachments, nil
}

// fetchTaskboardAttachment reads one file's bytes, as base64 JSON, which a
// peer view carries too.
func fetchTaskboardAttachment(peer, task string, id int64) (*taskboardAttachmentData, error) {
	var out taskboardAttachmentData
	path := taskboardPath(task, "/attachments/"+strconv.FormatInt(id, 10)) + "?format=json"
	if err := remoteRequestOpts(peer, http.MethodGet, path, nil, &out, DaemonOpts{Timeout: time.Minute}); err != nil {
		return nil, err
	}
	return &out, nil
}

// safeSaveName is a file name a peer (or anyone) sent, made safe to create
// in a directory: its base name, with no leading dot.
func safeSaveName(name string, id int64) string {
	base := filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	base = proto.SafeName(base, false)
	if base == "" || base == "." || base == ".." || base == "unknown" || strings.HasPrefix(base, ".") {
		return "attachment-" + strconv.FormatInt(id, 10)
	}
	return base
}

// saveTaskboardAttachments writes the task's files into dir, which it
// creates; it never overwrites a file there.
func saveTaskboardAttachments(peer, task, dir string, atts []taskboardAttachment) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var saved []string
	for _, a := range atts {
		data, err := fetchTaskboardAttachment(peer, task, a.ID)
		if err != nil {
			return saved, fmt.Errorf("%s: %w", a.Name, err)
		}
		if int64(len(data.Data)) != a.Size {
			return saved, fmt.Errorf("%s: got %d bytes, expected %d", a.Name, len(data.Data), a.Size)
		}
		path := filepath.Join(dir, safeSaveName(a.Name, a.ID))
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return saved, err
		}
		_, werr := f.Write(data.Data)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return saved, werr
		}
		saved = append(saved, path)
	}
	return saved, nil
}

func formatAttachmentSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// printTaskboardAttachments prints one line per file.
func printTaskboardAttachments(stdout io.Writer, indent string, atts []taskboardAttachment) {
	for _, a := range atts {
		line := fmt.Sprintf("%s#%d  %s  (%s, %s)", indent, a.ID, a.Name, a.ContentType, formatAttachmentSize(a.Size))
		if d := a.Drawing; d != nil {
			part := "all of"
			switch {
			case len(d.Elements) > 0:
				part = fmt.Sprintf("%d element(s) of", len(d.Elements))
			case len(d.Region) > 0 && string(d.Region) != "null":
				part = "a region of"
			}
			line += fmt.Sprintf("  drawing: %s drawing board %s at version %d", part, d.Board, d.Version)
		}
		fmt.Fprintln(stdout, line)
	}
}

// ---- attach ----

type taskboardAttachParams struct {
	Task     string   `pos:"true" help:"Task id (tk_…)"`
	Files    []string `pos:"true" help:"Files to attach (at most 10 per task, 4 MiB each; images, text, data and PDF)"`
	Peer     string   `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.write); the operator's"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func taskboardAttachCmd() *cobra.Command {
	return boa.CmdT[taskboardAttachParams]{
		Use:   "attach",
		Short: "Attach files to a task",
		Long: "Attach files to a task: at most 10 per task and 4 MiB each, of the types mail attachments may have " +
			"(images, text, markdown, csv, json, yaml, diff/patch, pdf). A name the task already has gets a numbered " +
			"prefix. It needs taskboard.write on the task's group; reading them back needs taskboard.read " +
			"(taskboard attachments --save).",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardAttachParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAttach(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardAttach(p *taskboardAttachParams, stdout, stderr io.Writer) int {
	if len(p.Files) == 0 {
		fmt.Fprintln(stderr, "Error: give one or more files to attach")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	var t taskboardTask
	if err := taskboardUploadFiles(p.Peer, taskboardPath(p.Task, "/attachments"), nil, p.Files, &t, ask); err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.JSON {
		return writeJSONTo(stdout, t)
	}
	fmt.Fprintf(stdout, "attached %d file(s) to %s: %s\n", len(p.Files), t.ID, t.Title)
	printTaskboardAttachments(stdout, "  ", t.Attachments)
	return rcOK
}

// ---- detach ----

type taskboardDetachParams struct {
	Task     string   `pos:"true" help:"Task id (tk_…)"`
	Names    []string `pos:"true" help:"Files to detach, by name or #id (as taskboard attachments lists them)"`
	Peer     string   `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.write)"`
	AskHuman string   `long:"ask-human" optional:"true" help:"On permission denial, ask the human via popup with this timeout (e.g. '30s'). Capped at 300s. Timeout = deny."`
	JSON     bool     `long:"json" help:"Output JSON"`
}

func taskboardDetachCmd() *cobra.Command {
	return boa.CmdT[taskboardDetachParams]{
		Use:         "detach",
		Short:       "Remove files from a task",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardDetachParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardDetach(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

// resolveTaskboardAttachment finds a file by #id, id or name (case-folded).
func resolveTaskboardAttachment(atts []taskboardAttachment, ref string) (taskboardAttachment, error) {
	ref = strings.TrimSpace(ref)
	if id, err := strconv.ParseInt(strings.TrimPrefix(ref, "#"), 10, 64); err == nil {
		for _, a := range atts {
			if a.ID == id {
				return a, nil
			}
		}
	}
	for _, a := range atts {
		if strings.EqualFold(a.Name, ref) {
			return a, nil
		}
	}
	return taskboardAttachment{}, fmt.Errorf("the task has no file %q", ref)
}

func runTaskboardDetach(p *taskboardDetachParams, stdout, stderr io.Writer) int {
	if len(p.Names) == 0 {
		fmt.Fprintln(stderr, "Error: give one or more files to detach, by name or #id")
		return rcInvalidArg
	}
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	ask, rc := parseAskHumanFlag(p.AskHuman, stdout, stderr)
	if rc != rcOK {
		return rc
	}
	atts, err := listTaskboardAttachments(p.Peer, p.Task)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	var targets []taskboardAttachment
	for _, name := range p.Names {
		a, err := resolveTaskboardAttachment(atts, name)
		if err != nil {
			fmt.Fprintf(stderr, "Error: %v\n", err)
			return rcInvalidArg
		}
		targets = append(targets, a)
	}
	var t taskboardTask
	for _, a := range targets {
		path := taskboardPath(p.Task, "/attachments/"+strconv.FormatInt(a.ID, 10))
		if err := remoteRequestOpts(p.Peer, http.MethodDelete, path, nil, &t, DaemonOpts{AskHuman: ask}); err != nil {
			fmt.Fprintf(stderr, "Error: detach %s: %v\n", a.Name, err)
			return MapDaemonErrorToRC(err)
		}
		if !p.JSON {
			fmt.Fprintf(stdout, "detached %s from %s\n", a.Name, t.ID)
		}
	}
	if p.JSON {
		return writeJSONTo(stdout, t)
	}
	return rcOK
}

// ---- attachments ----

type taskboardAttachmentsParams struct {
	Task string `pos:"true" help:"Task id (tk_…)"`
	Save string `long:"save" optional:"true" help:"Save every file into this directory (created if missing; an existing file is never overwritten)"`
	Peer string `long:"peer" optional:"true" help:"A task on a linked node's board, under the grants it gave this node (taskboard.read)"`
	JSON bool   `long:"json" help:"Output JSON"`
}

func taskboardAttachmentsCmd() *cobra.Command {
	return boa.CmdT[taskboardAttachmentsParams]{
		Use:   "attachments",
		Short: "List a task's files, or save them with --save DIR",
		Long: "List the files attached to a task. They are stored where agents cannot reach them, so --save is how you get " +
			"to them: it writes each into the directory and prints where. It needs taskboard.read on the task's group.",
		ParamEnrich: common.DefaultParamEnricher(),
		RunFunc: func(p *taskboardAttachmentsParams, _ *cobra.Command, _ []string) {
			taskboardExit(runTaskboardAttachments(p, cliStdout, cliStderr))
		},
	}.ToCobra()
}

func runTaskboardAttachments(p *taskboardAttachmentsParams, stdout, stderr io.Writer) int {
	if rc := RequireDaemonOrExit(stderr); rc != rcOK {
		return rc
	}
	atts, err := listTaskboardAttachments(p.Peer, p.Task)
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		return MapDaemonErrorToRC(err)
	}
	if p.Save == "" {
		if p.JSON {
			return writeJSONTo(stdout, map[string]any{"task": p.Task, "attachments": atts})
		}
		printRemoteScheduleBanner(stdout, p.Peer, "task files")
		if len(atts) == 0 {
			fmt.Fprintf(stdout, "%s has no files\n", p.Task)
			return rcOK
		}
		printTaskboardAttachments(stdout, "", atts)
		return rcOK
	}
	saved, err := saveTaskboardAttachments(p.Peer, p.Task, p.Save, atts)
	if p.JSON {
		if rc := writeJSONTo(stdout, map[string]any{"task": p.Task, "saved": saved}); rc != rcOK {
			return rc
		}
	} else {
		for _, path := range saved {
			fmt.Fprintf(stdout, "saved %s\n", path)
		}
	}
	if err != nil {
		fmt.Fprintf(stderr, "Error: %v\n", err)
		var de *DaemonError
		if errors.As(err, &de) {
			return MapDaemonErrorToRC(err)
		}
		return rcIOFailure
	}
	return rcOK
}
