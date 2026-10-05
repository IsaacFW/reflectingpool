// Package meta stores annotations in a .reflection directory at the root of
// each share, as plain text that stays readable without this program.
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
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

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
	root string

	mu     sync.Mutex
	byPath map[string]Annotation
	stamp  time.Time // modification time of the file as last read or written
	size   int64
}

func Open(shareRoot string) *Store {
	return &Store{root: shareRoot}
}

func (s *Store) file() string { return filepath.Join(s.root, Dir, fileName) }

// load refreshes the in-memory copy if the file changed on disk, so edits made
// by hand or by another tool are picked up.
func (s *Store) load() error {
	st, err := os.Stat(s.file())
	if errors.Is(err, os.ErrNotExist) {
		s.byPath, s.stamp, s.size = map[string]Annotation{}, time.Time{}, 0
		return nil
	}
	if err != nil {
		return err
	}
	if s.byPath != nil && st.ModTime().Equal(s.stamp) && st.Size() == s.size {
		return nil
	}
	data, err := os.ReadFile(s.file())
	if err != nil {
		return err
	}
	byPath := make(map[string]Annotation)
	sc := bufio.NewScanner(bytes.NewReader(data))
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

// place is where a share's metadata lives and who should own it.
type place struct {
	dir      string
	fileMode os.FileMode
	uid, gid int
}

func (p place) give(path string, mode os.FileMode) {
	os.Chmod(path, mode)
	os.Lchown(path, p.uid, p.gid) // fails harmlessly when not root
}

// prepare creates the metadata directory if it is missing. Everything in it
// takes the share's owner and permissions, so it stays reachable over the
// network shares the rest of the folder is served through.
func (s *Store) prepare() (place, error) {
	var owner unix.Stat_t
	if err := unix.Stat(s.root, &owner); err != nil {
		return place{}, err
	}
	dirMode := os.FileMode(owner.Mode & 0o777)
	p := place{dir: filepath.Join(s.root, Dir), fileMode: dirMode &^ 0o111, uid: int(owner.Uid), gid: int(owner.Gid)}
	if err := os.Mkdir(p.dir, dirMode); err == nil {
		p.give(p.dir, dirMode)
	} else if !errors.Is(err, os.ErrExist) {
		return place{}, err
	}
	return p, nil
}

// explain keeps the note that says what the files are for up to date. It is
// a convenience; nothing depends on it being written.
func (p place) explain() {
	path := filepath.Join(p.dir, "README.txt")
	if cur, err := os.ReadFile(path); err == nil && string(cur) == readme {
		return
	}
	if writeAtomic(path, []byte(readme)) == nil {
		p.give(path, p.fileMode)
	}
}

func (s *Store) save() error {
	p, err := s.prepare()
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	for _, a := range s.sorted() {
		line, err := json.Marshal(a)
		if err != nil {
			return err
		}
		buf.Write(line)
		buf.WriteByte('\n')
	}
	if err := writeAtomic(s.file(), buf.Bytes()); err != nil {
		return err
	}
	p.give(s.file(), p.fileMode)
	st, err := os.Stat(s.file())
	if err != nil {
		return err
	}
	s.stamp, s.size = st.ModTime(), st.Size()

	// The summary is a convenience; the annotations are saved whether or not
	// it can be written.
	if writeAtomic(filepath.Join(p.dir, indexName), s.summary()) == nil {
		p.give(filepath.Join(p.dir, indexName), p.fileMode)
	}
	p.explain()
	return nil
}

func (s *Store) skipFile() string { return filepath.Join(s.root, Dir, skipName) }

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
	p, err := s.prepare()
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.skipFile(), os.O_RDWR|os.O_APPEND|os.O_CREATE, p.fileMode)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() == 0 {
		p.give(s.skipFile(), p.fileMode)
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
	p.explain()
	return f.Close()
}

// Skips returns the skipped items in the order they were recorded.
func (s *Store) Skips() ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readSkips()
}

func (s *Store) readSkips() ([]string, error) {
	f, err := os.Open(s.skipFile())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
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
		return nil, fmt.Errorf("%s: %w", s.skipFile(), err)
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
		if err := os.Remove(s.skipFile()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	lines, err := skipLines(next)
	if err != nil {
		return err
	}
	p, err := s.prepare()
	if err != nil {
		return err
	}
	if err := writeAtomic(s.skipFile(), lines); err != nil {
		return err
	}
	p.give(s.skipFile(), p.fileMode)
	p.explain()
	return nil
}

// RemoveSkip takes one item off the list of skipped items.
func (s *Store) RemoveSkip(path string) error {
	return s.EditSkips(func(paths []string) []string {
		return slices.DeleteFunc(paths, func(p string) bool { return p == path })
	})
}

// writeAtomic replaces path so that a crash leaves either the old contents or
// the new, never a partial file.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
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
