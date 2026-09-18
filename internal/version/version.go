// Package version carries build information injected with -ldflags.
package version

// Version is the semantic version of the binary (set by the release build).
var Version = "0.0.0-dev"

// Commit is the git commit the binary was built from.
var Commit = "unknown"

// String returns "version (commit)".
func String() string { return Version + " (" + Commit + ")" }
