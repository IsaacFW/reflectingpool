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
	indexName = "INDEX.md"
	readme    = `This folder is maintained by Reflecting Pool.

annotations.jsonl  One line per annotated file or folder in this share:
                   what it is for, its prefixes, owner and review date.
                   It is the only copy of that information. Keep it.
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
	Skipped     bool      `json:"skipped,omitempty"`      // passed over in a review, nothing recorded
	Orphaned    bool      `json:"orphaned,omitempty"`     // the item could not be found at the last scan
	Identity    Identity  `json:"identity"`
	Updated     time.Time `json:"updated"`
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

func (s *Store) save() error {
	var owner unix.Stat_t
	if err := unix.Stat(s.root, &owner); err != nil {
		return err
	}
	dir := filepath.Join(s.root, Dir)
	// Files take the share's owner and permissions so they stay reachable
	// over the network shares the rest of the folder is served through.
	dirMode := os.FileMode(owner.Mode & 0o777)
	fileMode := dirMode &^ 0o111
	give := func(path string, mode os.FileMode) {
		os.Chmod(path, mode)
		os.Lchown(path, int(owner.Uid), int(owner.Gid)) // fails harmlessly when not root
	}
	if err := os.Mkdir(dir, dirMode); err == nil {
		give(dir, dirMode)
	} else if !errors.Is(err, os.ErrExist) {
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
	give(s.file(), fileMode)
	st, err := os.Stat(s.file())
	if err != nil {
		return err
	}
	s.stamp, s.size = st.ModTime(), st.Size()

	// The summary and the readme are conveniences; the annotations are saved
	// whether or not these succeed.
	if writeAtomic(filepath.Join(dir, indexName), s.summary()) == nil {
		give(filepath.Join(dir, indexName), fileMode)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.txt")); errors.Is(err, os.ErrNotExist) {
		if writeAtomic(filepath.Join(dir, "README.txt"), []byte(readme)) == nil {
			give(filepath.Join(dir, "README.txt"), fileMode)
		}
	}
	return nil
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
