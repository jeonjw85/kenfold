// Package buildinfo exposes version metadata injected at build time via
// -ldflags "-X github.com/kenfold/kenfold/internal/buildinfo.Version=...".
package buildinfo

var (
	Version = "dev"
	Commit  = "none"
	Date    = "unknown"
)
