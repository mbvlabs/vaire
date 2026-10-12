package cli

import (
	"context"
	"fmt"
	"net"
	"sort"
	"time"
)

// dnsPollInterval and dnsTimeout bound the DNS verification (D10).
const (
	dnsPollInterval = 10 * time.Second
	dnsTimeout      = 5 * time.Minute
)

// publicResolvers are queried directly so a local cache cannot lie.
var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// dnsRecord is one record the operator must create.
type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// dnsRecords returns the records for domain: an A record for ipv4 and, when
// the node has one, an AAAA record for ipv6.
func dnsRecords(domain, ipv4, ipv6 string) []dnsRecord {
	records := []dnsRecord{}
	if ipv4 != "" {
		records = append(records, dnsRecord{Type: "A", Name: domain, Value: ipv4})
	}
	if ipv6 != "" {
		records = append(records, dnsRecord{Type: "AAAA", Name: domain, Value: ipv6})
	}
	return records
}

// verifyDNS polls the public resolvers until domain resolves to ipv4 (and, when
// given, ipv6), or until ctx is done. It returns the answers seen.
func verifyDNS(ctx context.Context, domain, ipv4, ipv6 string) (bool, map[string][]string) {
	deadline := time.Now().Add(dnsTimeout)
	for {
		got := resolveDomain(ctx, domain)
		if dnsMatches(got, ipv4, ipv6) {
			return true, got
		}
		if time.Now().After(deadline) {
			return false, got
		}
		select {
		case <-ctx.Done():
			return false, got
		case <-time.After(dnsPollInterval):
		}
	}
}

// resolveDomain queries each public resolver for A and AAAA records.
func resolveDomain(ctx context.Context, domain string) map[string][]string {
	out := map[string][]string{}
	for _, server := range publicResolvers {
		resolver := &net.Resolver{
			PreferGo: true,
			Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", server)
			},
		}
		if ips, err := resolver.LookupNetIP(ctx, "ip4", domain); err == nil {
			for _, ip := range ips {
				out["A"] = appendUnique(out["A"], ip.Unmap().String())
			}
		}
		if ips, err := resolver.LookupNetIP(ctx, "ip6", domain); err == nil {
			for _, ip := range ips {
				if ip.Is4In6() {
					continue
				}
				out["AAAA"] = appendUnique(out["AAAA"], ip.String())
			}
		}
	}
	for _, values := range out {
		sort.Strings(values)
	}
	return out
}

// dnsMatches reports whether the resolved answers contain the expected
// addresses.
func dnsMatches(got map[string][]string, ipv4, ipv6 string) bool {
	if ipv4 != "" && !contains(got["A"], ipv4) {
		return false
	}
	if ipv6 != "" && !contains(got["AAAA"], ipv6) {
		return false
	}
	return ipv4 != "" || ipv6 != ""
}

func contains(values []string, want string) bool {
	for _, v := range values {
		if v == want {
			return true
		}
	}
	return false
}

func appendUnique(values []string, v string) []string {
	for _, existing := range values {
		if existing == v {
			return values
		}
	}
	return append(values, v)
}

// formatRecords renders the records as indented lines for humans.
func formatRecords(records []dnsRecord) string {
	out := ""
	for _, r := range records {
		out += fmt.Sprintf("  %s %s -> %s\n", r.Type, r.Name, r.Value)
	}
	return out
}
