// Package web holds the browser interface. There is no build step: the files
// in this folder are the files that run, and the binary carries them as they
// are.
package web

import "embed"

// Files is the interface: one page, one stylesheet, the application as ES
// modules in app/, and the libraries it is built on in lib/.
//
//go:embed index.html style.css app/*.js app/*.svg lib/*.js
var Files embed.FS
