package routebroker_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/routebroker"
)

func flowAuths(pubFlow, conFlow bool) (routebroker.PublisherAuth, routebroker.ConsumerAuth) {
	pub, con := auths()
	if pubFlow {
		pub.FlowWindow = routebroker.InitialWindow
	}
	if conFlow {
		con.FlowWindow = routebroker.InitialWindow
	}
	return pub, con
}

// openFlowStream opens local stream 1 and answers it, returning the
// publisher-side id and the OPEN / OPEN_OK payloads each end saw.
func openFlowStream(t *testing.T, pair attached) (uint64, []byte, []byte) {
	t.Helper()
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindOpen, Stream: 1})
	open := readFrame(t, pair.pubPeer)
	require.Equal(t, routebroker.KindOpen, open.Kind)
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindOpenOK, Stream: open.Stream})
	ok := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindOpenOK, ok.Kind)
	return open.Stream, open.Payload, ok.Payload
}

func TestBrokerFlowControlNeedsBothChannels(t *testing.T) {
	for _, tc := range []struct {
		name     string
		pub, con bool
		want     bool
	}{
		{"both", true, true, true},
		{"publisher only", true, false, false},
		{"consumer only", false, true, false},
		{"neither", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBroker(t, newTestAuthorizer(), routebroker.Config{})
			pub, con := flowAuths(tc.pub, tc.con)
			pair := attachPair(t, b, pub, con)
			_, open, ok := openFlowStream(t, pair)
			require.Equal(t, tc.want, routebroker.IsFlowOpen(open), "OPEN marker")
			require.Equal(t, tc.want, routebroker.IsFlowOpen(ok), "OPEN_OK marker")
			if !tc.want {
				// Neither end of a stream without flow control may see a
				// marker or a WINDOW: an older helper would refuse both.
				require.Empty(t, open)
				require.Empty(t, ok)
			}
		})
	}
}

func TestBrokerForwardsWindowsAndEnforcesCredit(t *testing.T) {
	b := newBroker(t, newTestAuthorizer(), routebroker.Config{})
	pub, con := flowAuths(true, true)
	pair := attachPair(t, b, pub, con)
	global, _, _ := openFlowStream(t, pair)

	// The consumer may send its implicit window, and the publisher reads it.
	chunk := make([]byte, routebroker.MaxFramePayload)
	for range routebroker.InitialWindow / len(chunk) {
		writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: chunk})
		require.Equal(t, routebroker.KindData, readFrame(t, pair.pubPeer).Kind)
	}

	// Having delivered it, the publisher grants more; the consumer sees the
	// grant under its own stream id.
	writeFrame(t, pair.pubPeer, routebroker.WindowFrame(global, 1000))
	w := readFrame(t, pair.conPeer)
	require.Equal(t, routebroker.KindWindow, w.Kind)
	require.Equal(t, uint64(1), w.Stream)
	n, err := routebroker.ParseWindow(w.Payload)
	require.NoError(t, err)
	require.Equal(t, 1000, n)

	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: make([]byte, 1000)})
	require.Equal(t, routebroker.KindData, readFrame(t, pair.pubPeer).Kind)

	// One byte over is a violation: the sender's channel is closed.
	writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: []byte{0}})
	select {
	case err := <-pair.conDone:
		require.ErrorIs(t, err, routebroker.ErrFlowViolation)
	case <-time.After(2 * time.Second):
		t.Fatal("consumer channel survived a credit violation")
	}
}

// TestBrokerHoldsAReceiverToItsDeclaredWindow: a receiver that grants
// beyond the window it declared — say, to make the broker queue a sender's
// bytes it never reads — fails its own channel.
func TestBrokerHoldsAReceiverToItsDeclaredWindow(t *testing.T) {
	b := newBroker(t, newTestAuthorizer(), routebroker.Config{})
	pub, con := flowAuths(true, true)
	pair := attachPair(t, b, pub, con)
	global, _, _ := openFlowStream(t, pair)

	writeFrame(t, pair.pubPeer, routebroker.WindowFrame(global, 1))
	select {
	case err := <-pair.pubDone:
		require.ErrorIs(t, err, routebroker.ErrFlowViolation)
	case <-time.After(2 * time.Second):
		t.Fatal("publisher granted past its declared window")
	}
}

// TestBrokerDropsWindowForRetiredStreams: a grant trailing an orderly end
// arrives after the stream, and possibly its tombstone, is gone. It must
// not cost the sender its channel.
func TestBrokerDropsWindowForRetiredStreams(t *testing.T) {
	b := newBroker(t, newTestAuthorizer(), routebroker.Config{})
	pub, con := flowAuths(true, true)
	pair := attachPair(t, b, pub, con)
	writeFrame(t, pair.pubPeer, routebroker.WindowFrame(999, 4096))
	writeFrame(t, pair.conPeer, routebroker.WindowFrame(999, 4096))
	_, _, _ = openFlowStream(t, pair) // both channels still serve
}

func TestBrokerDropsWindowOnStreamsWithoutFlowControl(t *testing.T) {
	b := newBroker(t, newTestAuthorizer(), routebroker.Config{})
	pub, con := flowAuths(true, false)
	pair := attachPair(t, b, pub, con)
	global, _, _ := openFlowStream(t, pair)

	writeFrame(t, pair.pubPeer, routebroker.WindowFrame(global, 1000))
	// Not forwarded: the next frame the consumer sees is the DATA after it.
	writeFrame(t, pair.pubPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: global, Payload: []byte("x")})
	require.Equal(t, routebroker.KindData, readFrame(t, pair.conPeer).Kind)
	// And without flow control a sender is not held to any credit.
	big := make([]byte, routebroker.MaxFramePayload)
	for range routebroker.InitialWindow/len(big) + 2 {
		writeFrame(t, pair.conPeer, routebroker.Frame{Kind: routebroker.KindData, Stream: 1, Payload: big})
		require.Equal(t, routebroker.KindData, readFrame(t, pair.pubPeer).Kind)
	}
}

func TestRecvWindowGrantsInQuarterWindows(t *testing.T) {
	r, initial := routebroker.NewRecvWindow(1 << 20)
	require.Equal(t, 1<<20-routebroker.InitialWindow, initial)
	require.NoError(t, r.Received(1<<20))
	require.ErrorIs(t, r.Received(1), routebroker.ErrFlowViolation)
	require.Zero(t, r.Consumed(1<<18-1))
	require.Equal(t, 1<<18, r.Consumed(1))
	require.NoError(t, r.Received(1<<18))
	require.ErrorIs(t, r.Received(1), routebroker.ErrFlowViolation)

	_, initial = routebroker.NewRecvWindow(1)
	require.Zero(t, initial, "a window below the implicit one is clamped up to it")
}

func TestSendWindowBlocksUntilGranted(t *testing.T) {
	w := routebroker.NewSendWindow()
	n, err := w.Wait(1 << 30)
	require.NoError(t, err)
	require.Equal(t, routebroker.InitialWindow, n)
	w.Spend(n)

	got := make(chan int, 1)
	go func() {
		n, err := w.Wait(100)
		if err != nil {
			n = -1
		}
		got <- n
	}()
	select {
	case <-got:
		t.Fatal("Wait returned without credit")
	case <-time.After(50 * time.Millisecond):
	}
	require.NoError(t, w.Grant(500))
	require.Equal(t, 100, <-got)

	w.Spend(500)
	go func() {
		_, err := w.Wait(1)
		got <- map[bool]int{true: -1, false: 0}[err != nil]
	}()
	w.Close()
	require.Equal(t, -1, <-got, "Close releases a blocked sender")
}
