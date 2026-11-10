package agentd

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"testing/iotest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type closeSpy struct {
	io.Reader
	closed *bool
}

func (c closeSpy) Close() error { *c.closed = true; return nil }

func TestSnapshotRequestBodyRestoresEveryByte(t *testing.T) {
	for name, size := range map[string]int{
		"small":          1024,
		"at the bound":   maxApprovalRestoreBody,
		"past the bound": maxApprovalRestoreBody + 1,
		"far past":       3 * maxApprovalRestoreBody,
	} {
		t.Run(name, func(t *testing.T) {
			want := make([]byte, size)
			_, err := rand.Read(want)
			require.NoError(t, err)
			closed := false
			r := httptest.NewRequest("POST", "/v1/anything", nil)
			source := bytes.NewReader(want)
			r.Body = closeSpy{Reader: source, closed: &closed}

			snapshotRequestBody(r)

			if size > maxApprovalRestoreBody {
				assert.False(t, closed, "the remainder is still to be read")
				assert.LessOrEqual(t, size-source.Len(), maxApprovalRestoreBody+1, "the rest stays out of memory")
			}
			got, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(want, got), "%d bytes back of %d", len(got), len(want))
			require.NoError(t, r.Body.Close())
			assert.True(t, closed)
		})
	}
}

// Over a real connection the sender blocks on the unread remainder while the
// popup waits, and the handler still gets all of it afterwards.
func TestSnapshotRequestBodyRemainderWaitsOnTheConnection(t *testing.T) {
	want := make([]byte, 16*maxApprovalRestoreBody)
	_, err := rand.Read(want)
	require.NoError(t, err)
	snapshotted := make(chan struct{})
	decided := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		snapshotRequestBody(r)
		close(snapshotted)
		<-decided
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(want, got) {
			http.Error(w, "body differs", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(strconv.Itoa(len(got))))
	}))
	defer server.Close()

	type result struct {
		status int
		err    error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := http.Post(server.URL, "application/octet-stream", bytes.NewReader(want))
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		done <- result{status: resp.StatusCode}
	}()
	<-snapshotted
	select {
	case got := <-done:
		t.Fatalf("the request finished before the human decided: %+v", got)
	default:
	}
	close(decided)
	got := <-done
	require.NoError(t, got.err)
	assert.Equal(t, http.StatusOK, got.status)
}

// A route whose facts read the whole body meets a failed read itself, and the
// handler must not be given the part that arrived as if it were all of it.
func TestApprovalFactsRefuseABodyThatCouldNotBeRead(t *testing.T) {
	broken := errors.New("connection reset")
	r := httptest.NewRequest("POST", "/v1/anything", nil)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader([]byte(`{"a":1}`)), iotest.ErrReader(broken)))
	r = withApprovalFacts(r, func([]byte) string {
		t.Error("facts must not describe a partial body")
		return ""
	})
	ticket, refused := (&approvalRegistry{pending: map[string]*approvalRequest{}}).admit("caller")
	require.NotNil(t, ticket, refused)
	_, _, err := approvalPreviewFacts(r, ticket, "preview", "")
	require.ErrorIs(t, err, broken)

	w := httptest.NewRecorder()
	refuseApprovalBody(w, err)
	assert.Equal(t, http.StatusBadRequest, w.Code)

	w = httptest.NewRecorder()
	refuseApprovalBody(w, &http.MaxBytesError{Limit: 1})
	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}
