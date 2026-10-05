package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/appdb"
	"github.com/IsaacFW/reflectingpool/internal/auth"
	"github.com/IsaacFW/reflectingpool/internal/core"
)

const password = "correct horse battery"

type harness struct {
	t      *testing.T
	url    string
	client *http.Client
	csrf   string
	index  string // the index the server last said it was using, sent back as a real client would
	code   string
	root   string
	app    *core.App
}

func newHarness(t *testing.T, opts Options, readOnly bool) *harness {
	t.Helper()
	root := t.TempDir()
	files := map[string]string{
		"media/big.mkv":    strings.Repeat("v", 5000),
		"media/small.mkv":  strings.Repeat("v", 100),
		"media/song.flac":  strings.Repeat("a", 900),
		"docs/notes.txt":   "0123456789",
		"docs/page.html":   "<script>alert(1)</script>",
		"docs/drawing.svg": "<svg xmlns='http://www.w3.org/2000/svg'><script>alert(1)</script></svg>",
	}
	for path, content := range files {
		full := filepath.Join(root, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	data := t.TempDir()
	db, err := appdb.Open(filepath.Join(data, "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	app, err := core.New(context.Background(), core.Config{Roots: []string{root}, DataDir: data, ReadOnly: readOnly}, db)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, root: app.Config().Roots[0], app: app}
	svc := auth.New(db, auth.Params{Time: 1, MemoryKiB: 1024, Threads: 1})
	svc.OnSetupCode = func(code string) { h.code = code }
	ts := httptest.NewServer(New(app, svc, opts).Handler())
	t.Cleanup(func() { ts.Close(); app.Close(); db.Close() })
	jar, _ := cookiejar.New(nil)
	h.url, h.client = ts.URL, &http.Client{Jar: jar}
	return h
}

type response struct {
	status int
	header http.Header
	raw    []byte
	body   map[string]any
}

func (h *harness) request(method, path string, body any, header map[string]string) response {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		switch b := body.(type) {
		case string:
			rd = strings.NewReader(b)
		default:
			enc, _ := json.Marshal(b)
			rd = bytes.NewReader(enc)
		}
	}
	req, err := http.NewRequest(method, h.url+path, rd)
	if err != nil {
		h.t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if h.csrf != "" {
		req.Header.Set("X-CSRF-Token", h.csrf)
	}
	if h.index != "" {
		req.Header.Set("X-RP-Index", h.index)
	}
	for k, v := range header {
		if v == "" {
			req.Header.Del(k)
		} else {
			req.Header.Set(k, v)
		}
	}
	res, err := h.client.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer res.Body.Close()
	out := response{status: res.StatusCode, header: res.Header}
	if v := res.Header.Get("X-RP-Index"); v != "" {
		h.index = v
	}
	out.raw, _ = io.ReadAll(res.Body)
	json.Unmarshal(out.raw, &out.body)
	return out
}

func (h *harness) expect(want int, r response) response {
	h.t.Helper()
	if r.status != want {
		h.t.Fatalf("status = %d, want %d; body: %s", r.status, want, r.raw)
	}
	return r
}

// signIn runs first-time setup, logs in and waits for a scan.
func (h *harness) signIn() {
	h.t.Helper()
	h.expect(200, h.request("GET", "/api/health", nil, nil))
	h.expect(200, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "isaac", "password": password}, nil))
	r := h.expect(200, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, nil))
	h.csrf = r.body["csrf"].(string)
	h.expect(202, h.request("POST", "/api/scan", nil, nil))
	for range 200 {
		st := h.expect(200, h.request("GET", "/api/scan", nil, nil))
		if st.body["running"] == false && st.body["index"] != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatal("scan did not finish")
}

// id finds an entry by name through the API.
func (h *harness) id(name string) string {
	h.t.Helper()
	r := h.expect(200, h.request("GET", "/api/entries?name="+name, nil, nil))
	for _, it := range r.body["items"].([]any) {
		if m := it.(map[string]any); m["name"] == name {
			return jsonNum(m["id"])
		}
	}
	h.t.Fatalf("no entry named %s", name)
	return ""
}

func jsonNum(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func TestSetupLoginAndSessionRules(t *testing.T) {
	h := newHarness(t, Options{}, false)

	// Nothing but health, setup and login answers without a session.
	for _, path := range []string{"/api/tree", "/api/entries", "/api/scan", "/api/storage", "/api/session", "/api/entries/1/content"} {
		h.expect(401, h.request("GET", path, nil, nil))
	}
	r := h.expect(200, h.request("GET", "/api/health", nil, nil))
	if r.body["setup_required"] != true || h.code == "" {
		t.Fatalf("health = %s, code = %q", r.raw, h.code)
	}
	if strings.Contains(string(r.raw), h.code) {
		t.Fatal("the setup code must never be served over HTTP")
	}
	for _, name := range []string{"X-Content-Type-Options", "X-Frame-Options", "Content-Security-Policy", "Referrer-Policy"} {
		if r.header.Get(name) == "" {
			t.Errorf("missing %s header", name)
		}
	}

	h.expect(403, h.request("POST", "/api/setup", map[string]string{"code": "WRONG", "username": "isaac", "password": password}, nil))
	h.expect(400, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "isaac", "password": "short"}, nil))
	h.expect(200, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "isaac", "password": password}, nil))
	h.expect(409, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "eve", "password": password}, nil))

	h.expect(401, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": "wrong password!"}, nil))
	// A form post, as a hostile page could make a browser send, is refused.
	h.expect(400, h.request("POST", "/api/login", "username=isaac&password="+password, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}))
	h.expect(403, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, map[string]string{"Origin": "https://evil.example"}))

	r = h.expect(200, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, nil))
	cookie := r.header.Get("Set-Cookie")
	for _, attr := range []string{"rp_session=", "HttpOnly", "SameSite=Strict", "Path=/"} {
		if !strings.Contains(cookie, attr) {
			t.Errorf("session cookie %q lacks %s", cookie, attr)
		}
	}
	csrf, _ := r.body["csrf"].(string)
	if csrf == "" || strings.Contains(cookie, csrf) {
		t.Fatalf("csrf token missing or equal to the cookie: %s", r.raw)
	}

	// The cookie alone reads but does not change anything.
	h.expect(200, h.request("GET", "/api/session", nil, nil))
	h.expect(403, h.request("POST", "/api/scan", nil, nil))
	h.expect(403, h.request("POST", "/api/scan", nil, map[string]string{"X-CSRF-Token": "guess"}))
	h.csrf = csrf
	h.expect(403, h.request("POST", "/api/scan", nil, map[string]string{"Origin": "https://evil.example"}))
	h.expect(202, h.request("POST", "/api/scan", nil, nil))

	h.expect(200, h.request("POST", "/api/logout", nil, nil))
	h.expect(401, h.request("GET", "/api/session", nil, nil))
}

