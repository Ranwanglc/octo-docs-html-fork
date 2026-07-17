package httpx

import (
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"strconv"

	"github.com/Mininglamp-OSS/octo-docs-html/internal/platform/apperr"
)

// Response envelope (OCTO API R1/R2). The wire contract:
//   - 2xx success: top-level {"data": ...}; lists add {"pagination": ...}
//   - 4xx/5xx:     top-level {"error": {"code","message","details","hint"}}
// The error code is one of the 12 fixed enum values (R2); the original
// fine-grained apperr code is preserved under error.details.code for debugging.

// dataEnvelope wraps any successful payload as {"data": ...}.
type dataEnvelope struct {
	Data any `json:"data"`
}

// pagination is the offset-paged list metadata (R5).
type pagination struct {
	Total    int `json:"total"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
}

// listEnvelope wraps a list payload as {"data":[...], "pagination":{...}}.
type listEnvelope struct {
	Data       any        `json:"data"`
	Pagination pagination `json:"pagination"`
}

// errorBody is the inner object of the failure envelope.
type errorBody struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
	Hint    string         `json:"hint,omitempty"`
}

// errorEnvelope wraps a failure as {"error": {...}}.
type errorEnvelope struct {
	Error errorBody `json:"error"`
}

// writeJSON marshals v at the given status with a JSON content type.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeData responds 200 with a single-object/empty envelope: {"data": v}.
// Pass struct{}{} (or a map) for the empty-success shape {"data":{}}.
func writeData(w http.ResponseWriter, status int, v any) {
	writeJSON(w, status, dataEnvelope{Data: v})
}

// writeList responds 200 with an offset-paged list envelope. items should be a
// slice; a nil slice is normalized to [] by the caller.
func writeList(w http.ResponseWriter, items any, p pagination) {
	writeJSON(w, http.StatusOK, listEnvelope{Data: items, Pagination: p})
}

// errorEnum maps an HTTP status to the fixed 12-value OCTO error code (R2).
func errorEnum(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "VALIDATION_ERROR"
	case http.StatusUnauthorized:
		return "AUTH_REQUIRED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusConflict:
		return "CONFLICT"
	case http.StatusRequestEntityTooLarge:
		return "PAYLOAD_TOO_LARGE"
	case http.StatusUnsupportedMediaType:
		return "UNSUPPORTED_MEDIA_TYPE"
	case http.StatusTooManyRequests:
		return "RATE_LIMITED"
	case http.StatusBadGateway, http.StatusServiceUnavailable:
		return "UPSTREAM_UNAVAILABLE"
	default:
		return "INTERNAL_ERROR"
	}
}

// writeErr maps an error to the failure envelope. A typed apperr.Error carries
// the HTTP status (→ enum code), message, optional details/hint; anything else
// is a generic 500. The original fine-grained apperr.Code is preserved under
// details.code so debugging context isn't lost in the enum collapse.
func writeErr(w http.ResponseWriter, logger *slog.Logger, err error) {
	var ae *apperr.Error
	if !errors.As(err, &ae) {
		logger.Error("unhandled error", "err", err.Error())
		writeJSON(w, http.StatusInternalServerError, errorEnvelope{Error: errorBody{
			Code: "INTERNAL_ERROR", Message: "an unexpected error occurred",
		}})
		return
	}

	if ae.Status >= 500 {
		logger.Error("request failed", "code", ae.Code, "err", ae.Msg)
	} else {
		logger.Info("request error", "code", ae.Code, "msg", ae.Msg)
	}

	details := map[string]any{}
	maps.Copy(details, ae.Details)
	// Preserve the fine-grained internal code for debugging unless the caller
	// already set one explicitly.
	if ae.Code != "" {
		if _, ok := details["code"]; !ok {
			details["code"] = ae.Code
		}
	}
	if ae.Status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", strconv.Itoa(ae.RetryAfter))
		details["retry_after_seconds"] = ae.RetryAfter
	}
	if len(details) == 0 {
		details = nil
	}

	writeJSON(w, ae.Status, errorEnvelope{Error: errorBody{
		Code:    errorEnum(ae.Status),
		Message: ae.Msg,
		Details: details,
		Hint:    ae.Hint,
	}})
}
