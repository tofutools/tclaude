// Command group-route-feasibility is a bounded, non-production probe for
// TCL-947. It intentionally lives under scripts: it exercises the platform
// primitives without adding a route capability to tclaude.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	modeFlag = "mode"

	linuxMode         = "linux"
	linuxPublisher    = "linux-publisher"
	linuxConsumer     = "linux-consumer"
	linuxUnauthorized = "linux-unauthorized"

	darwinMode       = "darwin"
	darwinCollision  = "darwin-collision"
	darwinPublisher  = "darwin-publisher"
	darwinConsumer   = "darwin-consumer"
	darwinLimitation = "darwin-limitation"

	groupRouteName = "api"
	groupName      = "group-a"
	brokerSocket   = "/route/broker.sock"

	probeTimeout = 30 * time.Second
)

var opaquePayload = []byte{0x00, 0xff, 0x10, 0x0a, 0x7f, 0x01, 0xfe, 0x42, 0x00, 0x80, 0x19, 0xc3, 0x5a}

func main() {
	mode := flag.String(modeFlag, "", "probe mode")
	port := flag.Int("port", 0, "port for a platform helper")
	brokerPort := flag.Int("broker-port", 0, "broker port for a platform helper")
	neighborPort := flag.Int("neighbor-port", 0, "denied neighboring port")
	endpoint := flag.String("endpoint", "", "host endpoint for a helper")
	marker := flag.String("marker", "", "marker path for a helper")
	shared := flag.String("shared", "", "shared host directory")
	flag.Parse()

	var err error
	switch *mode {
	case linuxMode:
		err = runLinux()
	case linuxPublisher:
		err = runLinuxPublisher(*marker, *port, *endpoint)
	case linuxConsumer:
		err = runLinuxConsumer(*marker)
	case linuxUnauthorized:
		err = runLinuxUnauthorized(*marker)
	case darwinMode:
		err = runDarwin()
	case darwinCollision:
		err = runDarwinCollision(*marker, *port)
	case darwinPublisher:
		err = runDarwinPublisher(*marker, *port)
	case darwinConsumer:
		err = runDarwinConsumer(*marker, *brokerPort, *neighborPort)
	case darwinLimitation:
		err = runDarwinLimitation(*marker, *endpoint)
	default:
		err = fmt.Errorf("unknown --%s %q", modeFlag, *mode)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "group-route-feasibility: %v\n", err)
		os.Exit(1)
	}
	_ = shared // kept as a documented launch argument for future probes
}

