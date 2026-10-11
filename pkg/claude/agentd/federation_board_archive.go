package agentd

import (
	"archive/tar"
	"compress/gzip"
	"mime"
	"net/http"

	"github.com/tofutools/tclaude/pkg/claude/common/skillbundle"
	"github.com/tofutools/tclaude/pkg/federation/proto"
)

func serveBoardArchive(w http.ResponseWriter, r *http.Request, m proto.BoardItemManifest, raw []byte) {
	files := []skillbundle.File{{Path: "bundle.json", Data: raw}}
	name := "board-" + m.Version + ".tar.gz"
	prefix := ""
	if m.Kind == "skill" {
		b, e := skillbundle.Decode(raw)
		if e != nil {
			writeError(w, 400, "unsafe_bundle", e.Error())
			return
		}
		files = b.Files
		prefix = b.Name + "/"
		name = b.Name + ".tar.gz"
	}
	preserve := r.URL.Query().Get("preserve_executable")
	if preserve != "" && preserve != "0" && preserve != "1" {
		writeError(w, 400, "archive_options", "preserve_executable must be 0 or 1")
		return
	}
	// Caller has reverified signature, hash and scans before response headers.
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
	w.Header().Set("Cache-Control", "no-store")
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	defer func() { _ = tw.Close(); _ = gz.Close() }()
	for _, file := range files {
		if r.Context().Err() != nil {
			return
		}
		mode := int64(0600)
		if preserve == "1" && file.Executable {
			mode = 0700
		}
		header := &tar.Header{Name: prefix + file.Path, Mode: mode, Size: int64(len(file.Data)), Typeflag: tar.TypeReg}
		if e := tw.WriteHeader(header); e != nil {
			return
		}
		if _, e := tw.Write(file.Data); e != nil {
			return
		}
	}
}
