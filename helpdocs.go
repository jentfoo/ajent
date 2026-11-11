package main

import (
	"embed"
	"io/fs"

	"github.com/jentfoo/ajent/pkg/app"
)

// The user-facing documentation /help answers from: README plus one file per
// surface under docs/features. Only this repo-root package can embed both — Go
// rejects patterns reaching above the package dir ("../top.txt: invalid pattern
// syntax") and refuses to embed a symlink — so this file owns the embed and
// hands one value down through app.RunOptions.
//
//go:embed README.md docs/features/*.md
var helpDocFS embed.FS

// helpDocs converts the embedded tree into the doc set the driver's /help
// controller answers from. Every member is compile-time verified to exist, so a
// read failure here is a build bug and panics rather than shipping a half-empty
// reference.
func helpDocs() []app.HelpDoc {
	readme, err := helpDocFS.ReadFile("README.md")
	if err != nil {
		panic("helpdocs: " + err.Error())
	}
	paths, err := fs.Glob(helpDocFS, "docs/features/*.md")
	if err != nil {
		panic("helpdocs: " + err.Error())
	}
	docs := make([]app.HelpDoc, 0, len(paths)+1)
	docs = append(docs, app.HelpDoc{Name: "README.md", Body: string(readme)})
	for _, p := range paths {
		body, rerr := helpDocFS.ReadFile(p)
		if rerr != nil {
			panic("helpdocs: " + rerr.Error())
		}
		docs = append(docs, app.HelpDoc{Name: p, Body: string(body)})
	}
	return docs
}