func runLinux() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("Linux probe requires Linux, got %s", runtime.GOOS)
	}
	if _, err := exec.LookPath("bwrap"); err != nil {
		return fmt.Errorf("bwrap is required: %w", err)
	}

	root, err := os.MkdirTemp("", "tclaude-group-route-linux-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	if err := os.Chmod(root, 0o700); err != nil {
		return err
	}
	executable, err := copyExecutable(root, os.Args[0])
	if err != nil {
		return err
	}

	hostControl, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("host control listener: %w", err)
	}
	defer hostControl.Close()
	broker, err := newLinuxBroker(filepath.Join(root, "broker.sock"))
	if err != nil {
		return err
	}
	defer broker.Close()

	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	hostNS, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return fmt.Errorf("read host network namespace: %w", err)
	}
	hostPort := hostControl.Addr().(*net.TCPAddr).Port

	pubMarker := filepath.Join(root, "publisher.marker")
	consumerMarker := filepath.Join(root, "consumer.marker")
	unauthorizedMarker := filepath.Join(root, "unauthorized.marker")
	pub := startBwrap(ctx, executable, root, linuxPublisher, pubMarker, 0, 0, hostPort)
	if err := waitForMarker(ctx, pubMarker); err != nil {
		return finishChild("publisher", pub, err)
	}
	if err := assertLinuxNamespace(pubMarker, hostNS, "publisher"); err != nil {
		return finishChild("publisher", pub, err)
	}
	pubEndpoint, err := markerValue(pubMarker, "endpoint")
	if err != nil {
		return finishChild("publisher", pub, err)
	}
	if err := hostCannotReach(pubEndpoint); err != nil {
		return finishChild("publisher", pub, err)
	}

	consumer := startBwrap(ctx, executable, root, linuxConsumer, consumerMarker, 0, 0, hostPort)
	unauthorized := startBwrap(ctx, executable, root, linuxUnauthorized, unauthorizedMarker, 0, 0, hostPort)
	if err := waitForMarker(ctx, consumerMarker); err != nil {
		return finishChildren(pub, consumer, unauthorized, err)
	}
	if err := waitForMarker(ctx, unauthorizedMarker); err != nil {
		return finishChildren(pub, consumer, unauthorized, err)
	}
	if err := assertLinuxNamespace(consumerMarker, hostNS, "consumer"); err != nil {
		return finishChildren(pub, consumer, unauthorized, err)
	}
	if err := assertLinuxNamespace(unauthorizedMarker, hostNS, "third-group"); err != nil {
		return finishChildren(pub, consumer, unauthorized, err)
	}
	pubNS, _ := markerValue(pubMarker, "netns")
	consumerNS, _ := markerValue(consumerMarker, "netns")
	unauthorizedNS, _ := markerValue(unauthorizedMarker, "netns")
	if pubNS == consumerNS || pubNS == unauthorizedNS || consumerNS == unauthorizedNS {
		return finishChildren(pub, consumer, unauthorized, fmt.Errorf("bwrap helpers did not receive distinct network namespaces: %q %q %q", pubNS, consumerNS, unauthorizedNS))
	}

	if err := waitChild("publisher", pub); err != nil {
		return finishChildren(nil, consumer, unauthorized, err)
	}
	if err := waitChild("consumer", consumer); err != nil {
		return finishChildren(nil, nil, unauthorized, err)
	}
	if err := waitChild("third-group", unauthorized); err != nil {
		return err
	}

	fmt.Printf("TCL-947 Linux evidence: POSITIVE\n")
	fmt.Printf("linux namespaces: publisher=%s consumer=%s third-group=%s host=%s\n", pubNS, consumerNS, unauthorizedNS, hostNS)
	fmt.Printf("linux route: opaque TCP stream carried through host Unix broker; consumer endpoint was created after launch\n")
	fmt.Printf("linux authorization: third group denied; direct publisher endpoint and host control endpoint unavailable from namespaces\n")
	fmt.Printf("linux policy floor: --unshare-all; no namespace join, bridge, nftables rule, or ambient peer network\n")
	return nil
}

func runLinuxPublisher(marker string, _ int, hostEndpoint string) error {
	if marker == "" {
		return fmt.Errorf("publisher marker is required")
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	app, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("publisher post-launch bind: %w", err)
	}
	defer app.Close()
	endpoint := app.Addr().String()
	if err := writeMarker(marker, map[string]string{"netns": ns, "endpoint": endpoint, "bind": "pass"}); err != nil {
		return err
	}
	if err := requireUnavailable(hostEndpoint, "host control"); err != nil {
		return err
	}
	if err := requireUnavailable("1.1.1.1:443", "Internet"); err != nil {
		return err
	}
	broker, err := net.DialTimeout("unix", brokerSocket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("publisher broker connect: %w", err)
	}
	defer broker.Close()
	reader := bufio.NewReader(broker)
	if _, err := fmt.Fprintf(broker, "PUBLISH %s %s\n", groupName, groupRouteName); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "PUBLISHED" {
		return fmt.Errorf("publisher registration rejected: %q (%v)", strings.TrimSpace(line), err)
	}
	line, err = reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "CONNECT" {
		return fmt.Errorf("publisher connect request missing: %q (%v)", strings.TrimSpace(line), err)
	}
	appConn, err := net.DialTimeout("tcp4", endpoint, 5*time.Second)
	if err != nil {
		return fmt.Errorf("publisher namespace-local helper dial: %w", err)
	}
	defer appConn.Close()
	if _, err := fmt.Fprintln(broker, "STREAM"); err != nil {
		return err
	}
	if err := relay(appConn, broker); err != nil {
		return err
	}
	return writeMarker(marker, map[string]string{
		"netns": ns, "endpoint": endpoint, "bind": "pass",
		"host-policy": "refused", "internet": "refused", "route": "publisher",
	})
}

