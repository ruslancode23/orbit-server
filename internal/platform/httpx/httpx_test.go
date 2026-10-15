package httpx

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type payload struct {
		Name string `json:"name"`
	}
	tests := []struct {
		name   string
		body   string
		ok     bool
		status int
		code   string
	}{
		{name: "valid", body: `{"name":"orbit"}`, ok: true},
		{name: "unknown field", body: `{"name":"orbit","admin":true}`, status: http.StatusBadRequest, code: CodeInvalidRequest},
		{name: "two objects", body: `{"name":"a"}{"name":"b"}`, status: http.StatusBadRequest, code: CodeInvalidRequest},
		{name: "malformed", body: `{"name":`, status: http.StatusBadRequest, code: CodeInvalidRequest},
		{name: "too large", body: `{"name":"` + strings.Repeat("x", 100) + `"}`, status: http.StatusRequestEntityTooLarge, code: CodeBodyTooLarge},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/", strings.NewReader(tt.body))

			var dst payload
			ok := DecodeJSON(rec, req, &dst, 64)

			if ok != tt.ok {
				t.Fatalf("DecodeJSON() = %v, want %v", ok, tt.ok)
			}
			if tt.ok {
				return
			}
			if rec.Code != tt.status {
				t.Errorf("status = %d, want %d", rec.Code, tt.status)
			}
			var body ErrorBody
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatalf("decode error body: %v", err)
			}
			if body.Error.Code != tt.code {
				t.Errorf("code = %q, want %q", body.Error.Code, tt.code)
			}
		})
	}
}
