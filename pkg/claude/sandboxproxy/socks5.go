package sandboxproxy

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/netip"
	"time"
)

// RFC 1928 wire constants.
const (
	socks5Version = 0x05

	socks5AuthNone         = 0x00
	socks5AuthNoAcceptable = 0xFF

	socks5CmdConnect      = 0x01
	socks5CmdBind         = 0x02
	socks5CmdUDPAssociate = 0x03

	socks5ATYPIPv4   = 0x01
	socks5ATYPDomain = 0x03
	socks5ATYPIPv6   = 0x04

	socks5ReplySucceeded           = 0x00
	socks5ReplyGeneralFailure      = 0x01
	socks5ReplyNotAllowedByRuleset = 0x02
	socks5ReplyHostUnreachable     = 0x04
	socks5ReplyCommandNotSupported = 0x07
	socks5ReplyATYPNotSupported    = 0x08
)

// serveSOCKS5 carries the SOCKS5 CONNECT command with the no-authentication
// method.
//
// No authentication is offered on purpose. The listener is reachable only from
// inside the sandbox — an empty netns on Linux, a port-exact Seatbelt exception
// on macOS — so a proxy credential would answer no threat, and a credential
// placed in the sandbox environment is readable by the very process it would
// authenticate.
func (s *Server) serveSOCKS5(conn net.Conn, reader *bufio.Reader) {
	if err := socks5Greet(conn, reader); err != nil {
		s.reportError(CarriageSOCKS5, err)
		return
	}
	command, target, failure, err := socks5ReadRequest(reader)
	if err != nil {
		s.reportError(CarriageSOCKS5, err)
		socks5WriteReply(conn, failure)
		return
	}
	if command != socks5CmdConnect {
		// UDP ASSOCIATE is deliberately out of v1: the floor carries no
		// datagrams, and a relay would reopen a resolver channel this posture
		// does not have. Answering with the protocol's own "command not
		// supported" lets a cooperating client fall back to TCP instead of
		// failing opaquely.
		s.reportError(CarriageSOCKS5, fmt.Errorf(
			"socks5 command 0x%02x is not supported", command))
		socks5WriteReply(conn, socks5ReplyCommandNotSupported)
		return
	}
	upstream, decision, dialErr := s.connect(s.baseCtx, CarriageSOCKS5, target)
	if !decision.Allowed() {
		// 0x02 is the protocol's exact word for a policy refusal, and
		// well-behaved clients surface it verbatim.
		socks5WriteReply(conn, socks5ReplyNotAllowedByRuleset)
		return
	}
	if dialErr != nil {
		socks5WriteReply(conn, socks5ReplyHostUnreachable)
		return
	}
	defer func() { _ = upstream.Close() }()
	if err := socks5WriteConnectReply(conn, upstream.LocalAddr()); err != nil {
		return
	}
	_ = conn.SetDeadline(time.Time{})
	pipe(&bufferedConn{Conn: conn, reader: reader}, upstream)
}

// socks5Greet performs the method-negotiation exchange.
func socks5Greet(conn net.Conn, reader *bufio.Reader) error {
	header := make([]byte, 2)
	if _, err := io.ReadFull(reader, header); err != nil {
		return fmt.Errorf("read socks5 greeting: %w", err)
	}
	if header[0] != socks5Version {
		return fmt.Errorf("socks version 0x%02x is not supported", header[0])
	}
	count := int(header[1])
	if count == 0 {
		return fmt.Errorf("socks5 greeting offers no methods")
	}
	methods := make([]byte, count)
	if _, err := io.ReadFull(reader, methods); err != nil {
		return fmt.Errorf("read socks5 methods: %w", err)
	}
	for _, method := range methods {
		if method == socks5AuthNone {
			_, err := conn.Write([]byte{socks5Version, socks5AuthNone})
			return err
		}
	}
	_, _ = conn.Write([]byte{socks5Version, socks5AuthNoAcceptable})
	return fmt.Errorf("socks5 client offers no acceptable authentication method")
}

