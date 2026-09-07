package browser

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"strings"
	"testing"

	"github.com/go-rod/rod/lib/input"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/internal/backend/app"
	"github.com/tofutools/tclaude/internal/backend/model"
)

func TestBrowserAttachmentPreviewIsExplicitInertAndCancelled(t *testing.T) {
	ctx, page, operator := processEditorBrowser(t)
	var encoded bytes.Buffer
	pixel := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixel.Set(0, 0, color.RGBA{255, 0, 0, 255})
	require.NoError(t, png.Encode(&encoded, pixel))
	var message model.Message
	require.NoError(t, operator.Call(ctx, "POST", "/v2/messages", map[string]any{"request_id": "previews", "subject": "Preview files", "body": "Inspect without acknowledging", "to": model.MessageAudience{Operator: true}, "attachments": []app.AttachmentInput{{Filename: "pixel.png", MediaType: "image/png", Content: encoded.Bytes()}, {Filename: "notes.html", MediaType: "text/html", Content: []byte("<img src=x onerror=alert(1)>\n" + strings.Repeat("a", 70000))}, {Filename: "spoof.png", MediaType: "image/png", Content: []byte("<svg></svg>")}}}, &message))
	page.MustEval(`() => {window.previewReads=0;window.previewRevoked=[];const original=window.fetch;window.fetch=(url,options)=>{if(String(url).startsWith('/v2/attachments/'))window.previewReads++;return original(url,options)};const revoke=URL.revokeObjectURL;URL.revokeObjectURL=url=>{window.previewRevoked.push(url);revoke(url)}}`)
	page.MustElement("#refresh").MustClick()
	page.MustElement("[data-tab=messages]").MustClick()
	page.MustElementR("#message-list button", "^Preview pixel.png$")
	require.Equal(t, 0, page.MustEval(`() => window.previewReads`).Int())
	page.MustElementR("#message-list button", "^Preview pixel.png$").MustClick()
	page.MustElementR("#attachment-preview [role=status]", "2 × 2 pixels")
	require.True(t, page.MustElement("#attachment-preview img").MustVisible())
	page.Keyboard.MustType(input.Escape)
	page.MustWait(`() => !document.querySelector('#attachment-preview').open`)
	require.Equal(t, 1, page.MustEval(`() => window.previewRevoked.length`).Int())
	page.MustElementR("#message-list button", "^Preview notes.html$").MustClick()
	page.MustElementR("#attachment-preview [role=status]", "first 64 KiB")
	require.Contains(t, page.MustElement("#attachment-preview pre").MustText(), "<img src=x onerror=alert(1)>")
	require.False(t, page.MustHas("#attachment-preview img"))
	page.MustElementR("#attachment-preview button", "^Close preview$").MustClick()
	page.MustElementR("#message-list button", "^Preview spoof.png$").MustClick()
	page.MustElementR("#attachment-preview [role=status]", "does not match")
	require.False(t, page.MustHas("#attachment-preview img"))
	page.MustElementR("#attachment-preview button", "^Close preview$").MustClick()
	var snapshot struct {
		Messages []model.Message `json:"messages"`
	}
	require.NoError(t, operator.Call(ctx, "GET", "/v2/snapshot", nil, &snapshot))
	require.Len(t, snapshot.Messages, 1)
	require.Nil(t, snapshot.Messages[0].Recipients[0].ReadAt)
	// Delay a real authenticated response, close the preview, then release it.
	page.MustEval(`() => {const original=window.fetch;window.previewRelease=null;window.fetch=async(url,options)=>{const response=await original(url,options);if(String(url).startsWith('/v2/attachments/'))await new Promise(resolve=>window.previewRelease=resolve);return response}}`)
	page.MustElementR("#message-list button", "^Preview pixel.png$").MustClick()
	page.MustWait(`() => typeof window.previewRelease==='function'`)
	page.MustEval(`() => {window.pendingPreviewButton=Array.from(document.querySelectorAll('#message-list button')).find(b=>b.textContent==='Preview pixel.png')}`)
	page.MustElementR("#attachment-preview button", "^Close preview$").MustClick()
	page.MustElement("#logout").MustClick()
	page.MustElementR("#connection", "Signed out")
	page.MustEval(`() => window.previewRelease()`)
	page.MustWait(`() => !window.pendingPreviewButton.disabled && !document.querySelector('#attachment-preview').open && document.querySelector('#attachment-preview').textContent==='Close preview'`)
	require.False(t, page.MustHas("#attachment-preview img"))
	require.Equal(t, 1, page.MustEval(`() => window.previewRevoked.length`).Int())
}