func TestLoginLockoutOverHTTP(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.request("GET", "/api/health", nil, nil)
	h.expect(200, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "isaac", "password": password}, nil))
	for range 5 {
		h.expect(401, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": "wrong password!"}, nil))
	}
	r := h.expect(429, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, nil))
	if r.header.Get("Retry-After") == "" {
		t.Error("no Retry-After header on lockout")
	}
	// Without TrustProxy a forged forwarding header does not buy a fresh start.
	h.expect(429, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, map[string]string{"X-Forwarded-For": "203.0.113.9"}))
}

func TestSecureCookieBehindTLS(t *testing.T) {
	h := newHarness(t, Options{TLS: true}, false)
	h.request("GET", "/api/health", nil, nil)
	h.expect(200, h.request("POST", "/api/setup", map[string]string{"code": h.code, "username": "isaac", "password": password}, nil))
	r := h.expect(200, h.request("POST", "/api/login", map[string]string{"username": "isaac", "password": password}, nil))
	if cookie := r.header.Get("Set-Cookie"); !strings.HasPrefix(cookie, "__Host-rp_session=") || !strings.Contains(cookie, "Secure") {
		t.Errorf("cookie over TLS = %q", cookie)
	}
}

func TestBrowseAnnotateAndReview(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()

	r := h.expect(200, h.request("GET", "/api/tree", nil, nil))
	roots := r.body["children"].([]any)
	if len(roots) != 1 || roots[0].(map[string]any)["name"] != h.root {
		t.Fatalf("roots = %s", r.raw)
	}
	rootID := jsonNum(roots[0].(map[string]any)["id"])
	r = h.expect(200, h.request("GET", "/api/tree?id="+rootID+"&sort=size&desc=1", nil, nil))
	kids := r.body["children"].([]any)
	if len(kids) != 2 || kids[0].(map[string]any)["name"] != "media" || kids[0].(map[string]any)["size"] != float64(6000) {
		t.Fatalf("children = %s", r.raw)
	}
	if r.body["entry"].(map[string]any)["path"] != h.root {
		t.Errorf("entry path = %v", r.body["entry"])
	}

	r = h.expect(200, h.request("GET", "/api/entries?kind=file&type=video&sort=size&desc=1", nil, nil))
	items := r.body["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["path"] != h.root+"/media/big.mkv" {
		t.Fatalf("videos = %s", r.raw)
	}
	// Folders only, for the folder tree: the root holds two shares and no files, a share holds no folders.
	if r := h.expect(200, h.request("GET", "/api/tree?kind=dir&id="+rootID, nil, nil)); r.body["total"] != float64(2) || len(r.body["children"].([]any)) != 2 {
		t.Errorf("folders in the root = %s", r.raw)
	}
	if r := h.expect(200, h.request("GET", "/api/tree?kind=dir&id="+h.id("media"), nil, nil)); r.body["total"] != float64(0) {
		t.Errorf("folders in media = %s", r.raw)
	}
	h.expect(400, h.request("GET", "/api/entries?sort=bogus", nil, nil))
	h.expect(400, h.request("GET", "/api/entries?limit=-1", nil, nil))
	h.expect(404, h.request("GET", "/api/entries/999999", nil, nil))
	h.expect(404, h.request("GET", "/api/entries/abc", nil, nil))

	h.expect(200, h.request("PUT", "/api/prefixes", map[string]any{"prefixes": []map[string]string{{"name": "KEEP", "meaning": "Do not delete"}}}, nil))
	big := h.id("big.mkv")
	h.expect(400, h.request("PUT", "/api/entries/"+big+"/annotation", map[string]any{"note": "x", "prefixes": []string{"NOPE"}}, nil))
	h.expect(400, h.request("PUT", "/api/entries/"+big+"/annotation", map[string]any{"note": "x", "surprise": true}, nil))
	h.expect(200, h.request("PUT", "/api/entries/"+big+"/annotation", map[string]any{"note": "Wedding video", "prefixes": []string{"KEEP"}}, nil))

	r = h.expect(200, h.request("GET", "/api/entries/"+big, nil, nil))
	if an := r.body["annotation"].(map[string]any); an["note"] != "Wedding video" || an["path"] != "big.mkv" {
		t.Errorf("annotation = %s", r.raw)
	}
	if _, err := os.Stat(filepath.Join(h.root, "media/.reflection/annotations.jsonl")); err != nil {
		t.Errorf("annotation file not written into the share: %v", err)
	}

	// The review queue: share by share, then file type, largest first.
	spec := map[string]any{"filter": map[string]any{"kind": "file"}, "groups": []string{"share", "type"}, "order": "size"}
	r = h.expect(200, h.request("POST", "/api/queue", spec, nil))
	group := r.body["groups"].([]any)[0].(map[string]any)
	next := r.body["items"].([]any)
	if group["remaining"] != float64(1) || group["total"] != float64(2) || len(next) != 1 || next[0].(map[string]any)["name"] != "small.mkv" {
		t.Fatalf("queue after annotating big.mkv = %s", r.raw)
	}
	h.expect(200, h.request("PUT", "/api/entries/"+h.id("small.mkv")+"/annotation", map[string]any{"skipped": true}, nil))
	r = h.expect(200, h.request("POST", "/api/queue", spec, nil))
	if next := r.body["items"].([]any); len(next) != 1 || next[0].(map[string]any)["name"] != "song.flac" {
		t.Fatalf("queue after skipping small.mkv = %s", r.raw)
	}
	h.expect(400, h.request("POST", "/api/queue", map[string]any{"groups": []string{"bogus"}}, nil))

	r = h.expect(200, h.request("GET", "/api/shares", nil, nil))
	for _, sh := range r.body["shares"].([]any) {
		if m := sh.(map[string]any); m["name"] == "media" && (m["annotated"] != float64(1) || m["skipped"] != float64(1) || m["covered_bytes"] != float64(5000)) {
			t.Errorf("media summary = %v", m)
		}
	}

	h.expect(200, h.request("DELETE", "/api/entries/"+big+"/annotation", nil, nil))
	r = h.expect(200, h.request("GET", "/api/entries/"+big, nil, nil))
	if r.body["annotation"] != nil {
		t.Errorf("annotation survived delete: %s", r.raw)
	}

	r = h.expect(200, h.request("GET", "/api/storage", nil, nil))
	if len(r.body["datasets"].([]any)) == 0 || r.body["zfs_origin"] == nil {
		t.Errorf("storage = %s", r.raw)
	}
}

