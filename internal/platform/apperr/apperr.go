// Package apperr defines typed application errors. Every failure that crosses a
// layer boundary is one of these, each carrying an HTTP status and a stable code
// so the HTTP error middleware can map it to a consistent JSON response.
package apperr

import "fmt"

// Error is an expected, mappable application error.
type Error struct {
	Status int
	Code   string
	Msg    string
	// RetryAfter is set on rate-limit errors (seconds).
	RetryAfter int
	// Details carries structured, machine-readable sub-classification surfaced to
	// the client under error.details (R2). Optional.
	Details map[string]any
	// Hint is a human-readable fix suggestion surfaced under error.hint. Optional.
	Hint  string
	Cause error
}

func (e *Error) Error() string { return e.Msg }
func (e *Error) Unwrap() error { return e.Cause }

// WithDetails attaches structured details and returns the error for chaining.
func (e *Error) WithDetails(details map[string]any) *Error {
	e.Details = details
	return e
}

// WithHint attaches a human-readable hint and returns the error for chaining.
func (e *Error) WithHint(hint string) *Error {
	e.Hint = hint
	return e
}

func newErr(status int, code, msg string) *Error {
	return &Error{Status: status, Code: code, Msg: msg}
}

// Validation is a 400 — malformed or missing fields.
func Validation(msg, code string) *Error {
	if code == "" {
		code = "invalid_request"
	}
	return newErr(400, code, msg)
}

// Unauthorized is a 401 — missing or invalid credentials.
func Unauthorized(msg, code string) *Error {
	if msg == "" {
		msg = "unauthorized"
	}
	if code == "" {
		code = "unauthorized"
	}
	return newErr(401, code, msg)
}

// Forbidden is a 403 — authenticated but not permitted.
func Forbidden(msg, code string) *Error {
	if msg == "" {
		msg = "forbidden"
	}
	if code == "" {
		code = "forbidden"
	}
	return newErr(403, code, msg)
}

// NotFound is a 404.
func NotFound(msg string) *Error {
	if msg == "" {
		msg = "not found"
	}
	return newErr(404, "not_found", msg)
}

// Conflict is a 409.
func Conflict(msg, code string) *Error {
	if code == "" {
		code = "conflict"
	}
	return newErr(409, code, msg)
}

// PayloadTooLarge is a 413.
func PayloadTooLarge(msg, code string) *Error {
	if code == "" {
		code = "payload_too_large"
	}
	return newErr(413, code, msg)
}

// RateLimited is a 429 carrying a retry hint in seconds.
func RateLimited(retryAfterSeconds int) *Error {
	e := newErr(429, "rate_limited", "rate limit exceeded")
	e.RetryAfter = retryAfterSeconds
	return e
}

// Upstream is a 502 — a downstream dependency (storage, GitHub) failed.
func Upstream(msg, code string, cause error) *Error {
	if code == "" {
		code = "upstream_error"
	}
	e := newErr(502, code, msg)
	e.Cause = cause
	return e
}

// Wrap annotates an error's cause while preserving its type.
func (e *Error) Wrap(cause error) *Error {
	e.Cause = cause
	e.Msg = fmt.Sprintf("%s: %v", e.Msg, cause)
	return e
}
