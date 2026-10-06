package server

import (
	"context"
	"mime"
	"net/http"
	"os"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/auth"
	"github.com/IsaacFW/reflectingpool/internal/core"
	"github.com/IsaacFW/reflectingpool/internal/index"
	"github.com/IsaacFW/reflectingpool/internal/meta"
	"github.com/IsaacFW/reflectingpool/internal/scan"
)

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	required, err := s.auth.SetupRequired()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "setup_required": required, "version": s.opts.Version})
}

func (s *Server) setup(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Code     string `json:"code"`
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.auth.Setup(s.clientAddr(r), req.Code, req.Username, req.Password); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decode(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	token, sess, err := s.auth.Login(r.Context(), s.clientAddr(r), req.Username, req.Password)
	if err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(r), Value: token, Path: "/",
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteStrictMode,
		MaxAge: int((30 * 24 * time.Hour).Seconds()),
	})
	s.session(w, r, sess)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := s.auth.Logout(s.token(r)); err != nil {
		s.fail(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: s.cookieName(r), Value: "", Path: "/",
		HttpOnly: true, Secure: s.secure(r), SameSite: http.SameSiteStrictMode, MaxAge: -1,
	})
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) session(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	writeJSON(w, http.StatusOK, map[string]any{
		"user": sess.User, "csrf": sess.CSRF, "read_only": s.app.Config().ReadOnly, "version": s.opts.Version,
		"roots": s.app.Config().Roots,
	})
}

func (s *Server) scanStatus(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	writeJSON(w, http.StatusOK, s.app.ScanStatus())
}

// scanStart begins a scan. The body is optional: {"intensity": "aggressive" |
// "balanced" | "low"} chooses how hard the scan leans on the disks, and no
// body means aggressive.
func (s *Server) scanStart(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	intensity := scan.Aggressive
	if r.ContentLength != 0 {
		var req struct {
			Intensity string `json:"intensity"`
		}
		if err := decode(w, r, &req); err != nil {
			s.fail(w, err)
			return
		}
		if req.Intensity != "" {
			var err error
			if intensity, err = scan.ParseIntensity(req.Intensity); err != nil {
				s.fail(w, badRequest(err.Error()))
				return
			}
		}
	}
	if err := s.app.StartScan(intensity); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "intensity": intensity.String()})
}

func (s *Server) storage(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	rep, err := s.app.Storage(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, rep)
}

func (s *Server) shares(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	shares, err := s.app.Shares(r.Context())
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shares": shares})
}

