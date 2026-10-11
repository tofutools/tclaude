package proto

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"unicode/utf8"
)

// BoardCatalogMetadata is hub-reported sorting data, outside signed receipts.
type BoardCatalogMetadata struct {
	UpdatedAt string `json:"updated_at"`
	Seq       int64  `json:"seq"`
}

// BoardItemMetadata extends encrypted metadata without changing the base
// manifest's signing bytes. Older readers ignore DescriptionProvenance.
type BoardItemMetadata struct {
	BoardItemManifest
	DescriptionProvenance *BoardItemDescription `json:"description_provenance,omitempty"`
}

type BoardItemDescription struct {
	Description string `json:"description"`
	Signature   []byte `json:"signature"`
}

func (d BoardItemDescription) signingBytes(m BoardItemManifest) []byte {
	raw, _ := json.Marshal(struct{ Item, Version, SHA256, Description string }{m.Item, m.Version, m.SHA256, d.Description})
	return append([]byte("tclaude-board-description-v1\n"), raw...)
}
func (d *BoardItemDescription) Sign(m BoardItemManifest, id *Identity) {
	d.Signature = ed25519.Sign(id.Priv, d.signingBytes(m))
}
func (d BoardItemDescription) Verify(m BoardItemManifest) error {
	if m.Verify() != nil || len(d.Description) > 2048 || !utf8.ValidString(d.Description) || !ed25519.Verify(m.Pub, d.signingBytes(m), d.Signature) {
		return errors.New("invalid publisher-signed Board description")
	}
	return nil
}

// Description returns only authenticated optional text; an invalid extension
// never hides otherwise valid older-format content.
func (m BoardItemMetadata) Description() string {
	if m.DescriptionProvenance == nil || m.DescriptionProvenance.Verify(m.BoardItemManifest) != nil {
		return ""
	}
	return m.DescriptionProvenance.Description
}