func runLinuxConsumer(marker string) error {
	if marker == "" {
		return fmt.Errorf("consumer marker is required")
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	local, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return fmt.Errorf("consumer post-launch bind: %w", err)
	}
	defer local.Close()
	if err := writeMarker(marker, map[string]string{"netns": ns, "endpoint": local.Addr().String(), "bind": "pass"}); err != nil {
		return err
	}
	clientDone := make(chan error, 1)
	go func() {
		conn, dialErr := net.DialTimeout("tcp4", local.Addr().String(), 5*time.Second)
		if dialErr != nil {
			clientDone <- dialErr
			return
		}
		defer conn.Close()
		if _, writeErr := conn.Write(opaquePayload); writeErr != nil {
			clientDone <- writeErr
			return
		}
		expected := responseFor(opaquePayload)
		got := make([]byte, len(expected))
		_, readErr := io.ReadFull(conn, got)
		if readErr == nil && string(got) != string(expected) {
			readErr = fmt.Errorf("opaque route response mismatch: got %x want %x", got, expected)
		}
		clientDone <- readErr
	}()
	localConn, err := local.Accept()
	if err != nil {
		return err
	}
	defer localConn.Close()
	broker, err := net.DialTimeout("unix", brokerSocket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("consumer broker connect: %w", err)
	}
	defer broker.Close()
	reader := bufio.NewReader(broker)
	if _, err := fmt.Fprintf(broker, "OPEN %s %s\n", groupName, groupRouteName); err != nil {
		return err
	}
	line, err := reader.ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "CONNECT" {
		return fmt.Errorf("consumer route open rejected: %q (%v)", strings.TrimSpace(line), err)
	}
	if _, err := fmt.Fprintln(broker, "STREAM"); err != nil {
		return err
	}
	if err := relay(localConn, broker); err != nil {
		return err
	}
	if err := <-clientDone; err != nil {
		return err
	}
	return writeMarker(marker, map[string]string{
		"netns": ns, "endpoint": local.Addr().String(), "bind": "pass", "roundtrip": "pass",
	})
}

func runLinuxUnauthorized(marker string) error {
	if marker == "" {
		return fmt.Errorf("unauthorized marker is required")
	}
	ns, err := os.Readlink("/proc/self/ns/net")
	if err != nil {
		return err
	}
	if err := writeMarker(marker, map[string]string{"netns": ns}); err != nil {
		return err
	}
	broker, err := net.DialTimeout("unix", brokerSocket, 5*time.Second)
	if err != nil {
		return err
	}
	defer broker.Close()
	if _, err := fmt.Fprintf(broker, "OPEN group-b %s\n", groupRouteName); err != nil {
		return err
	}
	line, err := bufio.NewReader(broker).ReadString('\n')
	if err != nil {
		return err
	}
	if strings.TrimSpace(line) != "DENY unauthorized-group" {
		return fmt.Errorf("third group unexpectedly opened route: %q", strings.TrimSpace(line))
	}
	return writeMarker(marker, map[string]string{"netns": ns, "authorization": "denied"})
}

type linuxBroker struct {
	listener  net.Listener
	mu        sync.Mutex
	pub       *linuxPeer
	ready     chan struct{}
	pubStream chan *linuxPeer
	closed    chan struct{}
}

type linuxPeer struct {
	conn       net.Conn
	reader     *bufio.Reader
	connectReq chan struct{}
}

