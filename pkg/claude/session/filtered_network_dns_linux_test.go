//go:build linux

package session

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"

	"github.com/mdlayher/netlink"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
)

type fakeFilteredDNSLeaseStore struct {
	calls []fakeFilteredDNSLeaseCall
	ttl   time.Duration
	err   error
}

type fakeFilteredDNSLeaseCall struct {
	matches sandboxpolicy.FilteredNetworkDNSMatches
	address netip.Addr
	ttl     time.Duration
}

func (f *fakeFilteredDNSLeaseStore) Ensure(
	matches sandboxpolicy.FilteredNetworkDNSMatches,
	address netip.Addr,
	ttl time.Duration,
) (time.Duration, error) {
	f.calls = append(f.calls, fakeFilteredDNSLeaseCall{
		matches: sandboxpolicy.FilteredNetworkDNSMatches{
			Allow: append([]sandboxpolicy.FilteredNetworkRule(nil), matches.Allow...),
			Deny:  append([]sandboxpolicy.FilteredNetworkRule(nil), matches.Deny...),
		},
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
	require.Len(t, leases.calls[0].matches.Allow, 1)
	assert.Equal(t, 0, leases.calls[0].matches.Allow[0].EntryIndex)
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

func TestFilteredDNSBrokerCNAMEIntoDeniedNameSeedsNegativeLease(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(
		sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeList,
			Allow: []sandboxpolicy.NetworkAllowEntry{{
				Host: "alias.example.test",
			}},
			Deny: []sandboxpolicy.NetworkAllowEntry{{
				Host: "blocked.example.test",
			}},
		})
	require.NoError(t, err)
	leases := &fakeFilteredDNSLeaseStore{}
	broker := testFilteredDNSBroker(rules, leases, func(
		_ context.Context,
		request dnsmessage.Message,
	) (dnsmessage.Message, error) {
		q := request.Questions[0]
		response := dnsmessage.Message{
			Header:    dnsmessage.Header{ID: request.ID, Response: true},
			Questions: request.Questions,
		}
		if q.Name.String() == "alias.example.test." {
			response.Answers = []dnsmessage.Resource{{
				Header: dnsmessage.ResourceHeader{
					Name: q.Name, Type: dnsmessage.TypeCNAME,
					Class: dnsmessage.ClassINET, TTL: 30,
				},
				Body: &dnsmessage.CNAMEResource{
					CNAME: dnsmessage.MustNewName("blocked.example.test."),
				},
			}}
			return response, nil
		}
		response.Answers = []dnsmessage.Resource{{
			Header: dnsmessage.ResourceHeader{
				Name: q.Name, Type: dnsmessage.TypeA,
				Class: dnsmessage.ClassINET, TTL: 30,
			},
			Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 55}},
		}}
		return response, nil
	})

	response := queryFilteredDNSBroker(
		t, broker, "alias.example.test.", dnsmessage.TypeA)
	assert.Equal(t, dnsmessage.RCodeRefused, response.RCode)
	assert.Empty(t, response.Answers)
	require.Len(t, leases.calls, 1)
	require.Len(t, leases.calls[0].matches.Allow, 1)
	require.Len(t, leases.calls[0].matches.Deny, 1)
	assert.Equal(t, "blocked.example.test",
		leases.calls[0].matches.Deny[0].Value)
}

func TestFilteredDNSBrokerDefaultAllowPortDenyReturnsLeasedAnswer(t *testing.T) {
	rules, err := sandboxpolicy.CompileFilteredNetworkRules(
		sandboxpolicy.NetworkRules{
			Mode: sandboxpolicy.AccessModeOpen,
			Deny: []sandboxpolicy.NetworkAllowEntry{{
				Host: "blocked.example.test", Ports: []int{443},
			}},
		})
	require.NoError(t, err)
	leases := &fakeFilteredDNSLeaseStore{}
	broker := testFilteredDNSBroker(rules, leases, func(
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
				Body: &dnsmessage.AResource{A: [4]byte{192, 0, 2, 56}},
			}},
		}, nil
	})

	response := queryFilteredDNSBroker(
		t, broker, "blocked.example.test.", dnsmessage.TypeA)
	assert.Equal(t, dnsmessage.RCodeSuccess, response.RCode)
	require.Len(t, response.Answers, 1)
	require.Len(t, leases.calls, 1)
	assert.Empty(t, leases.calls[0].matches.Allow)
	require.Len(t, leases.calls[0].matches.Deny, 1)
}