// Entry IDs are reassigned by every scan. A client still holding IDs from the
// previous index must be refused, not allowed to act on whatever item now
// has that number.
func TestStaleEntryIDsAreRefused(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()
	first := h.index
	id := h.id("big.mkv")
	if first == "" {
		t.Fatal("responses do not name the index in use")
	}

	// A second scan replaces the index while the client still holds the ID.
	h.expect(202, h.request("POST", "/api/scan", nil, nil))
	for range 300 {
		h.request("GET", "/api/scan", nil, nil)
		if h.index != first {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if h.index == first {
		t.Fatal("the index ID did not change after a scan")
	}
	stale := map[string]string{"X-RP-Index": first}

	r := h.expect(409, h.request("PUT", "/api/entries/"+id+"/annotation", map[string]any{"note": "for the old big.mkv"}, stale))
	if r.body["code"] != "index_changed" {
		t.Errorf("stale write: %s", r.raw)
	}
	for _, path := range []string{"/api/entries/" + id, "/api/entries/" + id + "/content", "/api/tree?id=" + id, "/api/entries?share=" + id} {
		if r := h.request("GET", path, nil, stale); r.status != 409 || r.body["code"] != "index_changed" {
			t.Errorf("stale read of %s: %d %s", path, r.status, r.raw)
		}
	}
	h.expect(409, h.request("POST", "/api/queue", map[string]any{"filter": map[string]any{"kind": "file"}}, stale))
	// A write that does not say which index it means is refused outright.
	r = h.expect(400, h.request("PUT", "/api/entries/"+id+"/annotation", map[string]any{"note": "x"}, map[string]string{"X-RP-Index": ""}))
	if r.body["code"] != "index_required" {
		t.Errorf("write without an index: %s", r.raw)
	}
	if _, err := os.Stat(filepath.Join(h.root, "media/.reflection")); !os.IsNotExist(err) {
		t.Fatal("a refused write still created annotation files")
	}

	// The client recovers by path, which outlives the scan, even while it
	// still names the old index.
	r = h.expect(200, h.request("GET", "/api/entries/lookup?path="+h.root+"/media/big.mkv", nil, stale))
	found := r.body["entry"].(map[string]any)
	if found["name"] != "big.mkv" {
		t.Fatalf("lookup = %s", r.raw)
	}
	h.expect(404, h.request("GET", "/api/entries/lookup?path="+h.root+"/media/nope", nil, nil))
	h.expect(200, h.request("PUT", "/api/entries/"+jsonNum(found["id"])+"/annotation", map[string]any{"note": "for big.mkv"}, nil))
}

func TestErrorCodesTotalsAndBlankSaves(t *testing.T) {
	h := newHarness(t, Options{}, false)
	if r := h.request("GET", "/api/tree", nil, nil); r.status != 401 || r.body["code"] != "not_signed_in" {
		t.Errorf("signed out: %d %s", r.status, r.raw)
	}
	h.signIn()
	for path, want := range map[string]string{"/api/entries/999999": "not_found", "/api/entries?sort=bogus": "bad_request", "/api/entries/lookup": "bad_request"} {
		if r := h.request("GET", path, nil, nil); r.body["code"] != want {
			t.Errorf("%s: code %v, want %s", path, r.body["code"], want)
		}
	}
	if r := h.request("DELETE", "/api/scan", nil, nil); r.status != 409 || r.body["code"] != "no_scan" {
		t.Errorf("stopping when nothing runs: %d %s", r.status, r.raw)
	}

	r := h.expect(200, h.request("GET", "/api/entries?kind=file&type=video&limit=1", nil, nil))
	if len(r.body["items"].([]any)) != 1 || r.body["total"] != float64(2) || r.body["total_size"] != float64(5100) {
		t.Errorf("totals must cover the whole filter, not the page: %s", r.raw)
	}
	r = h.expect(200, h.request("GET", "/api/entries?kind=dir", nil, nil))
	if r.body["total"] != float64(3) || r.body["total_size"] != nil { // the root and its two shares
		t.Errorf("folder sizes include their contents and must not be summed: %s", r.raw)
	}

	// Saving a blank form is how an item is skipped.
	r = h.expect(200, h.request("PUT", "/api/entries/"+h.id("song.flac")+"/annotation", map[string]any{}, nil))
	if r.body["annotation"].(map[string]any)["skipped"] != true {
		t.Errorf("a blank save was not stored as a skip: %s", r.raw)
	}
	// An item that has left the disk can still be skipped, so it does not
	// block the queue; it cannot be described.
	gone := h.id("small.mkv")
	os.Remove(filepath.Join(h.root, "media/small.mkv"))
	if r := h.request("PUT", "/api/entries/"+gone+"/annotation", map[string]any{"note": "x"}, nil); r.status != 409 || r.body["code"] != "gone" {
		t.Errorf("describing a missing item: %d %s", r.status, r.raw)
	}
	h.expect(200, h.request("PUT", "/api/entries/"+gone+"/annotation", map[string]any{}, nil))
}

func TestTheInterfaceIsServed(t *testing.T) {
	h := newHarness(t, Options{}, false)

	// Every screen is the same page, served under a policy with nothing
	// inline, and needs no session: it is where signing in happens.
	var etag string
	for _, path := range []string{"/", "/space?path=/pool/media", "/settings"} {
		r := h.expect(200, h.request("GET", path, nil, nil))
		csp := r.header.Get("Content-Security-Policy")
		if !strings.Contains(csp, "script-src 'self'") || !strings.Contains(csp, "style-src 'self'") || strings.Contains(csp, "unsafe") {
			t.Errorf("%s: policy = %q", path, csp)
		}
		if !strings.HasPrefix(r.header.Get("Content-Type"), "text/html") || !strings.Contains(string(r.raw), `src="/app/main.js"`) {
			t.Errorf("%s is not the page: %s %.80s", path, r.header.Get("Content-Type"), r.raw)
		}
		if r.header.Get("X-Frame-Options") != "DENY" || r.header.Get("Cache-Control") != "no-cache" {
			t.Errorf("%s: headers %v", path, r.header)
		}
		etag = r.header.Get("ETag")
	}
	// A copy the browser already holds is confirmed, not sent again.
	if r := h.request("GET", "/", nil, map[string]string{"If-None-Match": etag}); r.status != 304 || len(r.raw) != 0 {
		t.Errorf("revalidation: %d with %d bytes", r.status, len(r.raw))
	}

	for path, ctype := range map[string]string{"/app/main.js": "text/javascript", "/lib/preact.js": "text/javascript", "/style.css": "text/css", "/app/icon.svg": "image/svg+xml"} {
		r := h.expect(200, h.request("GET", path, nil, nil))
		if !strings.HasPrefix(r.header.Get("Content-Type"), ctype) || r.header.Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s: %s", path, r.header.Get("Content-Type"))
		}
		// Only the page itself may load anything.
		if got := r.header.Get("Content-Security-Policy"); !strings.HasPrefix(got, "default-src 'none'") {
			t.Errorf("%s: policy = %q", path, got)
		}
	}
	// What is not part of the interface is not served, wherever it sits.
	for _, path := range []string{"/nope", "/app/nope.js", "/lib/update.sh", "/lib/CHECKSUMS", "/lib/LICENSE-preact", "/embed.go", "/app/%2e%2e/embed.go", "/api/nope"} {
		if r := h.request("GET", path, nil, nil); r.status != 404 {
			t.Errorf("%s: %d, want 404", path, r.status)
		}
	}

	// A folder on disk can stand in for the built-in files while working on them.
	dev := newHarness(t, Options{Web: fstest.MapFS{"index.html": {Data: []byte("<!doctype html><title>dev</title>")}}}, false)
	if r := dev.expect(200, dev.request("GET", "/", nil, nil)); !strings.Contains(string(r.raw), "<title>dev</title>") {
		t.Errorf("development files not served: %s", r.raw)
	}

	// The first-run screen names what will be scanned.
	h.signIn()
	r := h.expect(200, h.request("GET", "/api/session", nil, nil))
	if roots, _ := r.body["roots"].([]any); len(roots) != 1 || roots[0] != h.root {
		t.Errorf("session roots = %v", r.body["roots"])
	}
}

func TestQueueToolsOverHTTP(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()
	media := h.id("media")

	// Everything under one folder, for stepping from the space view into a
	// listing or a review of what is in it.
	r := h.expect(200, h.request("GET", "/api/entries?kind=file&sort=age&under="+media, nil, nil))
	if r.body["total"] != float64(3) || r.body["total_size"] != float64(6000) {
		t.Errorf("under media = %s", r.raw)
	}
	h.expect(200, h.request("GET", "/api/entries?sort=created&desc=1", nil, nil))

	spec := map[string]any{
		"filter": map[string]any{"kind": "file"}, "groups": []string{"share", "age"},
		"order": "age", "exclude_covered": true, "limit": 1, "offset": 1,
	}
	r = h.expect(200, h.request("POST", "/api/queue", spec, nil))
	group := r.body["groups"].([]any)[0].(map[string]any)
	values := group["values"].([]any)
	share, age := values[0].(map[string]any), values[1].(map[string]any)
	if r.body["total"] != float64(6) || r.body["group_total"] != float64(2) || r.body["group_position"] != float64(1) ||
		r.body["remaining"] != float64(6) || r.body["total_size"] == nil || len(r.body["items"].([]any)) != 1 {
		t.Fatalf("queue = %s", r.raw)
	}
	if share["label"] != "media" || jsonNum(share["value"]) != media || age["value"] != "under-6m" || age["label"] != "Under 6 months" {
		t.Errorf("group values = %v", values)
	}

	// Skip the rest of the group, named by the values the queue gave for it.
	spec["group"] = []any{share["value"], age["value"]}
	h.expect(400, h.request("POST", "/api/queue", spec, nil)) // only the skip takes a group
	h.expect(400, h.request("POST", "/api/queue/skip", spec, map[string]string{"X-RP-Index": ""}))
	if r := h.request("POST", "/api/queue/skip", spec, map[string]string{"X-RP-Index": "scan-of-another-day"}); r.status != 409 || r.body["code"] != "index_changed" {
		t.Errorf("skip against another index: %d %s", r.status, r.raw)
	}
	r = h.expect(200, h.request("POST", "/api/queue/skip", spec, nil))
	if r.body["skipped"] != float64(3) {
		t.Errorf("skip = %s", r.raw)
	}
	list, err := os.ReadFile(filepath.Join(h.root, "media/.reflection/skipped.jsonl"))
	if err != nil || strings.Count(string(list), "\n") != 3 {
		t.Errorf("skipped list in the share: %q, %v", list, err)
	}
	delete(spec, "group")
	spec["offset"] = 0
	r = h.expect(200, h.request("POST", "/api/queue", spec, nil))
	if r.body["remaining"] != float64(3) || r.body["group_position"] != float64(2) || r.body["group_count"] != float64(1) {
		t.Errorf("queue after the skip = %s", r.raw)
	}
	spec["group"] = []any{"media", "under-6m"}
	if r := h.request("POST", "/api/queue/skip", spec, nil); r.status != 400 || r.body["code"] != "bad_request" {
		t.Errorf("a group named by its label: %d %s", r.status, r.raw)
	}

	ro := newHarness(t, Options{}, true)
	ro.signIn()
	spec["group"] = []any{json.Number(ro.id("media")), "under-6m"}
	if r := ro.request("POST", "/api/queue/skip", spec, nil); r.status != 403 || r.body["code"] != "read_only" {
		t.Errorf("read-only skip: %d %s", r.status, r.raw)
	}
}

func TestPrefixCountsAndMissingNotes(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()
	h.expect(200, h.request("PUT", "/api/prefixes", map[string]any{"prefixes": []map[string]string{{"name": "KEEP", "meaning": "Do not delete"}, {"name": "OLD", "meaning": ""}}}, nil))
	h.expect(200, h.request("PUT", "/api/entries/"+h.id("big.mkv")+"/annotation", map[string]any{"prefixes": []string{"KEEP"}}, nil))
	h.expect(200, h.request("PUT", "/api/entries/"+h.id("small.mkv")+"/annotation", map[string]any{"note": "about to vanish", "prefixes": []string{"KEEP"}}, nil))
	r := h.expect(200, h.request("GET", "/api/prefixes", nil, nil))
	list := r.body["prefixes"].([]any)
	if list[0].(map[string]any)["count"] != float64(2) || list[1].(map[string]any)["count"] != float64(0) {
		t.Errorf("prefix counts = %s", r.raw)
	}

	// An item deleted from the disk leaves its note behind after the next
	// scan. It can then only be removed through the share.
	media := h.id("media")
	os.Remove(filepath.Join(h.root, "media/small.mkv"))
	h.expect(202, h.request("POST", "/api/scan", nil, nil))
	for range 200 {
		if st := h.expect(200, h.request("GET", "/api/scan", nil, nil)); st.body["running"] == false {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	media = h.id("media") // the scan numbered everything afresh
	path := "/api/shares/" + media + "/annotations?path="
	if r := h.request("DELETE", path+"big.mkv", nil, nil); r.status != 400 {
		t.Errorf("deleting the note of an item that exists through the share: %d %s", r.status, r.raw)
	}
	h.expect(404, h.request("DELETE", path+"nothing.mkv", nil, nil))
	h.expect(400, h.request("DELETE", "/api/shares/"+media+"/annotations", nil, nil))
	h.expect(200, h.request("DELETE", path+"small.mkv", nil, nil))
	r = h.expect(200, h.request("GET", "/api/shares/"+media+"/annotations", nil, nil))
	if all := r.body["annotations"].([]any); len(all) != 1 || all[0].(map[string]any)["path"] != "big.mkv" {
		t.Errorf("notes left = %s", r.raw)
	}

	ro := newHarness(t, Options{}, true)
	ro.signIn()
	if r := ro.request("DELETE", "/api/shares/"+ro.id("media")+"/annotations?path=x", nil, nil); r.status != 403 {
		t.Errorf("read-only delete: %d", r.status)
	}
}

func TestSettingsAndSchedule(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()

	r := h.expect(200, h.request("GET", "/api/settings", nil, nil))
	if sched := r.body["scan_schedule"].(map[string]any); sched["enabled"] != false {
		t.Errorf("a schedule must be off until the user sets one: %s", r.raw)
	}
	st := h.expect(200, h.request("GET", "/api/scan", nil, nil))
	if st.body["next_scheduled"] != nil || st.body["last_choice"] != "aggressive" || len(st.body["history"].([]any)) != 1 {
		t.Errorf("status = %s", st.raw)
	}
	if rec := st.body["history"].([]any)[0].(map[string]any); rec["trigger"] != "manual" || rec["intensity"] != "aggressive" || rec["index_id"] != h.index {
		t.Errorf("history = %v", rec)
	}

	h.expect(400, h.request("PUT", "/api/settings", map[string]any{"scan_schedule": map[string]any{"enabled": true, "time": "25:99", "intensity": "low"}}, nil))
	h.expect(400, h.request("PUT", "/api/settings", map[string]any{"scan_schedule": map[string]any{"enabled": true, "time": "03:00", "intensity": "turbo"}}, nil))
	h.expect(200, h.request("PUT", "/api/settings", map[string]any{"scan_schedule": map[string]any{"enabled": true, "time": "03:00", "intensity": "balanced"}}, nil))
	st = h.expect(200, h.request("GET", "/api/scan", nil, nil))
	if st.body["next_scheduled"] == nil || st.body["schedule"].(map[string]any)["intensity"] != "balanced" {
		t.Errorf("status after enabling the schedule = %s", st.raw)
	}
}

func TestScanIntensityOverHTTP(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn() // leaves an index built by an aggressive scan

	h.expect(400, h.request("POST", "/api/scan", map[string]string{"intensity": "turbo"}, nil))
	r := h.expect(202, h.request("POST", "/api/scan", map[string]string{"intensity": "low"}, nil))
	if r.body["intensity"] != "low" {
		t.Errorf("scan start = %s", r.raw)
	}
	for range 300 {
		st := h.expect(200, h.request("GET", "/api/scan", nil, nil))
		if index, _ := st.body["index"].(map[string]any); st.body["running"] == false && index["intensity"] == "low" {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the low-impact scan did not finish, or the index does not record its intensity")
}

func TestContentIsServedSafely(t *testing.T) {
	h := newHarness(t, Options{}, false)
	h.signIn()

	r := h.expect(200, h.request("GET", "/api/entries/"+h.id("notes.txt")+"/content", nil, nil))
	if string(r.raw) != "0123456789" || !strings.HasPrefix(r.header.Get("Content-Type"), "text/plain") {
		t.Errorf("text: %q as %s", r.raw, r.header.Get("Content-Type"))
	}
	r = h.expect(206, h.request("GET", "/api/entries/"+h.id("notes.txt")+"/content", nil, map[string]string{"Range": "bytes=2-4"}))
	if string(r.raw) != "234" {
		t.Errorf("range request returned %q", r.raw)
	}

	// A hostile HTML file on a share must never run as a page of ours.
	r = h.expect(200, h.request("GET", "/api/entries/"+h.id("page.html")+"/content", nil, nil))
	if ct := r.header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("HTML served as %s", ct)
	}
	if r.header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("content served without nosniff")
	}
	r = h.expect(200, h.request("GET", "/api/entries/"+h.id("drawing.svg")+"/content", nil, nil))
	if !strings.Contains(r.header.Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("SVG served without a sandbox: %q", r.header.Get("Content-Security-Policy"))
	}
	r = h.expect(200, h.request("GET", "/api/entries/"+h.id("big.mkv")+"/content", nil, map[string]string{"Range": ""}))
	if r.header.Get("Content-Type") != "video/x-matroska" || r.header.Get("Accept-Ranges") != "bytes" {
		t.Errorf("video headers = %v", r.header)
	}

	h.expect(400, h.request("GET", "/api/entries/"+h.id("media")+"/content", nil, nil))
}

func TestReadOnlyOverHTTP(t *testing.T) {
	h := newHarness(t, Options{}, true)
	h.signIn()
	r := h.expect(200, h.request("GET", "/api/session", nil, nil))
	if r.body["read_only"] != true {
		t.Errorf("session = %s", r.raw)
	}
	h.expect(403, h.request("PUT", "/api/entries/"+h.id("big.mkv")+"/annotation", map[string]any{"note": "x"}, nil))
	if _, err := os.Stat(filepath.Join(h.root, "media/.reflection")); !os.IsNotExist(err) {
		t.Error("read-only mode created a metadata folder")
	}
}

func TestSelfSignedCert(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	now := time.Now()
	cert, fp, err := SelfSignedCert(dir, now)
	if err != nil || len(cert.Certificate) != 1 || len(fp) != 95 {
		t.Fatalf("create: %q, %v", fp, err)
	}
	if st, err := os.Stat(filepath.Join(dir, "key.pem")); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("private key mode: %v, %v", st.Mode().Perm(), err)
	}
	if _, again, _ := SelfSignedCert(dir, now.Add(24*time.Hour)); again != fp {
		t.Error("certificate was regenerated while still valid")
	}
	if _, renewed, err := SelfSignedCert(dir, now.Add(certLifetime-24*time.Hour)); err != nil || renewed == fp {
		t.Errorf("certificate not renewed near expiry: %v", err)
	}
}
