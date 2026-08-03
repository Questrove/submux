package web

import "embed"

//go:embed index.html user-guide.html user-guide-og.png
var FS embed.FS
