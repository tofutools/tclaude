// Package stream is the end-to-end encrypted byte stream two instances run
// over a hub stream relay (client.DialStream).
//
// Keys come from an ephemeral X25519 exchange carried in sealed envelopes
// (so each side's ephemeral key is authenticated by its pinned identity and
// hidden from the hub): key = HKDF-SHA256(X25519(eph, peer_eph), salt =
// stream id, info = "tclaude-fed-stream-v1" || initiator_eph ||
// responder_eph), split into one ChaCha20-Poly1305 key per direction. Every
// websocket message is one record sealed under a counter nonce, so the hub
// cannot read, reorder, replay, drop or inject records without the stream
// failing. A sender ends its direction with an authenticated FIN record; a
// stream that ends without one reads as io.ErrUnexpectedEOF.
package stream

import (
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/hkdf"

	"github.com/tofutools/tclaude/pkg/federation/proto"
)

// KeyPair is one side's ephemeral X25519 key for a single stream.
type KeyPair struct {
	priv []byte
	Pub  []byte
}

// NewKeyPair draws a fresh ephemeral key.
func NewKeyPair() (*KeyPair, error) {
	priv := make([]byte, curve25519.ScalarSize)
	if _, err := rand.Read(priv); err != nil {
		return nil, err
	}
	pub, err := curve25519.X25519(priv, curve25519.Basepoint)
	if err != nil {
		return nil, err
	}
	return &KeyPair{priv: priv, Pub: pub}, nil
}

// Keys are the per-direction stream keys.
type Keys struct {
	send, recv []byte
}

// DeriveKeys computes this side's send/receive keys for stream sid.
func DeriveKeys(kp *KeyPair, peerPub []byte, sid string, initiator bool) (*Keys, error) {
	if len(peerPub) != curve25519.PointSize {
		return nil, errors.New("bad peer stream key")
	}
	shared, err := curve25519.X25519(kp.priv, peerPub)
	if err != nil {
		return nil, err
	}
	ini, res := kp.Pub, peerPub
	if !initiator {
		ini, res = peerPub, kp.Pub
	}
	info := append([]byte("tclaude-fed-stream-v1"), ini...)
	info = append(info, res...)
	okm := make([]byte, 2*chacha20poly1305.KeySize)
	if _, err := io.ReadFull(hkdf.New(sha256.New, shared, []byte(sid), info), okm); err != nil {
		return nil, err
	}
	i2r, r2i := okm[:chacha20poly1305.KeySize], okm[chacha20poly1305.KeySize:]
	if initiator {
		return &Keys{send: i2r, recv: r2i}, nil
	}
	return &Keys{send: r2i, recv: i2r}, nil
}

const (
	recData  byte = 0
	recFin   byte = 1
	recReset byte = 2
	// writeTimeout bounds one record's write through the relay. A write
	// blocks while the far end's route flow control holds the stream, and
	// the hub's own stream-idle bound decides when that is too long; this
	// only catches a hub that stopped reading altogether.
	writeTimeout = 10 * time.Minute
	// maxChunk leaves room for the record header and AEAD tag.
	maxChunk = proto.MaxStreamMessage - 64
)

// Conn is an encrypted stream over a paired hub websocket.
type Conn struct {
	ws         *websocket.Conn
	send, recv cipher.AEAD

	wmu     sync.Mutex
	sendSeq uint64
	finSent bool

	rmu     sync.Mutex
	recvSeq uint64
	rbuf    []byte
	rerr    error

	closeOnce sync.Once
}

// New wraps ws (after stream_ready) with keys.
func New(ws *websocket.Conn, k *Keys) (*Conn, error) {
	s, err := chacha20poly1305.New(k.send)
	if err != nil {
		return nil, err
	}
	r, err := chacha20poly1305.New(k.recv)
	if err != nil {
		return nil, err
	}
	return &Conn{ws: ws, send: s, recv: r}, nil
}

func nonce(seq uint64) []byte {
	n := make([]byte, chacha20poly1305.NonceSize)
	binary.BigEndian.PutUint64(n[4:], seq)
	return n
}

// ErrTampered is returned when a record fails authentication.
var ErrTampered = errors.New("stream record failed authentication")

// ErrReset is returned when the peer aborted the stream: its side of the
// connection was reset rather than finished.
var ErrReset = errors.New("stream reset by peer")

