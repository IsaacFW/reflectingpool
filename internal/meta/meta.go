// Package meta stores annotations in a .reflection directory at the root of
// each share, as plain text that stays readable without this program.
//
// Whatever is in a share was put there by whoever can write to it, and this
// program runs with more access than they have. So the metadata folder is
// never reached by path: the share is opened one name at a time, refusing
// links, and everything in .reflection must be an ordinary file this program
// made. Anything else is reported as a Suspect and left alone.
package meta

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/safefs"
	"golang.org/x/sys/unix"
)

const (
	// Dir is the metadata directory at the root of each share.
	Dir       = ".reflection"
	fileName  = "annotations.jsonl"
	skipName  = "skipped.jsonl"
	indexName = "INDEX.md"
	readme    = `This folder is maintained by Reflecting Pool.

annotations.jsonl  One line per annotated file or folder in this share:
                   what it is for, its prefixes, owner and review date.
                   It is the only copy of that information. Keep it.
skipped.jsonl      One line per item that was passed over in a review with
                   nothing recorded. Deleting it returns those items to the
                   review queues at the next scan.
INDEX.md           A readable summary, regenerated from annotations.jsonl.
`
	fingerprintSpan = 64 * 1024
)

// Identity is what lets an annotation find its item again after a rename or
// a move. Dataset, Ino and Btime survive a rename within a filesystem; Size,
// Mtime and Hash identify a file that was copied to a new one.
type Identity struct {
	Dataset string `json:"dataset,omitempty"`
	Ino     uint64 `json:"ino,omitempty"`
	Btime   int64  `json:"btime,omitempty"`
	BtimeNs int64  `json:"btime_ns,omitempty"` // sub-second part of Btime
	Size    int64  `json:"size,omitempty"`
	Mtime   int64  `json:"mtime,omitempty"`
	Hash    string `json:"hash,omitempty"`
}

// Annotation is what a user recorded about one file or folder.
type Annotation struct {
	Path        string    `json:"path"` // relative to the share; "." is the share itself
	Kind        string    `json:"kind"` // "file" or "dir"
	Note        string    `json:"note,omitempty"`
	DisplayName string    `json:"display_name,omitempty"`
	Prefixes    []string  `json:"prefixes,omitempty"`
	Owner       string    `json:"owner,omitempty"`
	ReviewAfter string    `json:"review_after,omitempty"` // YYYY-MM-DD
	Skipped     bool      `json:"skipped,omitempty"`      // passed over with nothing recorded; kept in skipped.jsonl, not here
	Orphaned    bool      `json:"orphaned,omitempty"`     // the item could not be found at the last scan
	Identity    Identity  `json:"identity,omitzero"`
	Updated     time.Time `json:"updated,omitzero"`
}

// Store is the annotation file of one share. It is safe for concurrent use.
type Store struct {
	base, rel string // the trusted root and the share's path below it
	root      string // the share's path, for messages

	mu     sync.Mutex
	byPath map[string]Annotation
	stamp  time.Time // modification time of the file as last read or written
	size   int64
}

// Open returns the store of the share at rel below root. root is trusted
// and may be reached through links: it is the path the owner mapped into
// the container. Everything below it is opened without following links, so
// a share writer who swaps a folder for a link cannot send this program's
// reads or writes anywhere else. rel is empty when the root is the share.
func Open(root, rel string) *Store {
	rel = strings.Trim(filepath.Clean("/"+rel), "/")
	path := root
	if rel != "" {
		path = strings.TrimSuffix(root, "/") + "/" + rel
	}
	return &Store{base: root, rel: rel, root: path}
}

// maxFile bounds how much of a metadata file is read. A real one is far
// smaller; a huge one is someone's doing, and reading it all would take the
// server's memory.
var maxFile int64 = 1 << 30

// Suspect reports that something in the share's metadata folder is not what
// this program made: a link, a pipe, a file with more than one name, a file
// too large to be real. Nothing is read or written there until it is removed.
type Suspect struct {
	Path   string
	Reason string
}

func (e *Suspect) Error() string { return e.Path + " " + e.Reason }

// suspect turns a refusal from safefs into a Suspect; other errors pass.
func suspect(err error) error {
	var r *safefs.Refused
	if errors.As(err, &r) {
		return &Suspect{Path: r.Path, Reason: r.Reason()}
	}
	return err
}

