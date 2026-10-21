package clientip

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolve(t *testing.T) {
	trusted, err := ParseTrusted([]string{"10.0.0.0/8", "127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		remote    string
		forwarded []string
		want      string
	}{
		{"no proxy", "203.0.113.7:5000", nil, "203.0.113.7"},
		{"untrusted peer cannot forge the header", "203.0.113.7:5000", []string{"1.2.3.4"}, "203.0.113.7"},
		{"trusted proxy names the client", "127.0.0.1:5000", []string{"198.51.100.2"}, "198.51.100.2"},
		{"client-written entries left of ours are ignored", "127.0.0.1:5000", []string{"1.2.3.4, 198.51.100.2"}, "198.51.100.2"},
		{"several trusted hops are skipped", "127.0.0.1:5000", []string{"198.51.100.2, 10.1.2.3"}, "198.51.100.2"},
		{"split headers read as one chain", "127.0.0.1:5000", []string{"198.51.100.2", "10.1.2.3"}, "198.51.100.2"},
		{"garbage stops the chain at the last good hop", "127.0.0.1:5000", []string{"198.51.100.2, nonsense, 10.1.2.3"}, "10.1.2.3"},
		{"trusted proxy without the header", "127.0.0.1:5000", nil, "127.0.0.1"},
		{"ipv6 peer", "[2001:db8::1]:443", nil, "2001:db8::1"},
		{"ipv4-mapped peer", "[::ffff:127.0.0.1]:5000", []string{"198.51.100.2"}, "198.51.100.2"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			r.RemoteAddr = tt.remote
			for _, value := range tt.forwarded {
				r.Header.Add("X-Forwarded-For", value)
			}
			if got := Resolve(r, trusted).String(); got != tt.want {
				t.Errorf("Resolve = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestMiddlewarePutsTheAddressInTheContext(t *testing.T) {
	var got string
	handler := Middleware(nil)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = From(r.Context())
	}))
	r := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	r.RemoteAddr = "203.0.113.7:5000"
	handler.ServeHTTP(httptest.NewRecorder(), r)
	if got != "203.0.113.7" {
		t.Errorf("From = %q, want 203.0.113.7", got)
	}
}

func TestParseTrustedRejectsGarbage(t *testing.T) {
	if _, err := ParseTrusted([]string{"10.0.0.0/33"}); err == nil {
		t.Error("a bad prefix was accepted")
	}
	if _, err := ParseTrusted([]string{"proxy.local"}); err == nil {
		t.Error("a host name was accepted")
	}
}