func (c *Conn) Read(p []byte) (int, error) {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	for len(c.rbuf) == 0 {
		if c.rerr != nil {
			return 0, c.rerr
		}
		mt, msg, err := c.ws.ReadMessage()
		if err != nil {
			c.rerr = io.ErrUnexpectedEOF
			continue
		}
		if mt != websocket.BinaryMessage {
			c.rerr = ErrTampered
			continue
		}
		pt, err := c.recv.Open(nil, nonce(c.recvSeq), msg, nil)
		if err != nil || len(pt) == 0 {
			c.rerr = ErrTampered
			continue
		}
		c.recvSeq++
		switch pt[0] {
		case recFin:
			c.rerr = io.EOF
		case recReset:
			c.rerr = ErrReset
		case recData:
			c.rbuf = pt[1:]
		default:
			c.rerr = ErrTampered
		}
	}
	n := copy(p, c.rbuf)
	c.rbuf = c.rbuf[n:]
	return n, nil
}

func (c *Conn) writeRecord(typ byte, data []byte) error {
	pt := make([]byte, 1+len(data))
	pt[0] = typ
	copy(pt[1:], data)
	ct := c.send.Seal(nil, nonce(c.sendSeq), pt, nil)
	c.sendSeq++
	if typ != recReset {
		_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
	}
	return c.ws.WriteMessage(websocket.BinaryMessage, ct)
}

func (c *Conn) Write(p []byte) (int, error) {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.finSent {
		return 0, net.ErrClosed
	}
	n := 0
	for len(p) > 0 {
		chunk := p
		if len(chunk) > maxChunk {
			chunk = chunk[:maxChunk]
		}
		if err := c.writeRecord(recData, chunk); err != nil {
			return n, err
		}
		n += len(chunk)
		p = p[len(chunk):]
	}
	return n, nil
}

// CloseWrite ends this side's direction with an authenticated FIN.
func (c *Conn) CloseWrite() error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.finSent {
		return nil
	}
	c.finSent = true
	return c.writeRecord(recFin, nil)
}

// Drain keeps reading after Read has returned io.EOF, so keepalives are
// still answered and the end of the peer's side is noticed. It returns
// ErrReset if the peer aborts, ErrTampered on a record after FIN, and
// io.ErrUnexpectedEOF once the websocket ends.
func (c *Conn) Drain() error {
	c.rmu.Lock()
	defer c.rmu.Unlock()
	if c.rerr != io.EOF {
		return c.rerr
	}
	mt, msg, err := c.ws.ReadMessage()
	if err != nil {
		return io.ErrUnexpectedEOF
	}
	if mt != websocket.BinaryMessage {
		return ErrTampered
	}
	pt, err := c.recv.Open(nil, nonce(c.recvSeq), msg, nil)
	if err != nil || len(pt) == 0 {
		return ErrTampered
	}
	c.recvSeq++
	if pt[0] == recReset {
		return ErrReset
	}
	return ErrTampered
}

// Close closes the stream. After CloseWrite it is an orderly close;
// otherwise this side is aborted and the peer reads ErrReset, never a
// clean end of stream.
func (c *Conn) Close() error {
	var err error
	c.closeOnce.Do(func() {
		// A write blocked on a throttled relay holds wmu; then just drop
		// the websocket, which the peer also reads as an abort.
		if c.wmu.TryLock() {
			if !c.finSent {
				c.finSent = true
				_ = c.ws.SetWriteDeadline(time.Now().Add(time.Second))
				_ = c.writeRecord(recReset, nil)
			}
			c.wmu.Unlock()
		}
		_ = c.ws.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		err = c.ws.Close()
	})
	return err
}

func (c *Conn) LocalAddr() net.Addr                { return c.ws.LocalAddr() }
func (c *Conn) RemoteAddr() net.Addr               { return c.ws.RemoteAddr() }
func (c *Conn) SetReadDeadline(t time.Time) error  { return c.ws.SetReadDeadline(t) }
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.ws.SetWriteDeadline(t) }
func (c *Conn) SetDeadline(t time.Time) error {
	if err := c.ws.SetReadDeadline(t); err != nil {
		return err
	}
	return c.ws.SetWriteDeadline(t)
}