func newLinuxBroker(socket string) (*linuxBroker, error) {
	_ = os.Remove(socket)
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, fmt.Errorf("broker Unix listener: %w", err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		listener.Close()
		return nil, err
	}
	b := &linuxBroker{
		listener:  listener,
		ready:     make(chan struct{}),
		pubStream: make(chan *linuxPeer, 1),
		closed:    make(chan struct{}),
	}
	go b.serve()
	return b, nil
}

func (b *linuxBroker) serve() {
	for {
		conn, err := b.listener.Accept()
		if err != nil {
			select {
			case <-b.closed:
				return
			default:
			}
			continue
		}
		go b.handle(&linuxPeer{conn: conn, reader: bufio.NewReader(conn), connectReq: make(chan struct{}, 1)})
	}
}

func (b *linuxBroker) handle(peer *linuxPeer) {
	defer peer.conn.Close()
	line, err := peer.reader.ReadString('\n')
	if err != nil {
		return
	}
	fields := strings.Fields(line)
	if len(fields) != 3 {
		return
	}
	switch fields[0] {
	case "PUBLISH":
		if fields[1] != groupName || fields[2] != groupRouteName {
			return
		}
		b.mu.Lock()
		if b.pub != nil {
			b.mu.Unlock()
			return
		}
		b.pub = peer
		close(b.ready)
		b.mu.Unlock()
		if _, err := fmt.Fprintln(peer.conn, "PUBLISHED"); err != nil {
			return
		}
		<-peer.connectReq
		if _, err := fmt.Fprintln(peer.conn, "CONNECT"); err != nil {
			return
		}
		line, err = peer.reader.ReadString('\n')
		if err != nil || strings.TrimSpace(line) != "STREAM" {
			return
		}
		b.pubStream <- peer
	case "OPEN":
		if fields[1] != groupName || fields[2] != groupRouteName {
			_, _ = fmt.Fprintln(peer.conn, "DENY unauthorized-group")
			return
		}
		select {
		case <-b.ready:
		case <-time.After(5 * time.Second):
			_, _ = fmt.Fprintln(peer.conn, "DENY publisher-not-ready")
			return
		}
		b.mu.Lock()
		pub := b.pub
		b.mu.Unlock()
		if pub == nil {
			return
		}
		pub.connectReq <- struct{}{}
		select {
		case stream := <-b.pubStream:
			if _, err := fmt.Fprintln(peer.conn, "CONNECT"); err != nil {
				return
			}
			line, err := peer.reader.ReadString('\n')
			if err != nil || strings.TrimSpace(line) != "STREAM" {
				return
			}
			_ = relayPeers(stream, peer)
		case <-time.After(5 * time.Second):
			return
		}
	}
}

func (b *linuxBroker) Close() error {
	select {
	case <-b.closed:
	default:
		close(b.closed)
	}
	return b.listener.Close()
}

func relayPeers(publisher, consumer *linuxPeer) error {
	return relayReaders(publisher.conn, publisher.reader, consumer.conn, consumer.reader)
}

func relay(left, right net.Conn) error {
	return relayReaders(left, bufio.NewReader(left), right, bufio.NewReader(right))
}

func relayReaders(left net.Conn, leftReader *bufio.Reader, right net.Conn, rightReader *bufio.Reader) error {
	errs := make(chan error, 2)
	go func() { _, err := io.Copy(left, rightReader); errs <- err }()
	go func() { _, err := io.Copy(right, leftReader); errs <- err }()
	first := <-errs
	_ = left.Close()
	_ = right.Close()
	second := <-errs
	if first != nil && !errors.Is(first, net.ErrClosed) {
		return first
	}
	if second != nil && !errors.Is(second, net.ErrClosed) {
		return second
	}
	return nil
}

