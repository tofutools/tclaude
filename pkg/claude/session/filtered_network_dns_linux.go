//go:build linux

package session

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/nftables"
	"github.com/mdlayher/netlink"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

const (
	filteredNetworkDNSPort             = 53
	filteredNetworkDNSMaxMessage       = 64 << 10
	filteredNetworkDNSMaxChain         = 8
	filteredNetworkDNSMaxAnswers       = 64
	filteredNetworkDNSMaxConcurrent    = 32
	filteredNetworkDNSExchangeTimeout  = 3 * time.Second
	filteredNetworkDNSConnectionWindow = 5 * time.Second
	filteredNetworkDNSMaxLease         = time.Hour
	filteredNetworkDNSHostMappingTTL   = 2 * time.Second
	filteredNetworkNFTReplyLimit       = 64 << 10
)

type filteredDNSExchange func(
	context.Context,
	dnsmessage.Message,
) (dnsmessage.Message, error)

type filteredNetworkDNSBroker struct {
	rules    sandboxpolicy.FilteredNetworkRuleSet
	udp      *net.UDPConn
	tcp      *net.TCPListener
	upstream filteredDNSExchange
	leases   filteredNetworkDNSLeaseStore

	ctx       context.Context
	cancel    context.CancelFunc
	done      chan error
	failOnce  sync.Once
	closeOnce sync.Once
	sem       chan struct{}
}

type filteredNetworkDNSLeaseStore interface {
	Ensure(
		[]sandboxpolicy.FilteredNetworkRule,
		netip.Addr,
		time.Duration,
	) (time.Duration, error)
	Close() error
}

type filteredDNSFatalError struct {
	err error
}

func (e *filteredDNSFatalError) Error() string {
	return e.err.Error()
}

func (e *filteredDNSFatalError) Unwrap() error {
	return e.err
}

func newFilteredNetworkDNSBroker(
	rules sandboxpolicy.FilteredNetworkRuleSet,
	udp *net.UDPConn,
	tcp *net.TCPListener,
	nftAuthority *os.File,
	upstream filteredDNSExchange,
) (*filteredNetworkDNSBroker, error) {
	if udp == nil || tcp == nil || nftAuthority == nil || upstream == nil {
		return nil, fmt.Errorf("filtered DNS broker descriptor contract is incomplete")
	}
	if _, err := sandboxpolicy.RenderFilteredNetworkNFT(rules); err != nil {
		return nil, fmt.Errorf("validate filtered DNS broker policy: %w", err)
	}
	leases, err := newFilteredNetworkDNSLeases(nftAuthority)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &filteredNetworkDNSBroker{
		rules: rules, udp: udp, tcp: tcp, upstream: upstream, leases: leases,
		ctx: ctx, cancel: cancel, done: make(chan error, 1),
		sem: make(chan struct{}, filteredNetworkDNSMaxConcurrent),
	}, nil
}

func (b *filteredNetworkDNSBroker) Start() <-chan error {
	go b.serveUDP()
	go b.serveTCP()
	return b.done
}

func (b *filteredNetworkDNSBroker) Close() {
	if b == nil {
		return
	}
	b.closeOnce.Do(func() {
		b.cancel()
		_ = b.udp.Close()
		_ = b.tcp.Close()
		_ = b.leases.Close()
	})
}

func (b *filteredNetworkDNSBroker) fail(err error) {
	if err == nil || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, context.Canceled) {
		return
	}
	b.failOnce.Do(func() {
		b.done <- err
		b.Close()
	})
}

func (b *filteredNetworkDNSBroker) serveUDP() {
	buffer := make([]byte, filteredNetworkDNSMaxMessage)
	for {
		n, peer, err := b.udp.ReadFromUDP(buffer)
		if err != nil {
			b.fail(fmt.Errorf("filtered DNS UDP listener: %w", err))
			return
		}
		packet := append([]byte(nil), buffer[:n]...)
		if !b.acquire() {
			continue
		}
		go func() {
			defer b.release()
			response, fatalErr := b.handlePacket(packet)
			if fatalErr != nil {
				b.fail(fatalErr)
				return
			}
			if len(response) == 0 {
				return
			}
			_, _ = b.udp.WriteToUDP(response, peer)
		}()
	}
}