func TestValidateFilteredDNSUpstreamResponseBindsWholeQuestion(t *testing.T) {
	request := dnsmessage.Message{
		Header: dnsmessage.Header{ID: 41, RecursionDesired: true},
		Questions: []dnsmessage.Question{{
			Name: dnsmessage.MustNewName("api.example.test."),
			Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET,
		}},
	}
	response := dnsmessage.Message{
		Header:    dnsmessage.Header{ID: 41, Response: true},
		Questions: append([]dnsmessage.Question(nil), request.Questions...),
	}
	require.NoError(t, validateFilteredDNSUpstreamResponse(request, response))

	for name, mutate := range map[string]func(*dnsmessage.Message){
		"not_response": func(message *dnsmessage.Message) {
			message.Response = false
		},
		"opcode": func(message *dnsmessage.Message) {
			message.OpCode = dnsmessage.OpCode(1)
		},
		"id": func(message *dnsmessage.Message) {
			message.ID++
		},
		"name": func(message *dnsmessage.Message) {
			message.Questions[0].Name =
				dnsmessage.MustNewName("attacker.example.")
		},
		"type": func(message *dnsmessage.Message) {
			message.Questions[0].Type = dnsmessage.TypeAAAA
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := response
			candidate.Questions = append(
				[]dnsmessage.Question(nil), response.Questions...)
			mutate(&candidate)
			require.Error(t,
				validateFilteredDNSUpstreamResponse(request, candidate))
		})
	}
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

func TestFilteredDNSBrokerCIDROnlyRefusesNamesWithoutUpstreamLookup(t *testing.T) {
	rules := compileFilteredDNSRules(t, []sandboxpolicy.NetworkAllowEntry{{
		CIDR: "192.0.2.0/24", Ports: []int{443},
	}})
	upstreamCalled := false
	response := queryFilteredDNSBroker(
		t, testFilteredDNSBroker(rules, &fakeFilteredDNSLeaseStore{},
			func(context.Context, dnsmessage.Message) (dnsmessage.Message, error) {
				upstreamCalled = true
				return dnsmessage.Message{}, nil
			}),
		"arbitrary.example.", dnsmessage.TypeA,
	)
	assert.Equal(t, dnsmessage.RCodeRefused, response.RCode)
	assert.Empty(t, response.Answers)
	assert.False(t, upstreamCalled,
		"CIDR packet authority must not become arbitrary DNS-query authority")
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
	upserts []fakeFilteredNFTUpsert
}

type fakeFilteredNFTUpsert struct {
	sets    []string
	address netip.Addr
	ttl     time.Duration
}

func (f *fakeFilteredNFTLeaseAdder) Upsert(
	sets []string,
	address netip.Addr,
	ttl time.Duration,
) error {
	f.upserts = append(f.upserts, fakeFilteredNFTUpsert{
		sets: append([]string(nil), sets...), address: address, ttl: ttl,
	})
	return nil
}

func (*fakeFilteredNFTLeaseAdder) Close() error { return nil }

func TestFilteredDNSLeaseRefreshRequiresFreshAnswer(t *testing.T) {
	authority := &fakeFilteredNFTLeaseAdder{}
	leases := &filteredNetworkDNSLeases{
		authority: authority,
	}
	rules := sandboxpolicy.FilteredNetworkDNSMatches{
		Allow: []sandboxpolicy.FilteredNetworkRule{{EntryIndex: 4}},
	}
	address := netip.MustParseAddr("192.0.2.88")

	effective, err := leases.Ensure(rules, address, 30*time.Second)
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, effective)
	require.Len(t, authority.upserts, 1)
	assert.Equal(t, []string{"dns4_4"}, authority.upserts[0].sets)

	effective, err = leases.Ensure(
		sandboxpolicy.FilteredNetworkDNSMatches{
			Allow: append(rules.Allow,
				sandboxpolicy.FilteredNetworkRule{EntryIndex: 5}),
			Deny: []sandboxpolicy.FilteredNetworkRule{{EntryIndex: 2}},
		},
		address,
		60*time.Second,
	)
	require.NoError(t, err)
	assert.Equal(t, 60*time.Second, effective)
	require.Len(t, authority.upserts, 2)
	assert.Equal(t, []string{"dns4_d_2", "dns4_4", "dns4_5"},
		authority.upserts[1].sets)
	assert.Equal(t, 60*time.Second, authority.upserts[1].ttl,
		"a fresh DNS answer starts a fresh TTL-bound kernel lease")
}

func TestFilteredNFTLeaseMutationIsCreateOrRefresh(t *testing.T) {
	elementType := netlink.HeaderType(
		(unix.NFNL_SUBSYS_NFTABLES << 8) | unix.NFT_MSG_NEWSETELEM)
	requests := []netlink.Message{
		{Header: netlink.Header{
			Type: netlink.HeaderType(unix.NFNL_MSG_BATCH_BEGIN),
		}},
		{Header: netlink.Header{
			Type: elementType, Flags: netlink.Request | netlink.Create,
		}},
		{Header: netlink.Header{
			Type: netlink.HeaderType(unix.NFNL_MSG_BATCH_END),
		}},
	}
	markFilteredNetworkNFTUpserts(requests)
	assert.NotZero(t, requests[1].Header.Flags&netlink.Create)
	assert.NotZero(t, requests[1].Header.Flags&netlink.Replace)
	assert.Zero(t, requests[0].Header.Flags&netlink.Replace)
	assert.Zero(t, requests[2].Header.Flags&netlink.Replace)
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
