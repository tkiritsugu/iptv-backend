// Package httpx holds the pieces of the HTTP layer that more than one package
// needs: the single error envelope, JSON responses and the request id.
//
// Every error the API returns has the same shape and carries no internal
// detail:
//
//	{"error":{"code":"channel_not_found","message":"...","request_id":"..."}}
//
// The code is stable and machine readable, the message is written for a human
// reading a set-top box log, and neither ever contains SQL text, file paths or
// anything else that helps an attacker more than a user.
package httpx

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
)

// ErrorObject is the body of every error response.
type ErrorObject struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"request_id,omitempty"`
}

// ErrorBody wraps the error object.
type ErrorBody struct {
	Error ErrorObject `json:"error"`
}

type ctxKey int

const requestIDKey ctxKey = iota

// ErrorStable is a domain error that already knows its HTTP status and error
// code. The service layer returns these so handlers can render them without a
// translation table.
type ErrorStable struct {
	Status  int
	Code    string
	Message string
}

func (e *ErrorStable) Error() string { return e.Code + ": " + e.Message }

// NewError builds a stable error for a status, code and message.
func NewError(status int, code, message string) *ErrorStable {
	return &ErrorStable{Status: status, Code: code, Message: message}
}

// Marshal encodes a payload without HTML escaping. Escaping is the default for
// good reason in HTML documents, but this is application/json: escaping turns
// every "&" in a playback URL into \u0026, which is noise for the client and for
// anyone reading a log. Values are never pasted into HTML by the admin panel,
// which writes them with textContent under a CSP that allows no inline script.
func Marshal(payload any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return nil, err
	}
	// Encode appends a newline; responses are single-line documents.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// JSON writes a response body.
func JSON(w http.ResponseWriter, r *http.Request, status int, payload any) error {
	body, err := Marshal(payload)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Request-Id", RequestID(r.Context()))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return nil
	}
	_, err = w.Write(body)
	return err
}

// Error writes an error envelope. The error is logged server side with detail
// by the caller, never here.
func Error(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	payload := ErrorBody{Error: ErrorObject{Code: code, Message: message, RequestID: RequestID(r.Context())}}
	body, err := Marshal(payload)
	if err != nil {
		// Cannot happen for this struct; keep the connection honest anyway.
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Request-Id", RequestID(r.Context()))
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// ErrorOf renders a stable error, or a generic 500 for anything else. It is the
// last line of defence against an unexpected error reaching the client: the
// detail goes to the log, the caller gets a fixed message.
func ErrorOf(w http.ResponseWriter, r *http.Request, log *slog.Logger, err error) {
	var stable *ErrorStable
	if errors.As(err, &stable) {
		Error(w, r, stable.Status, stable.Code, stable.Message)
		return
	}
	log.Error("unhandled request error", "error", err, "path", r.URL.Path)
	Error(w, r, http.StatusInternalServerError, "internal_error", "the request could not be completed")
}

// WithRequestID puts a fresh id into the context.
func WithRequestID(ctx context.Context) context.Context {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// A missing id is cosmetic; the request still needs to be served.
		return ctx
	}
	return context.WithValue(ctx, requestIDKey, hex.EncodeToString(raw[:]))
}

// RequestID returns the id of the request, or empty when none (background work,
// unit tests).
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(requestIDKey).(string)
	return s
}