func (b *filteredNetworkDNSBroker) serveTCP() {
	for {
		conn, err := b.tcp.AcceptTCP()
		if err != nil {
			b.fail(fmt.Errorf("filtered DNS TCP listener: %w", err))
			return
		}
		if !b.acquire() {
			_ = conn.Close()
			continue
		}
		go func() {
			defer b.release()
			defer func() { _ = conn.Close() }()
			if err := conn.SetDeadline(
				time.Now().Add(filteredNetworkDNSConnectionWindow),
			); err != nil {
				b.fail(fmt.Errorf("filtered DNS TCP deadline: %w", err))
				return
			}
			for {
				var length [2]byte
				if _, err := io.ReadFull(conn, length[:]); err != nil {
					return
				}
				size := int(binary.BigEndian.Uint16(length[:]))
				if size == 0 || size > filteredNetworkDNSMaxMessage {
					return
				}
				packet := make([]byte, size)
				if _, err := io.ReadFull(conn, packet); err != nil {
					return
				}
				response, fatalErr := b.handlePacket(packet)
				if fatalErr != nil {
					b.fail(fatalErr)
					return
				}
				if len(response) > math.MaxUint16 {
					return
				}
				binary.BigEndian.PutUint16(length[:], uint16(len(response)))
				if _, err := conn.Write(length[:]); err != nil {
					return
				}
				if _, err := conn.Write(response); err != nil {
					return
				}
			}
		}()
	}
}

func (b *filteredNetworkDNSBroker) acquire() bool {
	select {
	case b.sem <- struct{}{}:
		return true
	default:
		return false
	}
}

func (b *filteredNetworkDNSBroker) release() {
	<-b.sem
}

func (b *filteredNetworkDNSBroker) handlePacket(packet []byte) ([]byte, error) {
	var request dnsmessage.Message
	if err := request.Unpack(packet); err != nil {
		return nil, nil
	}
	if request.Response || request.OpCode != dnsmessage.OpCode(0) ||
		len(request.Questions) != 1 {
		return packFilteredDNSResponse(
			request, dnsmessage.RCodeFormatError, nil), nil
	}
	question := request.Questions[0]
	if question.Class != dnsmessage.ClassINET {
		return packFilteredDNSResponse(
			request, dnsmessage.RCodeRefused, nil), nil
	}
	matches, err := sandboxpolicy.MatchFilteredNetworkDNSName(
		b.rules, question.Name.String())
	if err != nil {
		return packFilteredDNSResponse(
			request, dnsmessage.RCodeRefused, nil), nil
	}
	if len(matches) == 0 && !filteredNetworkHasCIDRRule(b.rules) {
		return packFilteredDNSResponse(
			request, dnsmessage.RCodeRefused, nil), nil
	}
	switch question.Type {
	case dnsmessage.TypeA, dnsmessage.TypeAAAA:
		answers, rcode, resolveErr := b.resolveAddresses(question, matches)
		if resolveErr != nil {
			var fatal *filteredDNSFatalError
			if errors.As(resolveErr, &fatal) {
				return nil, fatal
			}
			return packFilteredDNSResponse(
				request, dnsmessage.RCodeServerFailure, nil), nil
		}
		return packFilteredDNSResponse(request, rcode, answers), nil
	default:
		if len(matches) == 0 {
			return packFilteredDNSResponse(
				request, dnsmessage.RCodeRefused, nil), nil
		}
		response, upstreamErr := b.exchange(request)
		if upstreamErr != nil {
			return packFilteredDNSResponse(
				request, dnsmessage.RCodeServerFailure, nil), nil
		}
		sanitizeFilteredDNSResponse(&response)
		packed, packErr := response.Pack()
		if packErr != nil {
			return packFilteredDNSResponse(
				request, dnsmessage.RCodeServerFailure, nil), nil
		}
		return packed, nil
	}
}

func filteredNetworkHasCIDRRule(
	rules sandboxpolicy.FilteredNetworkRuleSet,
) bool {
	for _, rule := range rules.Rules {
		if rule.Selector == sandboxpolicy.NetworkSelectorCIDR {
			return true
		}
	}
	return false
}