func startBwrap(ctx context.Context, executable, shared, mode, marker string, port, brokerPort, hostPort int) *exec.Cmd {
	marker = filepath.Join("/route", filepath.Base(marker))
	args := []string{
		"--die-with-parent", "--unshare-all",
		"--ro-bind", "/", "/",
		"--dev", "/dev", "--proc", "/proc", "--tmpfs", "/tmp",
		"--dir", "/route", "--bind", shared, "/route", "--chdir", "/",
		"--", "/route/probe", "--mode", mode, "--marker", marker,
		"--port", strconv.Itoa(port), "--broker-port", strconv.Itoa(brokerPort),
		"--endpoint", fmt.Sprintf("127.0.0.1:%d", hostPort),
	}
	cmd := exec.CommandContext(ctx, "bwrap", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n", mode, err)
	}
	return cmd
}

func runDarwin() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("Darwin probe requires macOS, got %s", runtime.GOOS)
	}
	const poolSize = 8
	if _, err := os.Stat("/usr/bin/sandbox-exec"); err != nil {
		return fmt.Errorf("sandbox-exec is required: %w", err)
	}
	root, err := os.MkdirTemp("", "tclaude-group-route-darwin-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(root)
	executable, err := copyExecutable(root, os.Args[0])
	if err != nil {
		return err
	}
	pool, err := reservePortPool(poolSize)
	if err != nil {
		return err
	}
	portSet := make(map[int]bool, len(pool))
	for _, port := range pool {
		portSet[port] = true
	}
	publisherPort := pool[0]
	consumerPort := pool[1]
	neighborPort := consumerPort + 1
	for portSet[neighborPort] {
		neighborPort++
	}
	limitationPort := pool[2]
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	profile := darwinProfile(pool, publisherPort, true)
	if got := strings.Count(profile, "(allow network-outbound (remote tcp \"localhost:"); got != poolSize {
		return fmt.Errorf("rendered profile has %d exact TCP outbound slots, want %d", got, poolSize)
	}
	fmt.Printf("TCL-947 Darwin profile: POSITIVE exact-port pool=%d rendered-slots=%d\n", len(pool), strings.Count(profile, "localhost:"))

	// A broker-held consumer endpoint is reserved before the consumer starts.
	broker, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(consumerPort)))
	if err != nil {
		return fmt.Errorf("broker-held consumer endpoint %d: %w", consumerPort, err)
	}
	defer broker.Close()
	if _, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(consumerPort))); !isErrno(err, syscall.EADDRINUSE) {
		return fmt.Errorf("consumer endpoint reservation was not observed as EADDRINUSE: %v", err)
	}

	collision, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(publisherPort)))
	if err != nil {
		return fmt.Errorf("publisher collision fixture: %w", err)
	}
	collisionMarker := filepath.Join(root, "collision.marker")
	collisionCmd := startSandbox(ctx, executable, profile, darwinCollision, collisionMarker, publisherPort, 0, neighborPort, "")
	if err := waitForMarker(ctx, collisionMarker); err != nil {
		collision.Close()
		return finishChild("Darwin collision", collisionCmd, err)
	}
	if err := waitChild("Darwin collision", collisionCmd); err != nil {
		collision.Close()
		return err
	}
	collision.Close()
	if value, _ := markerValue(collisionMarker, "collision"); value != "EADDRINUSE" {
		return fmt.Errorf("publisher collision result %q, want EADDRINUSE", value)
	}
	fmt.Printf("TCL-947 Darwin collision: POSITIVE publisher slot collision=%s; no workaround invented\n", "EADDRINUSE")

	publisherMarker := filepath.Join(root, "publisher.marker")
	publisherCmd := startSandbox(ctx, executable, profile, darwinPublisher, publisherMarker, publisherPort, consumerPort, neighborPort, "")
	if err := waitForMarker(ctx, publisherMarker); err != nil {
		return finishChild("Darwin publisher", publisherCmd, err)
	}

	brokerDone := make(chan error, 1)
	go func() {
		conn, acceptErr := broker.Accept()
		if acceptErr != nil {
			brokerDone <- acceptErr
			return
		}
		defer conn.Close()
		publisher, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(publisherPort)), 5*time.Second)
		if dialErr != nil {
			brokerDone <- dialErr
			return
		}
		defer publisher.Close()
		brokerDone <- relay(conn, publisher)
	}()

	consumerMarker := filepath.Join(root, "consumer.marker")
	consumerProfile := darwinProfile(pool, publisherPort, false)
	consumerCmd := startSandbox(ctx, executable, consumerProfile, darwinConsumer, consumerMarker, 0, consumerPort, neighborPort, "")
	if err := waitChild("Darwin consumer", consumerCmd); err != nil {
		return finishChild("Darwin publisher", publisherCmd, err)
	}
	if err := <-brokerDone; err != nil {
		return finishChild("Darwin publisher", publisherCmd, fmt.Errorf("broker route: %w", err))
	}
	if err := waitChild("Darwin publisher", publisherCmd); err != nil {
		return err
	}

	limitationListener, limitationEndpoint, err := listenNonLoopback(limitationPort)
	if err != nil {
		return err
	}
	defer limitationListener.Close()
	go serveOneEcho(limitationListener)
	limitationMarker := filepath.Join(root, "limitation.marker")
	limitationCmd := startSandbox(ctx, executable, consumerProfile, darwinLimitation, limitationMarker, 0, 0, neighborPort, limitationEndpoint)
	if err := waitChild("Darwin localhost limitation", limitationCmd); err != nil {
		return err
	}
	if value, _ := markerValue(limitationMarker, "same-port"); value != "reachable" {
		return fmt.Errorf("localhost same-port limitation result %q", value)
	}
	fmt.Printf("TCL-947 Darwin localhost: LIMITATION Seatbelt localhost:<port> reached non-loopback %s\n", limitationEndpoint)
	fmt.Printf("TCL-947 Darwin evidence: POSITIVE broker-held consumer endpoint=%d reached publisher slot=%d with opaque TCP bytes\n", consumerPort, publisherPort)
	fmt.Printf("TCL-947 Darwin negative: non-reserved neighbor=%d and external TCP were refused by Seatbelt\n", neighborPort)
	fmt.Printf("TCL-947 Darwin contract: bounded exact pool is practical at %d slots; publisher ports require application cooperation; consumer slots are broker-reserved\n", poolSize)
	return nil
}

