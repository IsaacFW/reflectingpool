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