func (b *filteredNetworkDNSBroker) resolveAddresses(
	question dnsmessage.Question,
	matches []sandboxpolicy.FilteredNetworkRule,
) ([]dnsmessage.Resource, dnsmessage.RCode, error) {
	current := question.Name
	seen := make(map[string]struct{}, filteredNetworkDNSMaxChain+1)
	answers := make([]dnsmessage.Resource, 0, 4)
	chainTTL := uint32(filteredNetworkDNSMaxLease / time.Second)
	for depth := 0; depth <= filteredNetworkDNSMaxChain; depth++ {
		currentKey := strings.ToLower(current.String())
		if _, exists := seen[currentKey]; exists {
			return nil, dnsmessage.RCodeServerFailure,
				fmt.Errorf("filtered DNS CNAME loop")
		}
		seen[currentKey] = struct{}{}

		upstreamRequest := dnsmessage.Message{
			Header: dnsmessage.Header{
				ID: questionID(question, depth), RecursionDesired: true,
			},
			Questions: []dnsmessage.Question{{
				Name: current, Type: question.Type, Class: dnsmessage.ClassINET,
			}},
		}
		response, err := b.exchange(upstreamRequest)
		if err != nil {
			return nil, dnsmessage.RCodeServerFailure, err
		}
		if response.RCode != dnsmessage.RCodeSuccess {
			return nil, response.RCode, nil
		}
		next, foundCNAME, records, collectErr := collectFilteredDNSAnswers(
			response.Answers, current, question.Type)
		if collectErr != nil {
			return nil, dnsmessage.RCodeServerFailure, collectErr
		}
		if len(records) > 0 {
			for _, record := range records {
				record.Header.TTL = minUint32(record.Header.TTL, chainTTL)
				leased, allowed, leaseErr := b.leaseAddressRecord(record, matches)
				if leaseErr != nil {
					return nil, dnsmessage.RCodeServerFailure, leaseErr
				}
				if allowed {
					answers = append(answers, leased)
				}
			}
			if len(answers) == 0 {
				return nil, dnsmessage.RCodeRefused, nil
			}
			if len(answers) > filteredNetworkDNSMaxAnswers {
				return nil, dnsmessage.RCodeServerFailure,
					fmt.Errorf("filtered DNS answer exceeds record limit")
			}
			return answers, dnsmessage.RCodeSuccess, nil
		}
		if !foundCNAME {
			return answers, dnsmessage.RCodeSuccess, nil
		}
		cname := dnsmessage.Resource{
			Header: dnsmessage.ResourceHeader{
				Name: current, Type: dnsmessage.TypeCNAME,
				Class: dnsmessage.ClassINET, TTL: next.ttl,
			},
			Body: &dnsmessage.CNAMEResource{CNAME: next.name},
		}
		answers = append(answers, cname)
		chainTTL = minUint32(chainTTL, next.ttl)
		current = next.name
	}
	return nil, dnsmessage.RCodeServerFailure,
		fmt.Errorf("filtered DNS CNAME chain exceeds %d", filteredNetworkDNSMaxChain)
}

type filteredDNSCNAME struct {
	name dnsmessage.Name
	ttl  uint32
}

func collectFilteredDNSAnswers(
	resources []dnsmessage.Resource,
	owner dnsmessage.Name,
	qtype dnsmessage.Type,
) (filteredDNSCNAME, bool, []dnsmessage.Resource, error) {
	ownerKey := strings.ToLower(owner.String())
	var cname filteredDNSCNAME
	var foundCNAME bool
	records := make([]dnsmessage.Resource, 0, 2)
	for _, resource := range resources {
		if strings.ToLower(resource.Header.Name.String()) != ownerKey {
			continue
		}
		switch body := resource.Body.(type) {
		case *dnsmessage.CNAMEResource:
			if foundCNAME && cname.name.String() != body.CNAME.String() {
				return filteredDNSCNAME{}, false, nil,
					fmt.Errorf("filtered DNS answer has conflicting CNAMEs")
			}
			cname = filteredDNSCNAME{name: body.CNAME, ttl: resource.Header.TTL}
			foundCNAME = true
		case *dnsmessage.AResource:
			if qtype == dnsmessage.TypeA {
				records = append(records, resource)
			}
		case *dnsmessage.AAAAResource:
			if qtype == dnsmessage.TypeAAAA {
				records = append(records, resource)
			}
		}
	}
	return cname, foundCNAME, records, nil
}

