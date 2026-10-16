// Package httpx writes JSON responses in the API's common format.
// Handlers return generated response types (internal/api); httpx covers what
// happens outside them: middleware, validation and routing errors.
package httpx

import (
	"encoding/json"
	"net/http"
)

// ErrorBody mirrors the Error schema of api/openapi.yaml.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes returned in ErrorDetail.Code.
const (
	CodeInvalidRequest   = "invalid_request"
	CodeNotFound         = "not_found"
	CodeMethodNotAllowed = "method_not_allowed"
	CodeBodyTooLarge     = "body_too_large"
	CodeInternal         = "internal"
)

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}