func runDarwinCollision(marker string, port int) error {
	if marker == "" || port == 0 {
		return fmt.Errorf("collision marker and port are required")
	}
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err == nil {
		listener.Close()
		return writeMarker(marker, map[string]string{"collision": "unexpectedly-free"})
	}
	if !isErrno(err, syscall.EADDRINUSE) {
		return writeMarker(marker, map[string]string{"collision": err.Error()})
	}
	return writeMarker(marker, map[string]string{"collision": "EADDRINUSE"})
}

func runDarwinPublisher(marker string, port int) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("publisher reserved-slot bind: %w", err)
	}
	defer listener.Close()
	if err := writeMarker(marker, map[string]string{"bind": "pass", "port": strconv.Itoa(port)}); err != nil {
		return err
	}
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	defer conn.Close()
	return serveEcho(conn)
}

func runDarwinConsumer(marker string, brokerPort, neighborPort int) error {
	if brokerPort == 0 || neighborPort == 0 {
		return fmt.Errorf("consumer broker and neighbor ports are required")
	}
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(brokerPort)), 5*time.Second)
	if err != nil {
		return fmt.Errorf("consumer broker endpoint: %w", err)
	}
	expected := responseFor(opaquePayload)
	if _, err := conn.Write(opaquePayload); err != nil {
		conn.Close()
		return err
	}
	got := make([]byte, len(expected))
	if _, err := io.ReadFull(conn, got); err != nil {
		conn.Close()
		return err
	}
	if string(got) != string(expected) {
		conn.Close()
		return fmt.Errorf("Darwin opaque response mismatch: got %x want %x", got, expected)
	}
	conn.Close()
	if err := requireEPERM(func() error {
		c, dialErr := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(neighborPort)), time.Second)
		if dialErr == nil {
			c.Close()
		}
		return dialErr
	}); err != nil {
		return fmt.Errorf("non-reserved neighbor: %w", err)
	}
	if err := requireEPERM(func() error {
		c, dialErr := net.DialTimeout("tcp4", "1.1.1.1:443", time.Second)
		if dialErr == nil {
			c.Close()
		}
		return dialErr
	}); err != nil {
		return fmt.Errorf("external TCP: %w", err)
	}
	if err := requireEPERM(func() error {
		l, bindErr := net.Listen("tcp4", "127.0.0.1:0")
		if bindErr == nil {
			l.Close()
		}
		return bindErr
	}); err != nil {
		return fmt.Errorf("consumer bind floor: %w", err)
	}
	return writeMarker(marker, map[string]string{"route": "pass", "neighbor": "EPERM", "external": "EPERM", "bind": "EPERM"})
}