func (b *filteredNetworkDNSBroker) leaseAddressRecord(
	record dnsmessage.Resource,
	matches []sandboxpolicy.FilteredNetworkRule,
) (dnsmessage.Resource, bool, error) {
	var address netip.Addr
	switch body := record.Body.(type) {
	case *dnsmessage.AResource:
		address = netip.AddrFrom4(body.A)
	case *dnsmessage.AAAAResource:
		address = netip.AddrFrom16(body.AAAA)
	default:
		return dnsmessage.Resource{}, false, fmt.Errorf(
			"filtered DNS lease received non-address record")
	}
	ttl := time.Duration(record.Header.TTL) * time.Second
	if ttl <= 0 {
		return dnsmessage.Resource{}, false, fmt.Errorf(
			"filtered DNS refuses zero-TTL address answers")
	}
	if ttl > filteredNetworkDNSMaxLease {
		ttl = filteredNetworkDNSMaxLease
	}
	if address.IsLoopback() {
		if !sandboxpolicy.FilteredNetworkAllowsDNSLoopbackAnswer(b.rules) {
			return dnsmessage.Resource{}, false, nil
		}
		if len(matches) == 0 {
			return dnsmessage.Resource{}, false, nil
		}
		record.Header.TTL = uint32(ttl / time.Second)
		if address.Is4() {
			synthetic := netip.MustParseAddr(
				sandboxpolicy.FilteredNetworkLoopbackIPv4).As4()
			record.Body = &dnsmessage.AResource{A: synthetic}
		} else {
			synthetic := netip.MustParseAddr(
				sandboxpolicy.FilteredNetworkLoopbackIPv6).As16()
			record.Body = &dnsmessage.AAAAResource{AAAA: synthetic}
		}
		return record, true, nil
	}
	if len(matches) == 0 {
		if !sandboxpolicy.FilteredNetworkCIDRCoversAddress(b.rules, address) {
			return dnsmessage.Resource{}, false, nil
		}
		record.Header.TTL = uint32(ttl / time.Second)
		return record, true, nil
	}
	effectiveTTL, err := b.leases.Ensure(matches, address, ttl)
	if err != nil {
		return dnsmessage.Resource{}, false, &filteredDNSFatalError{err: fmt.Errorf(
			"install filtered DNS nft lease: %w", err)}
	}
	record.Header.TTL = uint32(effectiveTTL / time.Second)
	return record, true, nil
}

func (b *filteredNetworkDNSBroker) exchange(
	request dnsmessage.Message,
) (dnsmessage.Message, error) {
	ctx, cancel := context.WithTimeout(
		b.ctx, filteredNetworkDNSExchangeTimeout)
	defer cancel()
	return b.upstream(ctx, request)
}

func packFilteredDNSResponse(
	request dnsmessage.Message,
	rcode dnsmessage.RCode,
	answers []dnsmessage.Resource,
) []byte {
	response := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: request.ID, Response: true, RecursionDesired: request.RecursionDesired,
			RecursionAvailable: true, RCode: rcode,
		},
		Questions: append([]dnsmessage.Question(nil), request.Questions...),
		Answers:   answers,
	}
	packed, err := response.Pack()
	if err != nil {
		return nil
	}
	return packed
}

func sanitizeFilteredDNSResponse(response *dnsmessage.Message) {
	if response == nil {
		return
	}
	response.Answers = filterAddressBearingDNSResources(response.Answers)
	response.Authorities = filterAddressBearingDNSResources(response.Authorities)
	response.Additionals = filterAddressBearingDNSResources(response.Additionals)
}

func filterAddressBearingDNSResources(
	resources []dnsmessage.Resource,
) []dnsmessage.Resource {
	filtered := resources[:0]
	for _, resource := range resources {
		switch resource.Body.(type) {
		case *dnsmessage.AResource, *dnsmessage.AAAAResource,
			*dnsmessage.SVCBResource, *dnsmessage.HTTPSResource:
			continue
		default:
			filtered = append(filtered, resource)
		}
	}
	return filtered
}

func questionID(question dnsmessage.Question, depth int) uint16 {
	var value uint32 = uint32(depth + 1)
	for _, char := range question.Name.String() {
		value = value*33 + uint32(char)
	}
	value = value*33 + uint32(question.Type)
	return uint16(value)
}

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

type filteredNetworkDNSLeases struct {
	authority filteredNetworkNFTLeaseAdder
	now       func() time.Time
	mu        sync.Mutex
	expires   map[filteredNetworkLeaseKey]time.Time
}

