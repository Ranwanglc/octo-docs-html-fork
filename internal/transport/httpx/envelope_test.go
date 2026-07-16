package httpx

import (
	"net/http"
	"testing"
)

// TestErrorEnumUpstream verifies that a 502 (apperr.Upstream) maps to the
// UPSTREAM_UNAVAILABLE enum rather than falling through to INTERNAL_ERROR.
func TestErrorEnumUpstream(t *testing.T) {
	cases := map[int]string{
		http.StatusBadGateway:          "UPSTREAM_UNAVAILABLE",
		http.StatusServiceUnavailable:  "UPSTREAM_UNAVAILABLE",
		http.StatusBadRequest:          "VALIDATION_ERROR",
		http.StatusTooManyRequests:     "RATE_LIMITED",
		http.StatusInternalServerError: "INTERNAL_ERROR",
	}
	for status, want := range cases {
		if got := errorEnum(status); got != want {
			t.Errorf("errorEnum(%d) = %q, want %q", status, got, want)
		}
	}
}
