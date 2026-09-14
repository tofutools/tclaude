//go:build linux

package session

import (
	"context"
	"fmt"
	"net/netip"
	"os/exec"
	"slices"
	"strings"
	"time"

	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/sys/unix"
	"os"
)

type filteredDNSObservation struct {
	Names   []string
	Record  dnsmessage.Resource
	Expires time.Time
}

// Cache the whole CNAME path, including observations made under default-allow.
// A new deny can then revoke a cached answer without another DNS query.
func (b *filteredNetworkDNSBroker) observeDNS(record dnsmessage.Resource, seen map[string]struct{}) error {
	return b.observeDNSAt(record, seen, time.Now())
}

func (b *filteredNetworkDNSBroker) observeDNSAt(record dnsmessage.Resource, seen map[string]struct{}, now time.Time) error {
	b.observationMu.Lock()
	defer b.observationMu.Unlock()
	if b.observations == nil {
		b.observations = map[string]filteredDNSObservation{}
	}
	for key, observation := range b.observations {
		if !observation.Expires.After(now) {
			delete(b.observations, key)
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	slices.Sort(names)
	key := fmt.Sprintf("%v/%v", names, record.Body)
	if _, exists := b.observations[key]; !exists && len(b.observations) >= 8192 {
		return fmt.Errorf("DNS observation capacity reached; retry after cached answers expire")
	}
	ttl := min(time.Duration(record.Header.TTL)*time.Second, filteredNetworkDNSMaxLease)
	expires := now.Add(ttl)
	if previous, ok := b.observations[key]; ok && previous.Expires.After(expires) {
		expires = previous.Expires
	}
	b.observations[key] = filteredDNSObservation{Names: names, Record: record, Expires: expires}
	return nil
}

// nftLeaseBatch replays observations into the SAME transaction as the new
// base rules, so neither an empty deny set nor an obsolete allow set is visible.
type nftLeaseBatch struct{ elements map[string]time.Duration }

func (b *nftLeaseBatch) Close() error { return nil }
func (b *nftLeaseBatch) Ensure(matches sandboxpolicy.FilteredNetworkDNSMatches, addr netip.Addr, ttl time.Duration) (time.Duration, error) {
	for _, polarity := range []struct {
		deny  bool
		rules []sandboxpolicy.FilteredNetworkRule
	}{{false, matches.Allow}, {true, matches.Deny}} {
		for _, rule := range polarity.rules {
			v4, v6 := sandboxpolicy.FilteredNetworkDNSSetNamesForRule(polarity.deny, rule.EntryIndex)
			name := v6
			if addr.Is4() {
				name = v4
			}
			if b.elements == nil {
				b.elements = map[string]time.Duration{}
			}
			key := name + " " + addr.String()
			if ttl > b.elements[key] {
				b.elements[key] = ttl
			}
		}
	}
	return ttl, nil
}

func (p *preparedFilteredNetworkRelay) reloadNetworkRules(rules sandboxpolicy.FilteredNetworkRuleSet, namespacePID int) error {
	b := p.DNSBroker
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	policy, err := renderReloadNetworkPolicy(b, rules, time.Now())
	if err != nil {
		return err
	}
	if err := p.installReloadNetworkPolicy(policy); err != nil {
		return err
	}
	b.rules = rules
	p.Rules = rules
	p.Policy = policy
	return nil
}

func renderReloadNetworkPolicy(b *filteredNetworkDNSBroker, rules sandboxpolicy.FilteredNetworkRuleSet, now time.Time) (string, error) {
	policy, err := sandboxpolicy.RenderFilteredNetworkNFT(rules)
	if err != nil {
		return "", err
	}
	// Evaluate every outgoing packet against current authority. Keeping the
	// launch renderer's established bypass would grandfather removed allows.
	policy = strings.ReplaceAll(policy, "    ct state established accept\n", "")
	batch := &nftLeaseBatch{}
	replay := &filteredNetworkDNSBroker{rules: rules, leases: batch}
	for _, observation := range b.observations {
		ttl := observation.Expires.Sub(now).Truncate(time.Second)
		if ttl < time.Second {
			continue
		}
		matches := sandboxpolicy.FilteredNetworkDNSMatches{}
		for _, name := range observation.Names {
			m, err := sandboxpolicy.MatchFilteredNetworkDNSPolicy(rules, name)
			if err != nil {
				return "", err
			}
			matches.Allow = appendFilteredNetworkDNSMatches(matches.Allow, m.Allow)
			matches.Deny = appendFilteredNetworkDNSMatches(matches.Deny, m.Deny)
		}
		record := observation.Record
		record.Header.TTL = uint32(ttl / time.Second)
		if _, _, err := replay.leaseAddressRecord(record, matches); err != nil {
			return "", err
		}
	}
	keys := make([]string, 0, len(batch.elements))
	for key := range batch.elements {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var lines strings.Builder
	for _, key := range keys {
		set, addr, _ := strings.Cut(key, " ")
		fmt.Fprintf(&lines, "add element inet %s %s { %s timeout %ds }\n", sandboxpolicy.FilteredNetworkNFTTable, set, addr, max(1, int(batch.elements[key]/time.Second)))
	}
	return policy + lines.String(), nil
}

func (p *preparedFilteredNetworkRelay) installReloadNetworkPolicy(policy string) error {
	if p.sandboxNetns == nil {
		return fmt.Errorf("network namespace is not pinned")
	}
	ownerFD, err := unix.IoctlRetInt(int(p.sandboxNetns.Fd()), unix.NS_GET_USERNS)
	if err != nil {
		return err
	}
	owner := os.NewFile(uintptr(ownerFD), "network-sync-owner")
	defer func() { _ = owner.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, p.NsenterPath, filteredNetworkNsenterArgs(p.NFTPath, p.preserveCallerIdentity)...)
	cmd.Env = filteredNetworkHelperEnv()
	cmd.ExtraFiles = []*os.File{owner, p.sandboxNetns}
	cmd.Stdin = strings.NewReader(policy)
	if output, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() != nil {
			p.DNSBroker.fail(fmt.Errorf("network update outcome uncertain: %w", ctx.Err()))
		}
		return fmt.Errorf("atomic network policy update: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return nil
}