type filteredNetworkNFTLeaseAdder interface {
	Add(string, netip.Addr, time.Duration) error
	Close() error
}

type filteredNetworkLeaseKey struct {
	set     string
	address netip.Addr
}

func newFilteredNetworkDNSLeases(
	authority *os.File,
) (*filteredNetworkDNSLeases, error) {
	nftAuthority, err := newFilteredNetworkNFTAuthority(authority)
	if err != nil {
		return nil, err
	}
	return &filteredNetworkDNSLeases{
		authority: nftAuthority,
		now:       time.Now,
		expires:   make(map[filteredNetworkLeaseKey]time.Time),
	}, nil
}

func (l *filteredNetworkDNSLeases) Close() error {
	if l == nil || l.authority == nil {
		return nil
	}
	return l.authority.Close()
}

func (l *filteredNetworkDNSLeases) Ensure(
	rules []sandboxpolicy.FilteredNetworkRule,
	address netip.Addr,
	ttl time.Duration,
) (time.Duration, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !address.IsValid() || address.IsUnspecified() ||
		address.IsMulticast() || ttl < time.Second {
		return 0, fmt.Errorf("filtered DNS lease is invalid")
	}
	now := l.now()
	effective := ttl.Truncate(time.Second)
	keys := make([]filteredNetworkLeaseKey, 0, len(rules))
	for _, rule := range rules {
		ipv4Set, ipv6Set := sandboxpolicy.FilteredNetworkDNSSetNames(rule.EntryIndex)
		setName := ipv6Set
		if address.Is4() {
			setName = ipv4Set
		}
		key := filteredNetworkLeaseKey{set: setName, address: address}
		keys = append(keys, key)
		if expiry, ok := l.expires[key]; ok {
			remaining := expiry.Sub(now).Truncate(time.Second)
			if remaining >= time.Second {
				if remaining < effective {
					effective = remaining
				}
			} else {
				delete(l.expires, key)
			}
		}
	}
	for _, key := range keys {
		if _, exists := l.expires[key]; exists {
			continue
		}
		if err := l.authority.Add(key.set, key.address, effective); err != nil {
			return 0, err
		}
		l.expires[key] = now.Add(effective)
	}
	if effective < time.Second {
		return 0, fmt.Errorf("filtered DNS lease expired before reply")
	}
	return effective, nil
}

type filteredNetworkNFTAuthority struct {
	file *os.File
	fd   int
	pid  uint32
	mu   sync.Mutex
}

func newFilteredNetworkNFTAuthority(
	file *os.File,
) (*filteredNetworkNFTAuthority, error) {
	if file == nil {
		return nil, fmt.Errorf("filtered DNS nft authority is missing")
	}
	fd := int(file.Fd())
	address, err := unix.Getsockname(fd)
	if err != nil {
		return nil, fmt.Errorf("inspect filtered DNS nft authority: %w", err)
	}
	netlinkAddress, ok := address.(*unix.SockaddrNetlink)
	if !ok || netlinkAddress.Pid == 0 {
		return nil, fmt.Errorf("filtered DNS nft authority has invalid netlink identity")
	}
	if err := unix.SetsockoptTimeval(
		fd, unix.SOL_SOCKET, unix.SO_RCVTIMEO,
		&unix.Timeval{Sec: int64(filteredNetworkDNSExchangeTimeout / time.Second)},
	); err != nil {
		return nil, fmt.Errorf("bound filtered DNS nft authority timeout: %w", err)
	}
	return &filteredNetworkNFTAuthority{
		file: file, fd: fd, pid: netlinkAddress.Pid,
	}, nil
}

func (a *filteredNetworkNFTAuthority) Close() error {
	if a == nil || a.file == nil {
		return nil
	}
	return a.file.Close()
}

func (a *filteredNetworkNFTAuthority) Add(
	setName string,
	address netip.Addr,
	ttl time.Duration,
) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	conn, err := nftables.New(nftables.WithTestDial(a.exchange))
	if err != nil {
		return err
	}
	table := &nftables.Table{
		Family: nftables.TableFamilyINet,
		Name:   sandboxpolicy.FilteredNetworkNFTTable,
	}
	set := &nftables.Set{
		Table: table, Name: setName, HasTimeout: true,
	}
	key := address.AsSlice()
	if err := conn.SetAddElements(set, []nftables.SetElement{{
		Key: key, Timeout: ttl,
	}}); err != nil {
		return err
	}
	return conn.Flush()
}