// socks5ReadRequest parses one request into the same Target both carriages
// produce. ATYP=DOMAINNAME is a name target, exactly as an HTTP CONNECT host;
// ATYP=IPV4/IPV6 is a literal target, exactly as an HTTP CONNECT to a literal.
// The returned reply code is the RFC 1928 answer to send when err is non-nil,
// so a malformed request is refused in the protocol's own vocabulary rather
// than as a generic failure.
func socks5ReadRequest(reader *bufio.Reader) (byte, Target, byte, error) {
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return 0, Target{}, socks5ReplyGeneralFailure, fmt.Errorf("read socks5 request: %w", err)
	}
	if header[0] != socks5Version {
		return 0, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
			"socks version 0x%02x is not supported", header[0])
	}
	if header[2] != 0x00 {
		return 0, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
			"socks5 request reserved byte is not zero")
	}
	command := header[1]
	var host string
	switch header[3] {
	case socks5ATYPIPv4:
		raw := make([]byte, 4)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
				"read socks5 address: %w", err)
		}
		addr, _ := netip.AddrFromSlice(raw)
		host = addr.String()
	case socks5ATYPIPv6:
		raw := make([]byte, 16)
		if _, err := io.ReadFull(reader, raw); err != nil {
			return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
				"read socks5 address: %w", err)
		}
		addr, _ := netip.AddrFromSlice(raw)
		host = addr.Unmap().String()
	case socks5ATYPDomain:
		length, err := reader.ReadByte()
		if err != nil {
			return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
				"read socks5 name length: %w", err)
		}
		if length == 0 {
			return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
				"socks5 request has an empty name")
		}
		raw := make([]byte, int(length))
		if _, err := io.ReadFull(reader, raw); err != nil {
			return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
				"read socks5 name: %w", err)
		}
		host = string(raw)
	default:
		return command, Target{}, socks5ReplyATYPNotSupported, fmt.Errorf(
			"socks5 address type 0x%02x is not supported", header[3])
	}
	portRaw := make([]byte, 2)
	if _, err := io.ReadFull(reader, portRaw); err != nil {
		return command, Target{}, socks5ReplyGeneralFailure, fmt.Errorf(
			"read socks5 port: %w", err)
	}
	port := int(binary.BigEndian.Uint16(portRaw))
	// A command this proxy will refuse still needs its address consumed, so the
	// refusal reply lands on a synchronized stream.
	if command != socks5CmdConnect {
		return command, Target{}, socks5ReplyCommandNotSupported, nil
	}
	target, err := ParseTarget(host, port)
	if err != nil {
		return command, Target{}, socks5ReplyGeneralFailure, err
	}
	return command, target, socks5ReplySucceeded, nil
}

// socks5WriteReply answers with a bound address of 0.0.0.0:0, which RFC 1928
// permits when no address is meaningful.
func socks5WriteReply(conn net.Conn, reply byte) {
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	_, _ = conn.Write([]byte{
		socks5Version, reply, 0x00, socks5ATYPIPv4,
		0, 0, 0, 0,
		0, 0,
	})
}

// socks5WriteConnectReply reports the address the upstream connection was made
// from, as RFC 1928 specifies for a succeeded CONNECT.
func socks5WriteConnectReply(conn net.Conn, local net.Addr) error {
	reply := []byte{socks5Version, socks5ReplySucceeded, 0x00}
	tcpAddr, ok := local.(*net.TCPAddr)
	if !ok {
		socks5WriteReply(conn, socks5ReplySucceeded)
		return nil
	}
	addr, ok := netip.AddrFromSlice(tcpAddr.IP)
	if !ok {
		socks5WriteReply(conn, socks5ReplySucceeded)
		return nil
	}
	addr = addr.Unmap()
	if addr.Is4() {
		v4 := addr.As4()
		reply = append(reply, socks5ATYPIPv4)
		reply = append(reply, v4[:]...)
	} else {
		v6 := addr.As16()
		reply = append(reply, socks5ATYPIPv6)
		reply = append(reply, v6[:]...)
	}
	if tcpAddr.Port < 0 || tcpAddr.Port > 65535 {
		socks5WriteReply(conn, socks5ReplySucceeded)
		return nil
	}
	reply = binary.BigEndian.AppendUint16(reply, uint16(tcpAddr.Port))
	_ = conn.SetDeadline(time.Now().Add(handshakeTimeout))
	_, err := conn.Write(reply)
	return err
}
