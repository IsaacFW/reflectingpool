// Package server exposes the application as a JSON API over HTTP.
package server

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/auth"
	"github.com/IsaacFW/reflectingpool/internal/core"
	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
	"github.com/IsaacFW/reflectingpool/internal/preview"
	"github.com/IsaacFW/reflectingpool/web"
)

type Options struct {
	// TLS is set when this process terminates HTTPS itself.
	TLS bool
	// TrustProxy makes the server believe X-Forwarded-For and
	// X-Forwarded-Proto. Only set it behind a reverse proxy that overwrites
	// them; otherwise clients could forge their address to dodge lockouts.
	TrustProxy bool
	Version    string
	// Web holds the interface's files. The ones built into the binary are
	// used when it is nil; a folder on disk can stand in during development.
	Web fs.FS
}

type Server struct {
	app  *core.App
	auth *auth.Service
	opts Options
	web  fs.FS
}

func New(app *core.App, a *auth.Service, opts Options) *Server {
	s := &Server{app: app, auth: a, opts: opts, web: opts.Web}
	if s.web == nil {
		s.web = web.Files
	}
	return s
}

const maxBody = 1 << 20

type authedHandler func(w http.ResponseWriter, r *http.Request, sess auth.Session)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	// Everything except these three routes requires a session.
	mux.HandleFunc("GET /api/health", s.health)
	mux.HandleFunc("POST /api/setup", s.setup)
	mux.HandleFunc("POST /api/login", s.login)

	// Routes that take or return entry IDs are tied to one index: see authed.
	byID := map[string]authedHandler{
		"GET /api/shares":                     s.shares,
		"GET /api/shares/{id}/annotations":    s.shareAnnotations,
		"DELETE /api/shares/{id}/annotations": s.deleteShareAnnotation,
		"GET /api/tree":                       s.tree,
		"GET /api/entries":                    s.entries,
		"GET /api/entries/{id}":               s.entry,
		"PUT /api/entries/{id}/annotation":    s.putAnnotation,
		"DELETE /api/entries/{id}/annotation": s.deleteAnnotation,
		"GET /api/entries/{id}/content":       s.content,
		"GET /api/entries/{id}/preview":       s.preview,
		"POST /api/queue":                     s.queue,
		"POST /api/queue/skip":                s.queueSkip,
	}
	// These work the same whichever index is in use, and are how a client
	// finds its feet again after a scan.
	anyIndex := map[string]authedHandler{
		"POST /api/logout":        s.logout,
		"GET /api/session":        s.session,
		"GET /api/scan":           s.scanStatus,
		"POST /api/scan":          s.scanStart,
		"DELETE /api/scan":        s.scanStop,
		"GET /api/settings":       s.getSettings,
		"PUT /api/settings":       s.putSettings,
		"GET /api/storage":        s.storage,
		"GET /api/entries/lookup": s.lookup,
		"GET /api/prefixes":       s.getPrefixes,
		"PUT /api/prefixes":       s.putPrefixes,
	}
	for pattern, h := range byID {
		mux.HandleFunc(pattern, s.authed(h, true))
	}
	for pattern, h := range anyIndex {
		mux.HandleFunc(pattern, s.authed(h, false))
	}
	// The interface: one page for every screen, and the files it loads.
	mux.HandleFunc("GET /{$}", s.shell)
	for _, screen := range screens {
		mux.HandleFunc("GET /"+screen, s.shell)
	}
	mux.HandleFunc("GET /style.css", s.static)
	mux.HandleFunc("GET /app/{file}", s.static)
	mux.HandleFunc("GET /lib/{file}", s.static)
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
			writeError(w, http.StatusForbidden, "cross_origin", "cross-origin request refused")
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

// indexHeader carries the ID of the index in use: on every response, and on
// requests from a client saying which index its entry IDs came from.
const indexHeader = "X-RP-Index"

// authed wraps a handler that needs a session.
//
// Entry IDs are reassigned by every scan, so an ID a client has been holding
// can come to mean a different item. For routes that work with entry IDs
// (byID), a request that names an older index is refused with 409 and the
// code "index_changed"; the client then reloads and finds its place by path.
// Requests that change something must name their index.
func (s *Server) authed(h authedHandler, byID bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sess, err := s.auth.Authenticate(s.token(r))
		if err != nil {
			s.fail(w, err)
			return
		}
		if !safeMethod(r.Method) {
			got := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(sess.CSRF)) != 1 {
				writeError(w, http.StatusForbidden, "csrf", "missing or wrong X-CSRF-Token header")
				return
			}
		}
		current := s.app.IndexID()
		if current != "" {
			w.Header().Set(indexHeader, current)
		}
		if byID {
			claimed := r.Header.Get(indexHeader)
			switch {
			case claimed == "" && !safeMethod(r.Method):
				s.fail(w, errIndexRequired)
				return
			case claimed != "" && current != "" && claimed != current:
				s.fail(w, core.ErrIndexChanged)
				return
			}
			// Checked again inside the operation, while the index is held.
			r = r.WithContext(core.AtIndex(r.Context(), claimed))
		}
		h(w, r, sess)
	}
}