func (a *filteredNetworkNFTAuthority) exchange(
	requests []netlink.Message,
) ([]netlink.Message, error) {
	if len(requests) == 0 {
		return nil, nil
	}
	var payload []byte
	ackCount := 0
	for i := range requests {
		requests[i].Header.PID = a.pid
		if requests[i].Header.Flags&netlink.Acknowledge != 0 {
			ackCount++
		}
		data, err := requests[i].MarshalBinary()
		if err != nil {
			return nil, err
		}
		payload = append(payload, data...)
	}
	if _, err := unix.SendmsgN(
		a.fd, payload, nil,
		&unix.SockaddrNetlink{Family: unix.AF_NETLINK}, 0,
	); err != nil {
		return nil, err
	}
	replies := make([]netlink.Message, 0, ackCount)
	for len(replies) < ackCount {
		buffer := make([]byte, filteredNetworkNFTReplyLimit)
		n, _, err := unix.Recvfrom(a.fd, buffer, 0)
		if err != nil {
			return nil, err
		}
		messages, err := unmarshalFilteredNetworkNetlinkMessages(buffer[:n])
		if err != nil {
			return nil, err
		}
		replies = append(replies, messages...)
	}
	return replies, nil
}

func unmarshalFilteredNetworkNetlinkMessages(
	buffer []byte,
) ([]netlink.Message, error) {
	var messages []netlink.Message
	for len(buffer) > 0 {
		if len(buffer) < 16 {
			return nil, fmt.Errorf("filtered DNS nft reply has a short header")
		}
		length := int(binary.NativeEndian.Uint32(buffer[:4]))
		if length < 16 || length > len(buffer) {
			return nil, fmt.Errorf("filtered DNS nft reply has an invalid length")
		}
		aligned := (length + 3) &^ 3
		if aligned > len(buffer) {
			return nil, fmt.Errorf("filtered DNS nft reply is not aligned")
		}
		var message netlink.Message
		if err := message.UnmarshalBinary(buffer[:aligned]); err != nil {
			return nil, err
		}
		messages = append(messages, message)
		buffer = buffer[aligned:]
	}
	return messages, nil
}

func parseFilteredNetworkDNSUpstreams(resolvConf []byte) ([]string, error) {
	const maxUpstreams = 3
	upstreams := make([]string, 0, maxUpstreams)
	for _, line := range strings.Split(string(resolvConf), "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) < 2 || fields[0] != "nameserver" {
			continue
		}
		address, err := netip.ParseAddr(fields[1])
		if err != nil {
			return nil, fmt.Errorf(
				"host resolv.conf nameserver %q is invalid", fields[1])
		}
		upstreams = append(upstreams, net.JoinHostPort(
			address.String(), strconv.Itoa(filteredNetworkDNSPort)))
		if len(upstreams) == maxUpstreams {
			break
		}
	}
	if len(upstreams) == 0 {
		return nil, fmt.Errorf(
			"host resolv.conf has no usable nameserver for filtered DNS")
	}
	return upstreams, nil
}

func parseFilteredNetworkHostMappings(
	hosts []byte,
) (map[string][]netip.Addr, error) {
	if len(hosts) > 1<<20 {
		return nil, fmt.Errorf("host /etc/hosts exceeds the filtered-network limit")
	}
	mappings := make(map[string][]netip.Addr)
	for _, line := range strings.Split(string(hosts), "\n") {
		fields := strings.Fields(strings.SplitN(line, "#", 2)[0])
		if len(fields) < 2 {
			continue
		}
		address, err := netip.ParseAddr(fields[0])
		if err != nil {
			continue
		}
		for _, alias := range fields[1:] {
			name := strings.ToLower(strings.TrimSuffix(alias, "."))
			if name == "" {
				continue
			}
			seen := false
			for _, existing := range mappings[name] {
				if existing == address {
					seen = true
					break
				}
			}
			if !seen {
				mappings[name] = append(mappings[name], address)
			}
		}
	}
	return mappings, nil
}

