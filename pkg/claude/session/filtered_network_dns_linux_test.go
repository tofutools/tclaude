//go:build linux

package session

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/net/dns/dnsmessage"
)

type fakeFilteredDNSLeaseStore struct {
	calls []fakeFilteredDNSLeaseCall
	ttl   time.Duration
	err   error
}

type fakeFilteredDNSLeaseCall struct {
	rules   []sandboxpolicy.FilteredNetworkRule
	address netip.Addr
	ttl     time.Duration
}

func (f *fakeFilteredDNSLeaseStore) Ensure(
	rules []sandboxpolicy.FilteredNetworkRule,
	address netip.Addr,
	ttl time.Duration,
) (time.Duration, error) {
	f.calls = append(f.calls, fakeFilteredDNSLeaseCall{
		rules:   append([]sandboxpolicy.FilteredNetworkRule(nil), rules...),
		address: address,
		ttl:     ttl,
	})
	if f.ttl != 0 {
		ttl = f.ttl
	}
	return ttl, f.err
}

func (*fakeFilteredDNSLeaseStore) Close() error { return nil }

func TestFilteredDNSBrokerExactNameCNAMEAndTTLLease(t *testing.T) {
	rules := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		Host: "api.example.test", Ports: []int{443},
	}})
	leases := &fakeFilteredDNSLeaseStore{ttl: 17 * time.Second}
	broker := testFilteredDNSBroker(rules, leases, func(
		_ context.Context,
		request dnsmessage.Message,
	) (dnsmessage.Message, error) {
		q := request.Questions[0]
		switch q.Name.String() {
		case "api.example.test.":
			return dnsmessage.Message{
				Header:    dnsmessage.Header{ID: request.ID, Response: true},
				Questions: request.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: q.Name, Type: dnsmessage.TypeCNAME,
						Class: dnsmessage.ClassINET, TTL: 30,
					},
					Body: &dnsmessage.CNAMEResource{
						CNAME: dnsmessage.MustNewName("edge.example-cdn.test."),
					},
				}},
			}, nil
		case "edge.example-cdn.test.":
			return dnsmessage.Message{
				Header:    dnsmessage.Header{ID: request.ID, Response: true},
				Questions: request.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: q.Name, Type: dnsmessage.TypeA,
						Class: dnsmessage.ClassINET, TTL: 60,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 44}},
				}},
			}, nil
		default:
			t.Fatalf("unexpected upstream query %s", q.Name.String())
			return dnsmessage.Message{}, nil
		}
	})

	response := queryFilteredDNSBroker(
		t, broker, "api.example.test.", dnsmessage.TypeA)
	assert.Equal(t, dnsmessage.RCodeSuccess, response.RCode)
	require.Len(t, response.Answers, 2)
	assert.Equal(t, uint32(30), response.Answers[0].Header.TTL)
	assert.Equal(t, uint32(17), response.Answers[1].Header.TTL)
	require.Len(t, leases.calls, 1)
	assert.Equal(t, netip.MustParseAddr("192.0.2.44"), leases.calls[0].address)
	assert.Equal(t, 30*time.Second, leases.calls[0].ttl,
		"the address lease may not outlive the CNAME TTL")
	require.Len(t, leases.calls[0].rules, 1)
	assert.Equal(t, 0, leases.calls[0].rules[0].EntryIndex)
}

func TestFilteredDNSBrokerRefusesSiblingWithoutUpstreamLookup(t *testing.T) {
	rules := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		Host: "api.example.test",
	}})
	upstreamCalled := false
	broker := testFilteredDNSBroker(
		rules, &fakeFilteredDNSLeaseStore{},
		func(context.Context, dnsmessage.Message) (dnsmessage.Message, error) {
			upstreamCalled = true
			return dnsmessage.Message{}, nil
		},
	)
	response := queryFilteredDNSBroker(
		t, broker, "sibling.example.test.", dnsmessage.TypeA)
	assert.Equal(t, dnsmessage.RCodeRefused, response.RCode)
	assert.False(t, upstreamCalled)
}

