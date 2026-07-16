package apperr

import (
	"errors"
	"testing"
)

func TestConstructorsCarryStatusAndCode(t *testing.T) {
	cases := []struct {
		err    *Error
		status int
		code   string
	}{
		{Validation("bad", ""), 400, "invalid_request"},
		{Validation("bad", "custom"), 400, "custom"},
		{Unauthorized("", ""), 401, "unauthorized"},
		{Forbidden("", ""), 403, "forbidden"},
		{NotFound(""), 404, "not_found"},
		{Conflict("c", ""), 409, "conflict"},
		{PayloadTooLarge("big", ""), 413, "payload_too_large"},
		{RateLimited(7), 429, "rate_limited"},
		{Upstream("down", "", nil), 502, "upstream_error"},
	}
	for _, c := range cases {
		if c.err.Status != c.status {
			t.Errorf("%s: status = %d, want %d", c.code, c.err.Status, c.status)
		}
		if c.err.Code != c.code {
			t.Errorf("status %d: code = %q, want %q", c.status, c.err.Code, c.code)
		}
	}
}

func TestRateLimitedCarriesRetry(t *testing.T) {
	if RateLimited(13).RetryAfter != 13 {
		t.Error("retry-after not carried")
	}
}

func TestWrapAndUnwrap(t *testing.T) {
	cause := errors.New("root cause")
	e := Upstream("failed", "x", cause)
	if !errors.Is(e, cause) {
		t.Error("Unwrap should expose the cause")
	}
	wrapped := Validation("bad", "v").Wrap(cause)
	if !errors.Is(wrapped, cause) {
		t.Error("Wrap should set the cause")
	}
	if wrapped.Error() == "" {
		t.Error("error message empty")
	}
}

func TestErrorsAsType(t *testing.T) {
	var target *Error
	if !errors.As(NotFound("x"), &target) {
		t.Error("apperr.Error should be matchable via errors.As")
	}
}
