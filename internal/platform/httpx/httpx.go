// Package httpx holds the JSON request and response helpers shared by HTTP handlers.
package httpx

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
)

// ErrorBody is the single error format of the API:
// {"error": {"code": "invalid_request", "message": "room is required"}}.
type ErrorBody struct {
	Error ErrorDetail `json:"error"`
}

type ErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Error codes returned in ErrorDetail.Code.
const (
	CodeInvalidRequest = "invalid_request"
	CodeBodyTooLarge   = "body_too_large"
	CodeInternal       = "internal"
)

func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func WriteError(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, ErrorBody{Error: ErrorDetail{Code: code, Message: message}})
}

// DecodeJSON reads a single JSON object of at most maxBytes into dst and rejects
// unknown fields. On failure it writes the error response and returns false.
func DecodeJSON(w http.ResponseWriter, r *http.Request, dst any, maxBytes int64) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBytes))
	dec.DisallowUnknownFields()

	err := dec.Decode(dst)
	if err == nil {
		if dec.Decode(&struct{}{}) != io.EOF {
			err = errors.New("body must contain a single JSON object")
		}
	}
	if err == nil {
		return true
	}

	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		WriteError(w, http.StatusRequestEntityTooLarge, CodeBodyTooLarge,
			fmt.Sprintf("request body must not exceed %d bytes", maxBytes))
		return false
	}
	WriteError(w, http.StatusBadRequest, CodeInvalidRequest, "invalid JSON body: "+err.Error())
	return false
}