func TestFilteredDNSBrokerLoopbackRebindingRequiresSelectorAndRewrites(t *testing.T) {
	answer := func(
		_ context.Context,
		request dnsmessage.Message,
	) (dnsmessage.Message, error) {
		q := request.Questions[0]
		return dnsmessage.Message{
			Header:    dnsmessage.Header{ID: request.ID, Response: true},
			Questions: request.Questions,
			Answers: []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{
					Name: q.Name, Type: dnsmessage.TypeA,
					Class: dnsmessage.ClassINET, TTL: 30,
				},
				Body: &dnsmessage.AResource{A: [4]byte{127, 0, 0, 1}},
			}},
		}, nil
	}
	without := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		Host: "rebinding.example.test",
	}})
	response := queryFilteredDNSBroker(
		t, testFilteredDNSBroker(
			without, &fakeFilteredDNSLeaseStore{}, answer),
		"rebinding.example.test.", dnsmessage.TypeA,
	)
	assert.Equal(t, dnsmessage.RCodeRefused, response.RCode)

	with := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{
		{Host: "rebinding.example.test"},
		{Loopback: true, Ports: []int{443}},
	})
	leases := &fakeFilteredDNSLeaseStore{}
	response = queryFilteredDNSBroker(
		t, testFilteredDNSBroker(with, leases, answer),
		"rebinding.example.test.", dnsmessage.TypeA,
	)
	assert.Equal(t, dnsmessage.RCodeSuccess, response.RCode)
	require.Len(t, response.Answers, 1)
	resource, ok := response.Answers[0].Body.(*dnsmessage.AResource)
	require.True(t, ok)
	assert.Equal(t,
		netip.MustParseAddr(sandboxpolicy.FilteredNetworkLoopbackIPv4).As4(),
		resource.A,
	)
	assert.Empty(t, leases.calls,
		"the static loopback rule, not a domain lease, remains port authority")
}

func TestFilteredDNSBrokerCIDROnlyFiltersAnswersWithoutLeases(t *testing.T) {
	rules := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		CIDR: "192.0.2.0/24", Ports: []int{443},
	}})
	answer := func(address [4]byte) filteredDNSExchange {
		return func(
			_ context.Context,
			request dnsmessage.Message,
		) (dnsmessage.Message, error) {
			q := request.Questions[0]
			return dnsmessage.Message{
				Header:    dnsmessage.Header{ID: request.ID, Response: true},
				Questions: request.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: q.Name, Type: dnsmessage.TypeA,
						Class: dnsmessage.ClassINET, TTL: 30,
					},
					Body: &dnsmessage.AResource{A: address},
				}},
			}, nil
		}
	}
	leases := &fakeFilteredDNSLeaseStore{}
	allowed := queryFilteredDNSBroker(
		t, testFilteredDNSBroker(
			rules, leases, answer([4]byte{192, 0, 2, 55})),
		"arbitrary.example.", dnsmessage.TypeA,
	)
	assert.Equal(t, dnsmessage.RCodeSuccess, allowed.RCode)
	require.Len(t, allowed.Answers, 1)
	assert.Empty(t, leases.calls)

	denied := queryFilteredDNSBroker(
		t, testFilteredDNSBroker(
			rules, leases, answer([4]byte{192, 0, 3, 55})),
		"arbitrary.example.", dnsmessage.TypeA,
	)
	assert.Equal(t, dnsmessage.RCodeRefused, denied.RCode)
	assert.Empty(t, denied.Answers)
}

func TestFilteredDNSBrokerLeaseAuthorityFailureIsFatal(t *testing.T) {
	rules := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		Host: "api.example.test",
	}})
	leases := &fakeFilteredDNSLeaseStore{
		err: errors.New("netfilter authority closed"),
	}
	broker := testFilteredDNSBroker(
		rules, leases,
		func(
			_ context.Context,
			request dnsmessage.Message,
		) (dnsmessage.Message, error) {
			q := request.Questions[0]
			return dnsmessage.Message{
				Header:    dnsmessage.Header{ID: request.ID, Response: true},
				Questions: request.Questions,
				Answers: []dnsmessage.Resource{{
					Header: dnsmessage.ResourceHeader{
						Name: q.Name, Type: dnsmessage.TypeA,
						Class: dnsmessage.ClassINET, TTL: 30,
					},
					Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 44}},
				}},
			}, nil
		},
	)
	request := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 42, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName("api.example.test."),
			Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
		}},
	}
	packet, err := request.Pack()
	require.NoError(t, err)
	response, err := broker.handlePacket(packet)
	assert.Nil(t, response)
	var fatal *filteredDNSFatalError
	require.ErrorAs(t, err, &fatal)
	assert.ErrorContains(t, err, "netfilter authority closed")
}

