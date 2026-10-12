package privacy

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/paf"
)

// Audience kinds, as privacy.json names them.
const (
	AudienceSelf       = "self"
	AudienceRecipients = "recipients"
	AudienceBoard      = "board"
	AudiencePublic     = "public"
)

// ConsentRequest is a human's confirmation that a PAF may go to an
// audience.
type ConsentRequest struct {
	// By names who confirmed. It is recorded as given.
	By string
	// Audience is self, recipients, board or public.
	Audience   string
	Recipients []string
	Board      string
	// At defaults to now.
	At time.Time
}

func (c ConsentRequest) check() error {
	if strings.TrimSpace(c.By) == "" {
		return errors.New("consent needs who confirmed it")
	}
	switch c.Audience {
	case AudienceSelf, AudiencePublic:
	case AudienceRecipients:
		if len(c.Recipients) == 0 {
			return errors.New("consent for recipients needs at least one recipient")
		}
	case AudienceBoard:
		if c.Board == "" {
			return errors.New("consent for a board needs the board")
		}
	default:
		return fmt.Errorf("unknown audience %q: use self, recipients, board or public", c.Audience)
	}
	return nil
}

// AddConsent writes a copy of a with a consent record bound to its content
// digest. The audience and sensitivity are set to match. Content entries are
// copied byte for byte, so earlier consent for the same content stays
// valid; the copy is signed by keys.
func AddConsent(a *paf.Archive, out io.Writer, c ConsentRequest, limits paf.Limits, producer paf.Producer, keys ...paf.SigningKey) (paf.Consent, error) {
	if err := c.check(); err != nil {
		return paf.Consent{}, err
	}
	at := c.At
	if at.IsZero() {
		at = time.Now()
	}
	rec := paf.Consent{By: c.By, At: at.UTC().Truncate(time.Second), Audience: c.Audience, ContentDigest: paf.ContentDigest(a.Manifest.Entries)}
	p := paf.Privacy{}
	if old := a.Privacy(); old != nil {
		p = *old
	}
	p.Consent = append(slices.Clone(p.Consent), rec)
	p.Audience = &paf.Audience{Kind: c.Audience, Recipients: c.Recipients, Board: c.Board}
	switch c.Audience {
	case AudiencePublic:
		p.Sensitivity = "public"
	case AudienceRecipients, AudienceBoard:
		p.Sensitivity = "shared"
	default:
		if p.Sensitivity == "" {
			p.Sensitivity = "private"
		}
	}
	if err := paf.WithPrivacy(a, out, p, limits, producer, keys...); err != nil {
		return paf.Consent{}, err
	}
	return rec, nil
}

// ConsentCheck is one consent record and whether it binds this content.
type ConsentCheck struct {
	paf.Consent
	// Current is true when the record's digest matches the content: the
	// content has not changed since the human confirmed it.
	Current bool `json:"current"`
}

// CheckConsent reports each consent record and whether it still binds the
// archive's content.
func CheckConsent(a *paf.Archive) []ConsentCheck {
	p := a.Privacy()
	if p == nil {
		return nil
	}
	digest := paf.ContentDigest(a.Manifest.Entries)
	var out []ConsentCheck
	for _, c := range p.Consent {
		out = append(out, ConsentCheck{Consent: c, Current: c.ContentDigest == digest})
	}
	return out
}

// ConsentSummary is what a human sees before consenting: how much is
// shared, the time range and turn count of the conversation, what the scan
// finds and what was redacted.
type ConsentSummary struct {
	Bytes      int64           `json:"bytes"`
	Agents     int             `json:"agents"`
	Turns      int             `json:"turns"`
	From       *time.Time      `json:"from,omitempty"`
	To         *time.Time      `json:"to,omitempty"`
	NativeOnly int             `json:"native_only,omitempty"`
	Findings   []paf.Finding   `json:"findings"`
	Redactions []paf.Redaction `json:"redactions"`
	Digest     string          `json:"content_digest"`
}

// SummarizeForConsent describes a for a consent prompt. Turns are human
// turns in each agent's context, full when present, else effective; an
// agent with only a native blob is counted in NativeOnly. The scan runs on
// this node; the producer's recorded scan is not trusted.
func SummarizeForConsent(a *paf.Archive, recordLimit int) (*ConsentSummary, error) {
	s := &ConsentSummary{Agents: len(a.Manifest.Agents), Redactions: []paf.Redaction{}, Digest: paf.ContentDigest(a.Manifest.Entries)}
	for _, e := range a.Manifest.Entries {
		s.Bytes += e.Bytes
	}
	for _, key := range a.Manifest.Agents {
		scope := paf.ScopeFull
		if !a.HasContext(key, scope) {
			scope = paf.ScopeEffective
		}
		if !a.HasContext(key, scope) {
			if ag, ok := a.Agent(key); ok && ag.Native != nil {
				s.NativeOnly++
			}
			continue
		}
		if err := s.addContext(a, key, scope); err != nil {
			return nil, err
		}
	}
	findings, err := ScanArchive(a, recordLimit)
	if err != nil {
		return nil, err
	}
	s.Findings = findings
	if p := a.Privacy(); p != nil {
		s.Redactions = append(s.Redactions, p.Redactions...)
	}
	return s, nil
}

func (s *ConsentSummary) addContext(a *paf.Archive, key, scope string) error {
	cr, closer, err := a.Context(key, scope)
	if err != nil {
		return err
	}
	defer func() { _ = closer.Close() }()
	for {
		l, err := cr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if l.Type != paf.LineEntry {
			continue
		}
		if l.Role == "user" && l.Origin == "human" {
			s.Turns++
		}
		if t := l.Time; !t.IsZero() {
			if s.From == nil || t.Before(*s.From) {
				s.From = &t
			}
			if s.To == nil || t.After(*s.To) {
				s.To = &t
			}
		}
	}
}
