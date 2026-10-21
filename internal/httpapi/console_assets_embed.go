//go:build consoleassets

package httpapi

import (
	"io/fs"

	"github.com/shovan04/storvia-project/internal/httpapi/consoleout"
)

// The console ships inside the binary.
//
// Embedding rather than reading from disk means the artifact an operator deploys is
// self-contained. A console served from a separate build is a console that is stale in
// exactly the deployments where somebody needs it under pressure.
//
// The build tag keeps this file out of a build without a console, so `go vet` and the Go
// test suite do not require Node to have been run.
func loadConsoleAssets() fs.FS {
	sub, err := fs.Sub(consoleout.Files, "assets")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
