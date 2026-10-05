// Package server exposes the application as a JSON API over HTTP.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/IsaacFW/reflectingpool/internal/auth"
	"github.com/IsaacFW/reflectingpool/internal/core"
	"github.com/IsaacFW/reflectingpool/internal/index"
)

type Options struct {
	// TLS is set when this process terminates HTTPS itself.
	TLS bool
	// TrustProxy makes the server believe X-Forwarded-For and
	// X-Forwarded-Proto. Only set it behind a reverse proxy that overwrites
	// them; otherwise clients could forge their address to dodge lockouts.
	TrustProxy bool
	Version    string
}

type Server struct {
	app  *core.App
	auth *auth.Service
	opts Options
}

func New(app *core.App, a *auth.Service, opts Options) *Server {
	return &Server{app: app, auth: a, opts: opts}
}

const maxBody = 1 << 20

type authedHandler func(w http.ResponseWriter, r *http.Request, sess auth.Session)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Everything except these three routes requires a session.
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)

	routes := map[string]authedHandler{
		"POST /api/logout":                    s.logout,
		"GET /api/session":                    s.session,
		"GET /api/scan":                       s.scanStatus,
		"POST /api/scan":                      s.scanStart,
		"GET /api/storage":                    s.storage,
		"GET /api/shares":                     s.shares,
		"GET /api/shares/{id}/annotations":    s.shareAnnotations,
		"GET /api/tree":                       s.tree,
		"GET /api/entries":                    s.entries,
		"GET /api/entries/{id}":               s.entry,
		"PUT /api/entries/{id}/annotation":    s.putAnnotation,
		"DELETE /api/entries/{id}/annotation": s.deleteAnnotation,
		"GET /api/entries/{id}/content":       s.content,
		"GET /api/prefixes":                   s.getPrefixes,
		"PUT /api/prefixes":                   s.putPrefixes,
		"POST /api/queue":                     s.queue,
	}
	for pattern, h := range routes {
		mux.HandleFunc(pattern, s.authed(h))
	}
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "Reflecting Pool is running. The web interface has not been built yet; the API is under /api/.")
	})
	return s.headers(mux)
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		h.Set("Cache-Control", "no-store")
		if !safeMethod(r.Method) && !sameOrigin(r) {
			writeError(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// sameOrigin rejects state-changing requests that a browser says came from
// another site. Non-browser clients send no Origin and are let through; they
// still need the session cookie and CSRF token.
func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) secure(r *http.Request) bool {
	return s.opts.TLS || (s.opts.TrustProxy && r.Header.Get("X-Forwarded-Proto") == "https")
}

// cookieName uses the __Host- prefix over HTTPS, which makes browsers refuse
// the cookie unless it is Secure, host-only and scoped to the whole site.
func (s *Server) cookieName(r *http.Request) string {
	if s.secure(r) {
		return "__Host-rp_session"
	}
	return "rp_session"
}

func (s *Server) clientAddr(r *http.Request) string {
	if s.opts.TrustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// The last entry is the one our own proxy appended.
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) token(r *http.Request) string {
	c, err := r.Cookie(s.cookieName(r))
	if err != nil {
		return ""
	}
	return c.Value
}

func (s *Server) authed(h authedHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.auth.Authenticate(s.token(r))
		if err != nil {
			s.fail(w, err)
			return
		}
		if !safeMethod(r.Method) {
			got := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) != 1 {
				writeError(w, http.StatusForbidden, "missing or wrong X-CSRF-Token header")
				return
			}
		}
		h(w, r, sess)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// fail turns an error into a response. Errors the user can act on keep their
// message; anything else is logged and reported without detail.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var locked *auth.LockedError
	var invalid auth.ValidationError
	var input core.InputError
	var query index.QueryError
	var tooBig *http.MaxBytesError
	status := http.StatusInternalServerError
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		status = http.StatusTooManyRequests
	case errors.Is(err, auth.ErrInvalid), errors.Is(err, auth.ErrNoSession):
		status = http.StatusUnauthorized
	case errors.Is(err, auth.ErrSetupCode), errors.Is(err, core.ErrReadOnly):
		status = http.StatusForbidden
	case errors.Is(err, index.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, auth.ErrSetupDone), errors.Is(err, core.ErrNoIndex), errors.Is(err, core.ErrScanRunning), errors.Is(err, core.ErrGone):
		status = http.StatusConflict
	case errors.As(err, &invalid), errors.As(err, &input), errors.As(err, &query), errors.As(err, new(badRequest)),
		errors.Is(err, core.ErrNoShare), errors.Is(err, core.ErrNotFile):
		status = http.StatusBadRequest
	case errors.As(err, &tooBig):
		status = http.StatusRequestEntityTooLarge
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
		writeError(w, status, "internal error; see the server log")
		return
	}
	writeError(w, status, err.Error())
}

type badRequest string

func (e badRequest) Error() string { return string(e) }

func decode(r *http.Request, v any) error {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return badRequest("the request body must be application/json")
	}
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return badRequest("malformed JSON: " + err.Error())
	}
	return nil
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, index.ErrNotFound
	}
	return id, nil
}

// query reads typed values from the URL, remembering the first malformed one.
type query struct {
	v   url.Values
	err error
}

func (q *query) int(name string, def int64) int64 {
	raw := q.v.Get(name)
	if raw == "" {
		return def
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || n < 0 {
		if q.err == nil {
			q.err = badRequest(fmt.Sprintf("%s must be a non-negative whole number", name))
		}
		return def
	}
	return n
}

func (q *query) list(name string) []string {
	raw := q.v.Get(name)
	if raw == "" {
		return nil
	}
	return strings.Split(raw, ",")
}

func (q *query) ids(name string) []int64 {
	var out []int64
	for _, part := range q.list(name) {
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil {
			if q.err == nil {
				q.err = badRequest(fmt.Sprintf("%s must be a comma-separated list of IDs", name))
			}
			return nil
		}
		out = append(out, n)
	}
	return out
}

func (q *query) page(defLimit, maxLimit int64) (limit, offset int) {
	return int(min(q.int("limit", defLimit), maxLimit)), int(q.int("offset", 0))
}

func (q *query) sort() index.Sort {
	return index.Sort{Key: q.v.Get("sort"), Desc: q.v.Get("desc") == "1" || (q.v.Get("sort") == "" && q.v.Get("desc") == "")}
}
