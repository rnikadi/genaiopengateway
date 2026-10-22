package server

import (
	"encoding/json"
	"net/http"

	"github.com/rnikadi/genaiopengateway/internal/provider"
)

// Gateway error codes; see docs/api.md.
const (
	codeInvalidRequest   = "invalid_request"
	codeInvalidKey       = "invalid_api_key"
	codeModelNotAllowed  = "model_not_allowed"
	codeModelNotFound    = "model_not_found"
	codeTooLarge         = "request_too_large"
	codeAllTargetsFailed = "all_targets_failed"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError replies in the error shape of the endpoint's API family so
// existing SDKs surface the message.
func writeError(w http.ResponseWriter, ep provider.Endpoint, status int, code, msg string) {
	if ep == provider.Messages {
		writeJSON(w, status, map[string]any{
			"type":  "error",
			"error": map[string]any{"type": anthropicErrorType(status), "message": msg},
		})
		return
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
}

func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable:
		return "overloaded_error"
	}
	return "api_error"
}