func runDarwinLimitation(marker, endpoint string) error {
	if endpoint == "" {
		return fmt.Errorf("limitation endpoint is required")
	}
	conn, err := net.DialTimeout("tcp4", endpoint, 2*time.Second)
	if err != nil {
		if isErrno(err, syscall.EPERM) {
			return writeMarker(marker, map[string]string{"same-port": "refused"})
		}
		return err
	}
	defer conn.Close()
	if _, err := conn.Write([]byte("localhost-limitation")); err != nil {
		return err
	}
	return writeMarker(marker, map[string]string{"same-port": "reachable"})
}

func darwinProfile(pool []int, publisherPort int, allowPublisherBind bool) string {
	var b strings.Builder
	b.WriteString("(version 1)\n(allow default)\n")
	b.WriteString("(deny network-outbound (remote ip \"*:*\"))\n")
	for _, port := range pool {
		fmt.Fprintf(&b, "(allow network-outbound (remote tcp \"localhost:%d\"))\n", port)
	}
	if allowPublisherBind {
		fmt.Fprintf(&b, "(deny network-bind (require-not (local tcp \"localhost:%d\")))\n", publisherPort)
	} else {
		b.WriteString("(deny network-bind)\n")
	}
	return b.String()
}

func reservePortPool(size int) ([]int, error) {
	for attempt := 0; attempt < 80; attempt++ {
		seed, err := net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		base := seed.Addr().(*net.TCPAddr).Port
		seed.Close()
		listeners := make([]net.Listener, 0, size)
		ports := make([]int, 0, size)
		ok := true
		for offset := 0; offset < size; offset++ {
			listener, listenErr := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(base+offset)))
			if listenErr != nil {
				ok = false
				break
			}
			listeners = append(listeners, listener)
			ports = append(ports, base+offset)
		}
		for _, listener := range listeners {
			listener.Close()
		}
		if ok {
			return ports, nil
		}
	}
	return nil, fmt.Errorf("could not reserve a contiguous %d-port pool after bounded attempts", size)
}

func listenNonLoopback(port int) (net.Listener, string, error) {
	type candidate struct{ address string }
	var candidates []candidate
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, "", err
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		addresses, addrErr := iface.Addrs()
		if addrErr != nil {
			continue
		}
		for _, raw := range addresses {
			var ip net.IP
			switch value := raw.(type) {
			case *net.IPNet:
				ip = value.IP
			case *net.IPAddr:
				ip = value.IP
			}
			if ip == nil || ip.IsLoopback() || ip.IsLinkLocalUnicast() || !ip.IsGlobalUnicast() {
				continue
			}
			candidates = append(candidates, candidate{address: ip.String()})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].address < candidates[j].address })
	for _, item := range candidates {
		listener, listenErr := net.Listen("tcp4", net.JoinHostPort(item.address, strconv.Itoa(port)))
		if listenErr == nil {
			return listener, listener.Addr().String(), nil
		}
	}
	return nil, "", fmt.Errorf("no active non-loopback interface could bind limitation port %d", port)
}