func hostFilteredDNSExchange(
	upstreams []string,
	hostMappings map[string][]netip.Addr,
) filteredDNSExchange {
	return func(
		ctx context.Context,
		request dnsmessage.Message,
	) (dnsmessage.Message, error) {
		if response, ok := filteredDNSHostsResponse(request, hostMappings); ok {
			return response, nil
		}
		packet, err := request.Pack()
		if err != nil {
			return dnsmessage.Message{}, err
		}
		var failures []error
		for _, upstream := range upstreams {
			response, exchangeErr := exchangeFilteredDNSPacket(
				ctx, "udp", upstream, packet)
			if exchangeErr == nil && response.Truncated {
				response, exchangeErr = exchangeFilteredDNSPacket(
					ctx, "tcp", upstream, packet)
			}
			if exchangeErr != nil {
				failures = append(failures, exchangeErr)
				continue
			}
			if response.ID != request.ID {
				failures = append(failures, fmt.Errorf(
					"filtered DNS upstream returned a mismatched query ID"))
				continue
			}
			return response, nil
		}
		return dnsmessage.Message{}, errors.Join(failures...)
	}
}

func filteredDNSHostsResponse(
	request dnsmessage.Message,
	hostMappings map[string][]netip.Addr,
) (dnsmessage.Message, bool) {
	if len(request.Questions) != 1 {
		return dnsmessage.Message{}, false
	}
	question := request.Questions[0]
	name := strings.ToLower(strings.TrimSuffix(question.Name.String(), "."))
	addresses, ok := hostMappings[name]
	if !ok {
		return dnsmessage.Message{}, false
	}
	response := dnsmessage.Message{
		Header: dnsmessage.Header{
			ID: request.ID, Response: true,
			RecursionDesired:   request.RecursionDesired,
			RecursionAvailable: true,
		},
		Questions: request.Questions,
	}
	for _, address := range addresses {
		header := dnsmessage.ResourceHeader{
			Name: question.Name, Type: question.Type,
			Class: dnsmessage.ClassINET,
			TTL:   uint32(filteredNetworkDNSHostMappingTTL / time.Second),
		}
		switch {
		case question.Type == dnsmessage.TypeA && address.Is4():
			response.Answers = append(response.Answers, dnsmessage.Resource{
				Header: header,
				Body:   &dnsmessage.AResource{A: address.As4()},
			})
		case question.Type == dnsmessage.TypeAAAA && address.Is6():
			response.Answers = append(response.Answers, dnsmessage.Resource{
				Header: header,
				Body:   &dnsmessage.AAAAResource{AAAA: address.As16()},
			})
		}
	}
	return response, true
}

func exchangeFilteredDNSPacket(
	ctx context.Context,
	network, upstream string,
	packet []byte,
) (dnsmessage.Message, error) {
	conn, err := (&net.Dialer{}).DialContext(ctx, network, upstream)
	if err != nil {
		return dnsmessage.Message{}, err
	}
	defer func() { _ = conn.Close() }()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if network == "tcp" {
		if len(packet) > math.MaxUint16 {
			return dnsmessage.Message{}, fmt.Errorf("filtered DNS query is too large")
		}
		var length [2]byte
		binary.BigEndian.PutUint16(length[:], uint16(len(packet)))
		if _, err := conn.Write(length[:]); err != nil {
			return dnsmessage.Message{}, err
		}
	}
	if _, err := conn.Write(packet); err != nil {
		return dnsmessage.Message{}, err
	}
	var response []byte
	if network == "tcp" {
		var length [2]byte
		if _, err := io.ReadFull(conn, length[:]); err != nil {
			return dnsmessage.Message{}, err
		}
		size := int(binary.BigEndian.Uint16(length[:]))
		if size == 0 || size > filteredNetworkDNSMaxMessage {
			return dnsmessage.Message{}, fmt.Errorf(
				"filtered DNS upstream response length is invalid")
		}
		response = make([]byte, size)
		if _, err := io.ReadFull(conn, response); err != nil {
			return dnsmessage.Message{}, err
		}
	} else {
		response = make([]byte, filteredNetworkDNSMaxMessage)
		n, err := conn.Read(response)
		if err != nil {
			return dnsmessage.Message{}, err
		}
		response = response[:n]
	}
	var message dnsmessage.Message
	if err := message.Unpack(response); err != nil {
		return dnsmessage.Message{}, err
	}
	return message, nil
}