type fakeFilteredNFTLeaseAdder struct {
	adds []fakeFilteredNFTAdd
}

type fakeFilteredNFTAdd struct {
	set     string
	address netip.Addr
	ttl     time.Duration
}

func (f *fakeFilteredNFTLeaseAdder) Add(
	set string,
	address netip.Addr,
	ttl time.Duration,
) error {
	f.adds = append(f.adds, fakeFilteredNFTAdd{
		set: set, address: address, ttl: ttl,
	})
	return nil
}

func (*fakeFilteredNFTLeaseAdder) Close() error { return nil }

func TestFilteredDNSLeaseNeverExtendsPastOriginalExpiry(t *testing.T) {
	authority := &fakeFilteredNFTLeaseAdder{}
	now := time.Unix(1_000, 0)
	leases := &filteredNetworkDNSLeases{
		authority: authority,
		now:       func() time.Time { return now },
		expires:   make(map[filteredNetworkLeaseKey]time.Time),
	}
	rules := []sandboxpolicy.FilteredNetworkRule{{EntryIndex: 4}}
	address := netip.MustParseAddr("192.0.2.88")

	effective, err := leases.Ensure(rules, address, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, effective)
	require.Len(t, authority.adds, 1)
	assert.Equal(t, "dns4_4", authority.adds[0].set)

	now = now.Add(12 * time.Second)
	effective, err = leases.Ensure(
		append(rules, sandboxpolicy.FilteredNetworkRule{EntryIndex: 5}),
		address,
		60*time.Second,
	)
	require.NoError(t, err)
	assert.Equal(t, 18*time.Second, effective)
	require.Len(t, authority.adds, 2,
		"a repeated answer must not refresh the original kernel lease")
	assert.Equal(t, "dns4_5", authority.adds[1].set)
	assert.Equal(t, 18*time.Second, authority.adds[1].ttl,
		"a new rule's lease may not outlive an existing rule lease returned in the same answer")
}

func TestParseFilteredNetworkDNSUpstreams(t *testing.T) {
	got, err := parseFilteredNetworkDNSUpstreams([]byte(
		"# generated\nnameserver 127.0.0.53\nnameserver 2001:db8::53\n",
	))
	require.NoError(t, err)
	assert.Equal(t, []string{"127.0.0.53:53", "[2001:db8::53]:53"}, got)
}

func compileFilteredDNSRules(
	t *testing.T,
	allow []sandboxpolicy.NetworkAllowEntry,
) sandboxpolicy.FilteredNetworkRuleSet {
	t.Helper()
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(
		sandboxpolicy.NetworkRules{
			Mode:  sandboxpolicy.AccessModeList,
			Allow: allow,
		})
	require.NoError(t, err)
	return rules
}

func testFilteredDNSBroker(
	rules sandboxpolicy.FilteredNetworkRuleSet,
	leases filteredNetworkDNSLeaseStore,
	upstream filteredDNSExchange,
) *filteredNetworkDNSBroker {
	ctx, cancel := context.WithCancel(context.Background())
	return &filteredNetworkDNSBroker{
		rules: rules, leases: leases, upstream: upstream,
		ctx: ctx, cancel: cancel,
	}
}

func queryFilteredDNSBroker(
	t *testing.T,
	broker *filteredNetworkDNSBroker,
	name string,
	qtype dnsmessage.Type,
) dnsmessage.Message {
	t.Helper()
	request := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 42, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName(name),
			Type: qtype, Class: dnsmessage.ClassINET,
		}},
	}
	packet, err := request.Pack()
	require.NoError(t, err)
	responsePacket, err := broker.handlePacket(packet)
	require.NoError(t, err)
	var response dnsmessage.Message
	require.NoError(t, response.Unpack(responsePacket))
	return response
}