func serveOneEcho(listener net.Listener) {
	conn, err := listener.Accept()
	if err == nil {
		_ = serveEcho(conn)
	}
}

func serveEcho(conn net.Conn) error {
	defer conn.Close()
	request := make([]byte, len(opaquePayload))
	if _, err := io.ReadFull(conn, request); err != nil {
		return err
	}
	_, err := conn.Write(responseFor(request))
	return err
}

func responseFor(payload []byte) []byte {
	digest := sha256.Sum256(payload)
	return []byte("route-sha256:" + fmt.Sprintf("%x", digest[:]))
}

func copyExecutable(root, source string) (string, error) {
	data, err := os.ReadFile(source)
	if err != nil {
		return "", fmt.Errorf("read probe executable: %w", err)
	}
	target := filepath.Join(root, "probe")
	if err := os.WriteFile(target, data, 0o755); err != nil {
		return "", fmt.Errorf("copy probe executable: %w", err)
	}
	return target, nil
}

func startSandbox(ctx context.Context, executable, profile, mode, marker string, port, brokerPort, neighborPort int, endpoint string) *exec.Cmd {
	args := []string{"-p", profile, "--", executable, "--mode", mode, "--marker", marker,
		"--port", strconv.Itoa(port), "--broker-port", strconv.Itoa(brokerPort),
		"--neighbor-port", strconv.Itoa(neighborPort), "--endpoint", endpoint}
	cmd := exec.CommandContext(ctx, "/usr/bin/sandbox-exec", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "start %s: %v\n", mode, err)
	}
	return cmd
}

func waitForMarker(ctx context.Context, path string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for marker %s: %w", path, ctx.Err())
		case <-ticker.C:
		}
	}
}

func waitChild(name string, cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("%s failed: %w", name, err)
	}
	return nil
}

func finishChild(name string, cmd *exec.Cmd, cause error) error {
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}
	return cause
}

func finishChildren(a, b, c *exec.Cmd, cause error) error {
	for _, child := range []*exec.Cmd{a, b, c} {
		if child != nil && child.Process != nil {
			_ = child.Process.Kill()
			_ = child.Wait()
		}
	}
	return cause
}

func writeMarker(path string, values map[string]string) error {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, key := range keys {
		fmt.Fprintf(&b, "%s=%s\n", key, values[key])
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, []byte(b.String()), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func markerValue(path, key string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	prefix := key + "="
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix), nil
		}
	}
	return "", fmt.Errorf("marker %s lacks %s", path, key)
}

func assertLinuxNamespace(marker, host, label string) error {
	ns, err := markerValue(marker, "netns")
	if err != nil {
		return err
	}
	if ns == host {
		return fmt.Errorf("%s remained in host network namespace %s", label, host)
	}
	return nil
}

func hostCannotReach(endpoint string) error {
	conn, err := net.DialTimeout("tcp4", endpoint, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("host reached namespace-local publisher endpoint %s", endpoint)
	}
	return nil
}

func requireUnavailable(endpoint, label string) error {
	conn, err := net.DialTimeout("tcp4", endpoint, 500*time.Millisecond)
	if err == nil {
		conn.Close()
		return fmt.Errorf("%s unexpectedly reachable at %s", label, endpoint)
	}
	return nil
}

func requireEPERM(fn func() error) error {
	err := fn()
	if err == nil {
		return fmt.Errorf("operation unexpectedly succeeded")
	}
	if !isErrno(err, syscall.EPERM) {
		return fmt.Errorf("expected EPERM, got %w", err)
	}
	return nil
}

func isErrno(err, want error) bool {
	return err != nil && errors.Is(err, want)
}

func finishChildOutput(_ *exec.Cmd) string { return "" }
