package web

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path"
	"regexp"
	"strings"
	"testing"
)

// The libraries in lib/ are other people's published files. A change to one
// must be deliberate: made by lib/update.sh, which rewrites the checksums.
func TestLibrariesMatchTheirChecksums(t *testing.T) {
	sums, err := os.ReadFile("lib/CHECKSUMS")
	if err != nil {
		t.Fatal(err)
	}
	listed := 0
	for _, line := range strings.Split(strings.TrimSpace(string(sums)), "\n") {
		want, name, ok := strings.Cut(line, "  ")
		if !ok {
			t.Fatalf("unreadable line in CHECKSUMS: %q", line)
		}
		data, err := Files.ReadFile(path.Join("lib", path.Base(name)))
		if err != nil {
			t.Errorf("%s is listed but not in the binary: %v", name, err)
			continue
		}
		if got := sha256.Sum256(data); hex.EncodeToString(got[:]) != want {
			t.Errorf("%s does not match its checksum; run web/lib/update.sh or restore the file", name)
		}
		listed++
	}
	inBinary, _ := fs.Glob(Files, "lib/*.js")
	if listed != len(inBinary) {
		t.Errorf("%d libraries are listed in CHECKSUMS but %d are in the binary", listed, len(inBinary))
	}
}

var imports = regexp.MustCompile(`(?:from|import)\s*["']([^"']+)["']`)

// Every import must name a file that is served. A browser cannot resolve a
// package name, and a wrong path only shows as a blank page.
func TestEveryImportResolves(t *testing.T) {
	files, _ := fs.Glob(Files, "app/*.js")
	libs, _ := fs.Glob(Files, "lib/*.js")
	for _, file := range append(files, libs...) {
		src, err := Files.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range imports.FindAllStringSubmatch(string(src), -1) {
			target := m[1]
			if !strings.HasPrefix(target, "./") && !strings.HasPrefix(target, "../") {
				t.Errorf("%s imports %q, which is not a path to a file", file, target)
				continue
			}
			if _, err := fs.Stat(Files, path.Join(path.Dir(file), target)); err != nil {
				t.Errorf("%s imports %q, which is not in the binary", file, target)
			}
		}
	}
	if len(files) == 0 || len(libs) == 0 {
		t.Fatal("the interface's files are missing from the binary")
	}
}

// The page is served under a policy that allows no inline script or style,
// and names from the pool are untrusted text. These are the ways either rule
// gets broken by accident.
func TestNoInlineCodeOrRawMarkup(t *testing.T) {
	page, err := Files.ReadFile("index.html")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"<style", " style=", " onclick=", " onload=", "javascript:"} {
		if strings.Contains(strings.ToLower(string(page)), bad) {
			t.Errorf("index.html contains %q, which the content security policy forbids", bad)
		}
	}
	if regexp.MustCompile(`(?i)<script(?:\s[^>]*)?>\s*[^<\s]`).Match(page) {
		t.Error("index.html has an inline script")
	}
	files, _ := fs.Glob(Files, "app/*.js")
	for _, file := range files {
		src, _ := Files.ReadFile(file)
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "dangerouslySetInnerHTML", "document.write", "eval(", "new Function"} {
			if strings.Contains(string(src), bad) {
				t.Errorf("%s uses %s: text from the pool must never be parsed as markup or code", file, bad)
			}
		}
	}
}