func tooLarge(path string, size int64) error {
	return &Suspect{Path: path, Reason: fmt.Sprintf("is %d bytes, more than a real one could be", size)}
}

func (s *Store) file() string { return filepath.Join(s.root, Dir, fileName) }

// share opens the share's folder.
func (s *Store) share() (*safefs.Dir, error) {
	root, err := safefs.Open(s.base)
	if err != nil {
		return nil, err
	}
	if s.rel == "" {
		return root, nil
	}
	defer root.Close()
	return root.Walk(s.rel)
}

// place is who should own the share's metadata and with what permissions.
type place struct {
	fileMode, dirMode os.FileMode
	uid, gid          int
}

// give hands a file this program made the share's owner and permissions, so
// it stays reachable over the network shares the rest of the folder is served
// through. It fails harmlessly when not root.
func (p place) give(f *os.File) {
	f.Chmod(p.fileMode)
	unix.Fchown(int(f.Fd()), p.uid, p.gid)
}

// metaDir opens the share's metadata folder, making it when create is set
// and the folder is missing. A link or a file in its place is a Suspect, and
// so is a share folder that cannot be reached without following a link.
func (s *Store) metaDir(create bool) (*safefs.Dir, place, error) {
	share, err := s.share()
	if err != nil {
		return nil, place{}, suspect(err)
	}
	defer share.Close()
	owner, err := share.Stat()
	if err != nil {
		return nil, place{}, err
	}
	dirMode := os.FileMode(owner.Mode & 0o777)
	p := place{dirMode: dirMode, fileMode: dirMode &^ 0o111, uid: int(owner.Uid), gid: int(owner.Gid)}
	made := false
	if create {
		if made, err = share.Mkdir(Dir, dirMode); err != nil {
			return nil, place{}, err
		}
	}
	dir, err := share.Sub(Dir)
	if err != nil {
		return nil, place{}, suspect(err)
	}
	if made {
		dir.Chmod(dirMode)
		dir.Chown(p.uid, p.gid)
	}
	return dir, p, nil
}

// open opens one of the metadata files for reading, or reports that there is
// none. The file must be an ordinary one of a believable size.
func (s *Store) open(name string) (*os.File, error) {
	dir, _, err := s.metaDir(false)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	f, err := dir.OpenFile(name, os.O_RDONLY, 0, false)
	if err != nil {
		return nil, suspect(err)
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if st.Size() > maxFile {
		f.Close()
		return nil, tooLarge(f.Name(), st.Size())
	}
	return f, nil
}

// load refreshes the in-memory copy if the file changed on disk, so edits made
// by hand or by another tool are picked up.
func (s *Store) load() error {
	f, err := s.open(fileName)
	if errors.Is(err, fs.ErrNotExist) {
		s.byPath, s.stamp, s.size = map[string]Annotation{}, time.Time{}, 0
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if s.byPath != nil && st.ModTime().Equal(s.stamp) && st.Size() == s.size {
		return nil
	}
	byPath := make(map[string]Annotation)
	sc := bufio.NewScanner(io.LimitReader(f, maxFile))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var a Annotation
		if err := json.Unmarshal(line, &a); err != nil {
			// Refuse to go on: rewriting the file would drop this line.
			return fmt.Errorf("%s line %d: %w", s.file(), n, err)
		}
		if a.Path == "" {
			return fmt.Errorf("%s line %d: no path", s.file(), n)
		}
		byPath[a.Path] = a
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("%s: %w", s.file(), err)
	}
	s.byPath, s.stamp, s.size = byPath, st.ModTime(), st.Size()
	return nil
}

// All returns every annotation, ordered by path.
func (s *Store) All() ([]Annotation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, err
	}
	return s.sorted(), nil
}

