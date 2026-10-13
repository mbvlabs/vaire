package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// publicResolvers are queried directly so a local cache cannot lie.
var publicResolvers = []string{"1.1.1.1:53", "8.8.8.8:53"}

// cloudflareRanges are Cloudflare's published proxy ranges
// (https://www.cloudflare.com/ips/). An answer inside them means the record
// is proxied (orange cloud), which database clients can't connect through.
var cloudflareRanges = mustPrefixes(
	"173.245.48.0/20", "103.21.244.0/22", "103.22.200.0/22", "103.31.4.0/22",
	"141.101.64.0/18", "108.162.192.0/18", "190.93.240.0/20", "188.114.96.0/20",
	"197.234.240.0/22", "198.41.128.0/17", "162.158.0.0/15", "104.16.0.0/13",
	"104.24.0.0/14", "172.64.0.0/13", "131.0.72.0/22",
	"2400:cb00::/32", "2606:4700::/32", "2803:f800::/32", "2405:b500::/32",
	"2405:8100::/32", "2a06:98c0::/29", "2c0f:f248::/32",
)

func mustPrefixes(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

// dnsRecord is one record the operator must create.
type dnsRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

// dnsRecords returns the records for a node's hostname: an A record for
// ipv4 and, when the node has one, an AAAA record for ipv6.
func dnsRecords(hostname, ipv4, ipv6 string) []dnsRecord {
	records := []dnsRecord{}
	if ipv4 != "" {
		records = append(records, dnsRecord{Type: "A", Name: hostname, Value: ipv4})
	}
	if ipv6 != "" {
		records = append(records, dnsRecord{Type: "AAAA", Name: hostname, Value: ipv6})
	}
	return records
}

// dnsAnswers are one resolver's A and AAAA answers.
type dnsAnswers struct {
	Resolver string   `json:"resolver"`
	A        []string `json:"a,omitempty"`
	AAAA     []string `json:"aaaa,omitempty"`
	Error    string   `json:"error,omitempty"`
}

// dnsCheck is the result of one check against the public resolvers.
type dnsCheck struct {
	Matches   bool         `json:"matches"`
	Proxied   bool         `json:"proxied"`
	Resolvers []dnsAnswers `json:"resolvers"`
}

// checkDNS queries each public resolver once and compares the answers with
// the node's addresses.
func checkDNS(ctx context.Context, hostname, ipv4, ipv6 string) dnsCheck {
	check := dnsCheck{Matches: ipv4 != "" || ipv6 != ""}
	for _, server := range publicResolvers {
		got := resolveWith(ctx, server, hostname)
		check.Resolvers = append(check.Resolvers, got)
		merged := map[string][]string{"A": got.A, "AAAA": got.AAAA}
		if !dnsMatches(merged, ipv4, ipv6) {
			check.Matches = false
		}
		for _, ip := range append(append([]string(nil), got.A...), got.AAAA...) {
			if inCloudflare(ip) {
				check.Proxied = true
			}
		}
	}
	return check
}

// resolveWith asks one resolver for the A and AAAA records of hostname.
func resolveWith(ctx context.Context, server, hostname string) dnsAnswers {
	out := dnsAnswers{Resolver: strings.TrimSuffix(server, ":53")}
	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "udp", server)
		},
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	ips, err := resolver.LookupNetIP(ctx, "ip4", hostname)
	if err == nil {
		for _, ip := range ips {
			out.A = appendUnique(out.A, ip.Unmap().String())
		}
	} else {
		out.Error = lookupError(err)
	}
	if ips, err := resolver.LookupNetIP(ctx, "ip6", hostname); err == nil {
		for _, ip := range ips {
			if !ip.Is4In6() {
				out.AAAA = appendUnique(out.AAAA, ip.String())
			}
		}
	}
	sort.Strings(out.A)
	sort.Strings(out.AAAA)
	return out
}

func lookupError(err error) string {
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		switch {
		case dnsErr.IsNotFound:
			return "no record"
		case dnsErr.IsTimeout:
			return "timeout"
		}
	}
	return err.Error()
}

func inCloudflare(ip string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	addr = addr.Unmap()
	for _, p := range cloudflareRanges {
		if p.Contains(addr) {
			return true
		}
	}
	return false
}

// describe renders one resolver's answers against what is expected, such as
// "1.1.1.1: A 104.21.27.128 (want 46.224.129.96)".
func (a dnsAnswers) describe(ipv4, ipv6 string) (string, bool) {
	ok := true
	var parts []string
	if ipv4 != "" {
		got := strings.Join(a.A, ", ")
		if got == "" {
			got = "none"
			if a.Error != "" {
				got = a.Error
			}
		}
		part := "A " + got
		if !contains(a.A, ipv4) {
			ok = false
			part += " (want " + ipv4 + ")"
		}
		parts = append(parts, part)
	}
	if ipv6 != "" {
		got := strings.Join(a.AAAA, ", ")
		if got == "" {
			got = "none"
		}
		part := "AAAA " + got
		if !contains(a.AAAA, ipv6) {
			ok = false
			part += " (want " + ipv6 + ")"
		}
		parts = append(parts, part)
	}
	return a.Resolver + ": " + strings.Join(parts, "; "), ok
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

// recordLines renders the records as aligned TYPE NAME VALUE lines that copy
// cleanly into a DNS provider's form.
func recordLines(records []dnsRecord) []string {
	nameWidth := 0
	for _, r := range records {
		nameWidth = max(nameWidth, len(r.Name))
	}
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, fmt.Sprintf("%-4s  %-*s  %s", r.Type, nameWidth, r.Name, r.Value))
	}
	return out
}
