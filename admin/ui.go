package main

import (
	"embed"
	"io/fs"
)

//go:embed static
var staticEmbed embed.FS

// uiFS returns the embedded static asset tree rooted at static/.
func uiFS() fs.FS {
	sub, err := fs.Sub(staticEmbed, "static")
	if err != nil {
		panic(err)
	}
	return sub
}
