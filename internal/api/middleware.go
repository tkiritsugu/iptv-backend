package api

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"iptv/internal/auth"
	"iptv/internal/httpx"
)

// recoverMiddleware turns a panic into a 500 and a stack trace in the log.
// Without it a panic in a handler would kill the connection with no answer and,
// depending on the server configuration, could take the process with it.
func recoverMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Error("panic in handler",
					"panic", rec,
					"path", r.URL.Path,
					"request_id", httpx.RequestID(r.Context()),
					"stack", string(debug.Stack()))
				httpx.Error(w, r, http.StatusInternalServerError, "internal_error",
					"the request could not be completed")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// requestIDMiddleware gives every request an id, returned in X-Request-Id and
// repeated in error bodies, so a support ticket that says "the box shows
// 8f3a1c0b" leads to the exact log lines.
func requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		next.ServeHTTP(w, r.WithContext(httpx.WithRequestID(r.Context())))
	})
}

// accessLogMiddleware logs one line per request. The fields are an explicit
// allow list: the Authorization header, passwords and query parameters are never
// logged.
func accessLogMiddleware(next http.Handler, log *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &recorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		attrs := []any{
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"bytes", rec.bytes,
			"duration_ms", float64(time.Since(start).Microseconds()) / 1000,
			"client", clientHost(r),
			"request_id", httpx.RequestID(r.Context()),
		}
		if principal, ok := auth.FromContext(r.Context()); ok {
			attrs = append(attrs, "user_id", principal.UserID)
		}
		switch {
		case rec.status >= 500:
			log.Error("request", attrs...)
		case rec.status >= 400:
			log.Warn("request", attrs...)
		case r.URL.Path == "/healthz" || r.URL.Path == "/readyz":
			log.Debug("request", attrs...)
		default:
			log.Info("request", attrs...)
		}
	})
}

// timeoutMiddleware bounds the work a single request may do. It is separate from
// the server's write timeout on purpose: a slow client streaming a body must not
// be able to hold a handler and its database connection open for minutes.
func timeoutMiddleware(next http.Handler, timeout time.Duration) http.Handler {
	if timeout <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// bodyLimitMiddleware caps the request body. Reads are wrapped so a handler that
// forgets to limit gets a truncated reader plus a clear error.
func bodyLimitMiddleware(next http.Handler, maxBytes int64) http.Handler {
	if maxBytes <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders sets the small set of headers that matter for an API served to
// machines plus a browser based admin panel.
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		head := w.Header()
		head.Set("X-Content-Type-Options", "nosniff")
		head.Set("X-Frame-Options", "DENY")
		head.Set("Referrer-Policy", "no-referrer")
		// A strict CSP for the panel: default-deny, with same-origin scripts,
		// styles and fetches allowed. connect-src is what lets the page call the
		// admin API; without it the page renders and every request inside it is
		// blocked, which is exactly the kind of bug a screenshot catches and an
		// API test does not.
		head.Set("Content-Security-Policy", strings.Join([]string{
			"default-src 'none'",
			"script-src 'self'",
			"style-src 'self'",
			"connect-src 'self'",
			"img-src 'self' data:",
			"form-action 'self'",
			"base-uri 'none'",
			"frame-ancestors 'none'",
		}, "; "))
		next.ServeHTTP(w, r)
	})
}

// recorder captures what a handler sent so the access log can report it.
type recorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *recorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.bytes += n
	return n, err
}

// Flush lets http.ResponseController reach the underlying writer.
func (r *recorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func clientHost(r *http.Request) string {
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// errNoBody is returned when a request body that must be present is empty.
var errNoBody = httpx.NewError(http.StatusBadRequest, "empty_body", "a request body is required")

// readAll is used by tests and the panel to bound what is buffered.
func readAll(r *http.Request, limit int64) ([]byte, error) {
	if r.Body == nil {
		return nil, errNoBody
	}
	defer r.Body.Close()
	body, err := io.ReadAll(io.LimitReader(r.Body, limit))
	if err != nil {
		return nil, httpx.NewError(http.StatusBadRequest, "unreadable_body", "the request body could not be read")
	}
	if len(body) == 0 {
		return nil, errNoBody
	}
	return body, nil
}
