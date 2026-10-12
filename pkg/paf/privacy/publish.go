package privacy

import (
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/tofutools/tclaude/pkg/paf"
)

// PublisherConsentBy is who a published PAF's consent record names: the
// publisher, never a person's name.
const PublisherConsentBy = "publisher"

// PublishOptions are the redactions a publisher may add. Publishing always
// masks flagged credentials, replaces paths with placeholders, strips
// identity and drops the native blob and extension data.
type PublishOptions struct {
	StubToolResults bool
	KeepTurns       int
	DropMedia       bool
	DropReasoning   bool
	// Native decodes a native blob for an agent with no context file.
	Native NativeDecoder
	Limits paf.Limits
}

// Publication is a prepared, not yet signed, public PAF.
type Publication struct {
	// Archive is the redacted copy, read from the scratch file.
	Archive *paf.Archive
	Report  *RedactReport
	Summary *ConsentSummary
}

// PreparePublication writes the redacted copy of a to scratch and checks
// it: one agent, no remaining credential findings. The publisher reviews
// Summary (its content digest in particular) before FinishPublication.
func PreparePublication(a *paf.Archive, scratch *os.File, opts PublishOptions, producer paf.Producer, recordLimit int) (*Publication, error) {
	if a.Manifest.Kind != paf.KindAgent || len(a.Manifest.Agents) != 1 {
		return nil, errors.New("publish one agent at a time")
	}
	ro := RedactOptions{MaskFlagged: true, PathsToPlaceholders: true, StripIdentity: true, DropNative: true,
		StubToolResults: opts.StubToolResults, KeepTurns: opts.KeepTurns, DropMedia: opts.DropMedia, DropReasoning: opts.DropReasoning,
		Native: opts.Native, Limits: opts.Limits}
	rep, err := Redact(a, scratch, ro, producer)
	if err != nil {
		return nil, err
	}
	if len(rep.Findings) > 0 {
		return nil, fmt.Errorf("the redacted copy still has %d suspected credential kind(s); remove them from the source and publish again", len(rep.Findings))
	}
	size, err := scratch.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil, err
	}
	out, err := paf.Open(scratch, size, paf.Options{Limits: opts.Limits, Purpose: paf.PurposeInspect})
	if err != nil {
		return nil, err
	}
	summary, err := SummarizeForConsent(out, recordLimit)
	if err != nil {
		return nil, err
	}
	return &Publication{Archive: out, Report: rep, Summary: summary}, nil
}

// FinishPublication records the publisher's consent to public sharing and
// writes the copy to out, signed by the pseudonymous publisher key only.
func FinishPublication(p *Publication, out io.Writer, producer paf.Producer, key paf.SigningKey, limits paf.Limits) error {
	if key.Signer == nil || key.Signer.Kind != "publisher" {
		return errors.New("a published PAF is signed by a publisher key")
	}
	_, err := AddConsent(p.Archive, out, ConsentRequest{By: PublisherConsentBy, Audience: AudiencePublic, At: time.Now()}, limits, producer, key)
	return err
}