var errIndexRequired = errors.New("this request changes something by entry ID, so it must name the index the ID came from in the " + indexHeader + " header")

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError sends an error with a stable code. Clients act on the code; the
// message is for a person.
func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, map[string]string{"error": msg, "code": code})
}

// fail turns an error into a response. Errors the user can act on keep their
// message; anything else is logged and reported without detail.
func (s *Server) fail(w http.ResponseWriter, err error) {
	var locked *auth.LockedError
	var invalid auth.ValidationError
	var input core.InputError
	var query index.QueryError
	var tooBig *http.MaxBytesError
	status, code := http.StatusInternalServerError, "internal"
	switch {
	case errors.As(err, &locked):
		w.Header().Set("Retry-After", strconv.Itoa(int(locked.RetryAfter.Seconds())+1))
		status, code = http.StatusTooManyRequests, "locked"
	case errors.Is(err, auth.ErrBusy):
		w.Header().Set("Retry-After", "2")
		status, code = http.StatusTooManyRequests, "busy"
	case errors.Is(err, errBodyTimeout):
		status, code = http.StatusRequestTimeout, "timeout"
	case errors.Is(err, auth.ErrNoSession):
		status, code = http.StatusUnauthorized, "not_signed_in"
	case errors.Is(err, auth.ErrInvalid):
		status, code = http.StatusUnauthorized, "invalid_credentials"
	case errors.Is(err, auth.ErrSetupCode):
		status, code = http.StatusForbidden, "bad_setup_code"
	case errors.Is(err, preview.ErrUnsupported):
		status, code = http.StatusNotFound, "no_preview"
	case errors.Is(err, preview.ErrNoTool):
		status, code = http.StatusServiceUnavailable, "previews_unavailable"
	case errors.As(err, new(*preview.FailedError)):
		status, code = http.StatusUnprocessableEntity, "preview_failed"
	case errors.As(err, new(*core.ShareReadOnlyError)):
		status, code = http.StatusForbidden, "share_read_only"
	case errors.As(err, new(*core.ShareUnsafeError)), errors.As(err, new(*meta.Suspect)):
		status, code = http.StatusForbidden, "share_unsafe"
	case errors.Is(err, core.ErrReadOnly):
		status, code = http.StatusForbidden, "read_only"
	case errors.Is(err, index.ErrNotFound):
		status, code = http.StatusNotFound, "not_found"
	case errors.Is(err, auth.ErrSetupDone):
		status, code = http.StatusConflict, "setup_done"
	case errors.Is(err, core.ErrNoIndex):
		status, code = http.StatusConflict, "no_index"
	case errors.Is(err, core.ErrScanRunning):
		status, code = http.StatusConflict, "scan_running"
	case errors.Is(err, core.ErrNoScan):
		status, code = http.StatusConflict, "no_scan"
	case errors.Is(err, core.ErrGone):
		status, code = http.StatusConflict, "gone"
	case errors.Is(err, core.ErrIndexChanged):
		status, code = http.StatusConflict, "index_changed"
	case errors.Is(err, core.ErrClosed):
		status, code = http.StatusServiceUnavailable, "shutting_down"
	case errors.Is(err, errIndexRequired):
		status, code = http.StatusBadRequest, "index_required"
	case errors.Is(err, core.ErrNoShare):
		status, code = http.StatusBadRequest, "not_in_share"
	case errors.Is(err, core.ErrNotFile):
		status, code = http.StatusBadRequest, "not_a_file"
	case errors.As(err, &invalid), errors.As(err, &input), errors.As(err, &query), errors.As(err, new(badRequest)):
		status, code = http.StatusBadRequest, "bad_request"
	case errors.As(err, &tooBig):
		status, code = http.StatusRequestEntityTooLarge, "too_large"
	}
	if status == http.StatusInternalServerError {
		log.Printf("internal error: %v", err)
		writeError(w, status, code, "internal error; see the server log")
		return
	}
	writeError(w, status, code, err.Error())
}

type badRequest string

func (e badRequest) Error() string { return string(e) }

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

// bodyTimeout is how long a request has to send its body once its headers
// are in. The bodies here are small JSON; a client that trickles one in is
// holding a connection and a goroutine for nothing. The header timeout is
// set on the server; this one is set per request, so that a long answer such
// as a film streaming out is not cut short by a limit meant for reading.
var bodyTimeout = 10 * time.Second

var errBodyTimeout = errors.New("the request body did not arrive in time")

func decode(w http.ResponseWriter, r *http.Request, v any) error {
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		return badRequest("the request body must be application/json")
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(bodyTimeout))
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	err := dec.Decode(v)
	if errors.Is(err, os.ErrDeadlineExceeded) {
		// The deadline stays. Before answering, the server tries to read
		// the rest of the body, and that must give up at once too; the
		// connection is then closed.
		return errBodyTimeout
	}
	// Read in time: the deadline goes, so that the answer can take as long
	// as it needs.
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return err
		}
		return badRequest("malformed JSON: " + err.Error())
	}
	return nil
}