func (s *Store) sorted() []Annotation {
	out := make([]Annotation, 0, len(s.byPath))
	for _, a := range s.byPath {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (s *Store) Get(path string) (Annotation, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Annotation{}, false, err
	}
	a, ok := s.byPath[path]
	return a, ok, nil
}

// Update applies fn to the annotations and saves them if fn reports a change.
func (s *Store) Update(fn func(byPath map[string]Annotation) bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	if !fn(s.byPath) {
		return nil
	}
	return s.save()
}

func (s *Store) Put(a Annotation) error {
	return s.Update(func(m map[string]Annotation) bool {
		m[a.Path] = a
		return true
	})
}

func (s *Store) Delete(path string) error {
	return s.Update(func(m map[string]Annotation) bool {
		_, ok := m[path]
		delete(m, path)
		return ok
	})
}

// explain keeps the note that says what the files are for up to date. It is
// a convenience; nothing depends on it being written. Something odd in its
// place is still reported, like anything else in the folder.
func (p place) explain(dir *safefs.Dir) error {
	f, err := dir.OpenFile("README.txt", os.O_RDONLY, 0, false)
	if err == nil {
		cur, rerr := io.ReadAll(io.LimitReader(f, int64(len(readme))+1))
		f.Close()
		if rerr == nil && string(cur) == readme {
			return nil
		}
	} else if err = suspect(err); isSuspect(err) {
		return err
	}
	if _, err := writeAtomic(dir, "README.txt", []byte(readme), p); isSuspect(err) {
		return err
	}
	return nil
}

func isSuspect(err error) bool {
	var s *Suspect
	return errors.As(err, &s)
}

func (s *Store) save() error {
	dir, p, err := s.metaDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	var buf bytes.Buffer
	for _, a := range s.sorted() {
		line, err := json.Marshal(a)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	st, err := writeAtomic(dir, fileName, buf.Bytes(), p)
	if err != nil {
		return err
	}
	s.stamp, s.size = st.ModTime(), st.Size()

	// The summary is a convenience; the annotations are saved whether or not
	// it can be written.
	if _, err := writeAtomic(dir, indexName, s.summary(), p); isSuspect(err) {
		return err
	}
	return p.explain(dir)
}

func skipLines(paths []string) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf) // one value per line
	enc.SetEscapeHTML(false)
	for _, path := range paths {
		if err := enc.Encode(path); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// AddSkips appends items to the share's list of skipped items.
//
// The list is kept apart from the annotations because it grows by tens of
// thousands of lines at a time, and because a skip records nothing: adding
// one should cost a line, not a rewrite of everything a person has written.
func (s *Store) AddSkips(paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	lines, err := skipLines(paths)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, p, err := s.metaDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	// Appending writes into whatever the name belongs to, so the file must
	// be an ordinary one with this single name.
	f, err := dir.OpenFile(skipName, os.O_RDWR|os.O_APPEND|os.O_CREATE, p.fileMode, true)
	if err != nil {
		return suspect(err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		p.give(f)
	} else {
		// A write cut short by a crash can leave half a line at the end.
		// Start a new line, so that only the damaged skip is lost.
		last := make([]byte, 1)
		if _, err := f.ReadAt(last, st.Size()-1); err != nil {
			return err
		}
		if last[0] != '\n' {
			lines = append([]byte{'\n'}, lines...)
		}
	}
	if _, err := f.Write(lines); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := p.explain(dir); err != nil {
		return err
	}
	return f.Close()
}

// Skips returns the skipped items in the order they were recorded.
func (s *Store) Skips() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readSkips()
}

func (s *Store) readSkips() ([]string, error) {
	f, err := s.open(skipName)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(io.LimitReader(f, maxFile))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		// A line that cannot be read is passed over, unlike in the
		// annotations: the worst that follows is that one item returns to a
		// review queue.
		var path string
		if json.Unmarshal(sc.Bytes(), &path) == nil && path != "" {
			out = append(out, path)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("%s: %w", f.Name(), err)
	}
	return out, nil
}

// EditSkips passes the list of skipped items to fn and stores what fn
// returns, if that differs. fn runs with the store locked, so nothing can be
// appended between the reading and the writing.
func (s *Store) EditSkips(fn func(paths []string) []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	old, err := s.readSkips()
	if err != nil {
		return err
	}
	next := fn(slices.Clone(old))
	if slices.Equal(old, next) {
		return nil
	}
	if len(next) == 0 {
		dir, _, err := s.metaDir(false)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer dir.Close()
		if err := checkRegular(dir, skipName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := dir.Remove(skipName); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	lines, err := skipLines(next)
	if err != nil {
		return err
	}
	dir, p, err := s.metaDir(true)
	if err != nil {
		return err
	}
	defer dir.Close()
	if _, err := writeAtomic(dir, skipName, lines, p); err != nil {
		return err
	}
	return p.explain(dir)
}

// RemoveSkip takes one item off the list of skipped items.
func (s *Store) RemoveSkip(path string) error {
	return s.EditSkips(func(paths []string) []string {
		return slices.DeleteFunc(paths, func(p string) bool { return p == path })
	})
}

// Check looks over the share's metadata folder without changing anything and
// reports the first thing there that this program did not make.
func (s *Store) Check() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir, _, err := s.metaDir(false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	for _, name := range []string{fileName, skipName, indexName, "README.txt"} {
		if err := checkRegular(dir, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

// checkRegular reports a Suspect unless the entry is an ordinary file of a
// believable size with one name, or is missing.
func checkRegular(dir *safefs.Dir, name string) error {
	st, err := dir.Lstat(name)
	if err != nil {
		return err
	}
	path := dir.Path() + "/" + name
	switch {
	case st.Mode&unix.S_IFMT == unix.S_IFLNK:
		return &Suspect{Path: path, Reason: (&safefs.Refused{Why: safefs.IsLink}).Reason()}
	case st.Mode&unix.S_IFMT != unix.S_IFREG:
		return &Suspect{Path: path, Reason: (&safefs.Refused{Why: safefs.NotRegular}).Reason()}
	case st.Nlink > 1:
		return &Suspect{Path: path, Reason: (&safefs.Refused{Why: safefs.ManyNames}).Reason()}
	case st.Size > maxFile:
		return tooLarge(path, st.Size)
	}
	return nil
}

// writeAtomic replaces name in dir so that a crash leaves either the old
// contents or the new, never a partial file. What stands at name must be an
// ordinary file or nothing: a link there is someone else's doing.
func writeAtomic(dir *safefs.Dir, name string, data []byte, p place) (os.FileInfo, error) {
	if err := checkRegular(dir, name); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	tmp, err := dir.CreateTemp(".tmp-", p.fileMode)
	if err != nil {
		return nil, err
	}
	tmpName := filepath.Base(tmp.Name())
	fail := func(err error) (os.FileInfo, error) {
		tmp.Close()
		dir.Remove(tmpName)
		return nil, err
	}
	if _, err := tmp.Write(data); err != nil {
		return fail(err)
	}
	if err := tmp.Sync(); err != nil {
		return fail(err)
	}
	p.give(tmp)
	st, err := tmp.Stat()
	if err != nil {
		return fail(err)
	}
	if err := tmp.Close(); err != nil {
		dir.Remove(tmpName)
		return nil, err
	}
	if err := dir.Rename(tmpName, name); err != nil {
		dir.Remove(tmpName)
		return nil, err
	}
	return st, nil
}

func (s *Store) summary() []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\nGenerated by Reflecting Pool from %s. Changes made here are overwritten.\n\n", filepath.Base(s.root), fileName)
	b.WriteString("| Item | Prefixes | What it is | Owner | Review after |\n|---|---|---|---|---|\n")
	cell := strings.NewReplacer("|", `\|`, "\r", "", "\n", " ")
	n := 0
	for _, a := range s.sorted() {
		if a.Note == "" && len(a.Prefixes) == 0 && a.DisplayName == "" && a.Owner == "" && a.ReviewAfter == "" {
			continue
		}
		item := a.Path
		if a.Kind == "dir" && item != "." {
			item += "/"
		}
		if a.DisplayName != "" {
			item += " (" + a.DisplayName + ")"
		}
		if a.Orphaned {
			item += " (missing)"
		}
		fmt.Fprintf(&b, "| %s | %s | %s | %s | %s |\n", cell.Replace(item), cell.Replace(strings.Join(a.Prefixes, ", ")),
			cell.Replace(a.Note), cell.Replace(a.Owner), a.ReviewAfter)
		n++
	}
	if n == 0 {
		b.WriteString("| (nothing annotated yet) | | | | |\n")
	}
	return []byte(b.String())
}

// Fingerprint hashes the first and last 64 KiB of a file together with its
// size. It is cheap at any file size and enough to recognise a file that was
// copied elsewhere, alongside its size and modified time.
func Fingerprint(f io.ReaderAt, size int64) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%d\n", size)
	buf := make([]byte, fingerprintSpan)
	read := func(off int64) error {
		n, err := f.ReadAt(buf, off)
		if err != nil && err != io.EOF {
			return err
		}
		h.Write(buf[:n])
		return nil
	}
	if err := read(0); err != nil {
		return "", err
	}
	if size > fingerprintSpan {
		if err := read(max(size-fingerprintSpan, fingerprintSpan)); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
