package index

import "strings"

// Categories group file extensions into the file types the UI filters and
// sorts by. The numeric values are stored in the index.
const (
	CatOther = iota
	CatFolder
	CatVideo
	CatAudio
	CatImage
	CatDocument
	CatArchive
	CatDiskImage
	CatText
	CatSubtitle
)

var catNames = []string{"other", "folder", "video", "audio", "image", "document", "archive", "disk-image", "text", "subtitle"}

func CatName(cat int) string {
	if cat < 0 || cat >= len(catNames) {
		return catNames[CatOther]
	}
	return catNames[cat]
}

// CatByName returns the category for a name from CatName.
func CatByName(name string) (int, bool) {
	for i, n := range catNames {
		if n == name {
			return i, true
		}
	}
	return 0, false
}

var catByExt = func() map[string]int {
	groups := map[int]string{
		CatVideo:     "mkv mp4 m4v avi mov wmv flv webm mpg mpeg m2ts ts vob ogv 3gp divx",
		CatAudio:     "mp3 flac wav aac ogg opus m4a m4b wma alac aiff ape dsf mka",
		CatImage:     "jpg jpeg png gif bmp tif tiff webp heic heif avif svg ico psd raw cr2 cr3 nef arw dng orf rw2",
		CatDocument:  "pdf doc docx xls xlsx ppt pptx odt ods odp rtf epub mobi azw3 pages numbers key djvu",
		CatArchive:   "zip rar 7z tar gz tgz bz2 tbz2 xz txz zst lz4 lzma cab jar war r00 001",
		CatDiskImage: "iso img vmdk vdi vhd vhdx qcow2 dmg bin cue nrg wim",
		CatText:      "txt md log csv tsv json xml yaml yml toml ini conf cfg nfo html htm css js ts py go rs c h cpp java sh sql tex",
		CatSubtitle:  "srt ass ssa sub idx vtt sup",
	}
	m := make(map[string]int)
	for cat, exts := range groups {
		for _, e := range strings.Fields(exts) {
			if _, dup := m[e]; !dup {
				m[e] = cat
			}
		}
	}
	// "ts" is far more often an MPEG transport stream than TypeScript on a NAS.
	m["ts"] = CatVideo
	return m
}()

// classify returns the lowercased extension of name and its category.
// Dotfiles such as ".bashrc" have no extension.
func classify(name string) (string, int) {
	i := strings.LastIndexByte(name, '.')
	if i <= 0 || i == len(name)-1 || len(name)-i-1 > 10 {
		return "", CatOther
	}
	ext := strings.ToLower(name[i+1:])
	for _, r := range ext {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return "", CatOther
		}
	}
	return ext, catByExt[ext]
}
