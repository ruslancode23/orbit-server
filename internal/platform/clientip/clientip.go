// Package clientip finds the address a request came from, for limits keyed by
// address. X-Forwarded-For is trusted only from the proxies an operator listed:
// anyone can send the header, so without that list it would let a client pick
// its own address and walk around every limit.
package clientip

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

type ctxKey struct{}

// ParseTrusted reads a list such as "10.0.0.0/8, 127.0.0.1": a bare address is
// a prefix of one.
func ParseTrusted(entries []string) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(entries))
	for _, entry := range entries {
		if strings.Contains(entry, "/") {
			prefix, err := netip.ParsePrefix(entry)
			if err != nil {
				return nil, fmt.Errorf("trusted proxy %q: %w", entry, err)
			}
			out = append(out, prefix.Masked())
			continue
		}
		addr, err := netip.ParseAddr(entry)
		if err != nil {
			return nil, fmt.Errorf("trusted proxy %q: %w", entry, err)
		}
		addr = addr.Unmap()
		out = append(out, netip.PrefixFrom(addr, addr.BitLen()))
	}
	return out, nil
}

// Middleware puts the client address in the request context.
func Middleware(trusted []netip.Prefix) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			addr := Resolve(r, trusted)
			if addr.IsValid() {
				r = r.WithContext(WithAddr(r.Context(), addr.String()))
			}
			next.ServeHTTP(w, r)
		})
	}
}

// WithAddr is what Middleware does, for code that builds a context itself,
// such as a handler test.
func WithAddr(ctx context.Context, addr string) context.Context {
	return context.WithValue(ctx, ctxKey{}, addr)
}

// From is the address Middleware found, or "" when there was none.
func From(ctx context.Context) string {
	addr, _ := ctx.Value(ctxKey{}).(string)
	return addr
}

// Resolve is the connection's peer, unless that peer is a trusted proxy. Then
// X-Forwarded-For is read from the right, skipping the trusted hops: the first
// address that is not one of ours is the client. Entries left of it were
// written by the client and prove nothing.
func Resolve(r *http.Request, trusted []netip.Prefix) netip.Addr {
	peer := remoteAddr(r.RemoteAddr)
	if !peer.IsValid() || !isTrusted(peer, trusted) {
		return peer
	}

	hops := forwardedHops(r.Header.Values("X-Forwarded-For"))
	client := peer
	for i := len(hops) - 1; i >= 0; i-- {
		addr, err := netip.ParseAddr(hops[i])
		if err != nil {
			// A garbled hop ends the chain we can believe.
			return client
		}
		client = addr.Unmap()
		if !isTrusted(client, trusted) {
			return client
		}
	}
	return client
}

func remoteAddr(raw string) netip.Addr {
	host, _, err := net.SplitHostPort(raw)
	if err != nil {
		host = raw
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return addr.Unmap()
}

func forwardedHops(values []string) []string {
	var hops []string
	for _, value := range values {
		for part := range strings.SplitSeq(value, ",") {
			if part = strings.TrimSpace(part); part != "" {
				hops = append(hops, part)
			}
		}
	}
	return hops
}

func isTrusted(addr netip.Addr, trusted []netip.Prefix) bool {
	for _, prefix := range trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}