func (s *Server) shareAnnotations(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	all, err := s.app.ShareAnnotations(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if all == nil {
		all = []meta.Annotation{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"annotations": all})
}

// tree lists one directory's children for the space breakdown, or only the
// folders among them with kind=dir. With no id it
// lists the scan roots.
func (s *Server) tree(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := &query{v: r.URL.Query()}
	id := q.int("id", 0)
	limit, offset := q.page(500, 5000)
	if q.err != nil {
		s.fail(w, q.err)
		return
	}
	ctx := r.Context()
	resp := map[string]any{}
	err := s.app.View(func(ix *index.Index) error {
		if id != 0 {
			entry, err := ix.Entry(ctx, id)
			if err != nil {
				return err
			}
			if entry.Path, err = ix.EntryPath(ctx, id); err != nil {
				return err
			}
			one := []index.Row{entry}
			if err := ix.FillTypes(ctx, one); err != nil {
				return err
			}
			resp["entry"] = one[0]
		}
		list := ix.Children
		if q.v.Get("kind") == "dir" { // folders only, for the folder tree
			list = ix.ChildDirs
		}
		children, total, err := list(ctx, id, q.sort(), limit, offset)
		if err != nil {
			return err
		}
		resp["children"], resp["total"] = children, total
		return ix.FillTypes(ctx, children)
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// entries is the discovery listing: everything in the index that matches a
// filter, largest first unless asked otherwise.
func (s *Server) entries(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	q := &query{v: r.URL.Query()}
	f := index.Filter{
		Kind: q.v.Get("kind"), Under: q.int("under", 0), Shares: q.ids("share"), Types: q.list("type"), Exts: q.list("ext"),
		MinSize: q.int("min_size", 0), MaxSize: q.int("max_size", 0), ModifiedBefore: q.int("modified_before", 0),
		Name: q.v.Get("name"), State: q.v.Get("state"), Prefix: q.v.Get("prefix"),
	}
	limit, offset := q.page(100, 1000)
	if q.err != nil {
		s.fail(w, q.err)
		return
	}
	ctx := r.Context()
	var rows []index.Row
	var totals index.Totals
	err := s.app.View(func(ix *index.Index) error {
		var err error
		if rows, err = ix.Find(ctx, f, q.sort(), limit, offset); err != nil {
			return err
		}
		if err = ix.FillTypes(ctx, rows); err != nil {
			return err
		}
		if totals, err = ix.Count(ctx, f); err != nil {
			return err
		}
		return ix.FillPaths(ctx, rows)
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"items": rows, "total": totals.Count, "total_size": totals.Size, "total_disk": totals.Disk,
	})
}

// lookup returns the entry at a path. Paths outlive scans and entry IDs do
// not, so this is how a client finds an item again after the index changes.
func (s *Server) lookup(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	path := r.URL.Query().Get("path")
	if path == "" {
		s.fail(w, badRequest("give the item's full path in the path parameter"))
		return
	}
	ctx := r.Context()
	var entry index.Row
	err := s.app.View(func(ix *index.Index) error {
		var err error
		if entry, err = ix.Lookup(ctx, path); err != nil {
			return err
		}
		entry.Path, err = ix.EntryPath(ctx, entry.ID)
		return err
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entry": entry})
}

// scanStop ends the scan in progress; the previous index stays in use.
func (s *Server) scanStop(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	if err := s.app.StopScan(); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	settings, err := s.app.Settings()
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, settings)
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request, sess auth.Session) {
	var settings core.Settings
	if err := decode(w, r, &settings); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.app.SaveSettings(settings); err != nil {
		s.fail(w, err)
		return
	}
	s.getSettings(w, r, sess)
}

func (s *Server) entry(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	resp := map[string]any{"annotation": nil, "links": []index.Row{}}
	err = s.app.View(func(ix *index.Index) error {
		entry, err := ix.Entry(ctx, id)
		if err != nil {
			return err
		}
		if entry.Path, err = ix.EntryPath(ctx, id); err != nil {
			return err
		}
		one := []index.Row{entry}
		if err := ix.FillTypes(ctx, one); err != nil {
			return err
		}
		resp["entry"] = one[0]
		if entry.Nlink > 1 && !entry.IsDir() {
			links, err := ix.ByInode(ctx, entry.Dev, entry.Ino)
			if err != nil {
				return err
			}
			if err := ix.FillPaths(ctx, links); err != nil {
				return err
			}
			resp["links"] = links
		}
		return nil
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	if an, found, err := s.app.Annotation(ctx, id); err != nil {
		s.fail(w, err)
		return
	} else if found {
		resp["annotation"] = an
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) putAnnotation(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	var in core.AnnotationInput
	if err := decode(w, r, &in); err != nil {
		s.fail(w, err)
		return
	}
	// Writing the annotation should finish even if the browser has already
	// moved on to the next item in a review.
	an, err := s.app.PutAnnotation(context.WithoutCancel(r.Context()), id, in)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"annotation": an})
}

func (s *Server) deleteAnnotation(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	if err := s.app.DeleteAnnotation(context.WithoutCancel(r.Context()), id); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) getPrefixes(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	list, err := s.app.Prefixes()
	if err != nil {
		s.fail(w, err)
		return
	}
	// How many items carry each one; nothing to count before the first scan.
	s.app.View(func(ix *index.Index) error {
		counts, err := ix.PrefixCounts(r.Context())
		for i := range list {
			list[i].Count = counts[list[i].Name]
		}
		return err
	})
	writeJSON(w, http.StatusOK, map[string]any{"prefixes": list})
}

// deleteShareAnnotation removes a note whose item has gone missing.
func (s *Server) deleteShareAnnotation(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		s.fail(w, badRequest("give the note's path in the path parameter"))
		return
	}
	if err := s.app.DeleteMissingAnnotation(context.WithoutCancel(r.Context()), id, path); err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) putPrefixes(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var req struct {
		Prefixes []core.Prefix `json:"prefixes"`
	}
	if err := decode(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	if err := s.app.SetPrefixes(req.Prefixes); err != nil {
		s.fail(w, err)
		return
	}
	s.getPrefixes(w, r, auth.Session{})
}

// queueDef is a review queue as a client describes it.
type queueDef struct {
	Filter         index.Filter `json:"filter"`
	Groups         []string     `json:"groups"`
	Order          string       `json:"order"`
	Desc           *bool        `json:"desc"`
	Limit          int          `json:"limit"`
	Offset         int          `json:"offset"`
	ExcludeCovered bool         `json:"exclude_covered"`
}

func (d queueDef) spec() index.QueueSpec {
	if d.Order == "" {
		d.Order = "size"
	}
	// Largest first for size; oldest first for the time keys; A to Z for names.
	desc := d.Order == "size"
	if d.Desc != nil {
		desc = *d.Desc
	}
	return index.QueueSpec{
		Filter: d.Filter, Groups: d.Groups, Order: index.Sort{Key: d.Order, Desc: desc},
		Limit: min(d.Limit, 200), Offset: d.Offset, ExcludeCovered: d.ExcludeCovered,
	}
}

// queue serves the next items of a review. The client sends the same request
// after each item it annotates or skips; finished items drop out on their own.
func (s *Server) queue(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var req queueDef
	if err := decode(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	ctx := r.Context()
	var res index.QueueResult
	err := s.app.View(func(ix *index.Index) error {
		var err error
		if res, err = ix.Queue(ctx, req.spec()); err != nil {
			return err
		}
		return ix.FillTypes(ctx, res.Items)
	})
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

// queueSkip passes over everything left in one group of a review queue. The
// group is named by the values the queue gave for it, so a request that
// arrives late cannot skip whichever group happens to be current by then.
func (s *Server) queueSkip(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	var req struct {
		queueDef
		Group []any `json:"group"`
	}
	if err := decode(w, r, &req); err != nil {
		s.fail(w, err)
		return
	}
	// Like a single save, this finishes even if the browser has moved on.
	skipped, err := s.app.SkipGroup(context.WithoutCancel(r.Context()), req.spec(), req.Group)
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skipped": skipped})
}

// Types a browser can show directly and which cannot run script in our origin.
var previewTypes = map[string]string{
	"jpg": "image/jpeg", "jpeg": "image/jpeg", "png": "image/png", "gif": "image/gif", "webp": "image/webp",
	"avif": "image/avif", "bmp": "image/bmp", "ico": "image/x-icon", "heic": "image/heic", "svg": "image/svg+xml",
	"mp4": "video/mp4", "m4v": "video/mp4", "webm": "video/webm", "mkv": "video/x-matroska", "mov": "video/quicktime",
	"ogv": "video/ogg", "avi": "video/x-msvideo", "mpg": "video/mpeg", "mpeg": "video/mpeg", "ts": "video/mp2t",
	"mp3": "audio/mpeg", "flac": "audio/flac", "wav": "audio/wav", "ogg": "audio/ogg", "opus": "audio/ogg",
	"m4a": "audio/mp4", "aac": "audio/aac",
	"pdf": "application/pdf",
}

// content streams a file for the preview pane, with Range support so video
// can seek. Files on a share are untrusted input served from our own origin,
// so the type is chosen here, never sniffed: HTML, XML and scripts go out as
// plain text, and an SVG opened as a document is sandboxed.
func (s *Server) content(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	f, row, err := s.app.OpenContent(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	ctype, ok := previewTypes[row.Ext]
	switch {
	case ok:
	case row.Type == "text" || row.Type == "subtitle":
		ctype = "text/plain; charset=utf-8"
	default:
		ctype = "application/octet-stream"
	}
	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": row.Name}))
	// The preview pane embeds this response in a frame of our own page.
	h.Set("X-Frame-Options", "SAMEORIGIN")
	h.Set("Content-Security-Policy", "frame-ancestors 'self'")
	if row.Ext == "svg" {
		h.Set("Content-Security-Policy", "sandbox; frame-ancestors 'self'")
	}
	h.Set("Cache-Control", "private, no-store")
	http.ServeContent(w, r, "", st.ModTime(), f)
}

// preview serves a generated picture of a file the browser cannot show by
// itself, or with info=1 what is known about it (a film's length, size and
// codec). The picture is made the first time it is asked for and kept.
func (s *Server) preview(w http.ResponseWriter, r *http.Request, _ auth.Session) {
	id, err := pathID(r)
	if err != nil {
		s.fail(w, err)
		return
	}
	path, info, err := s.app.Preview(r.Context(), id)
	if err != nil {
		s.fail(w, err)
		return
	}
	if r.URL.Query().Get("info") != "" {
		writeJSON(w, http.StatusOK, info)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		s.fail(w, err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", st.ModTime(), f)
}
