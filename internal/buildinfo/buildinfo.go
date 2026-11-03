// Package buildinfo holds the build identity of the bbbench binaries. The
// values are set at link time by the Makefile and the release scripts:
//
//	-ldflags "-X bbbench/internal/buildinfo.Version=0.2.0
//	          -X bbbench/internal/buildinfo.Commit=<sha>
//	          -X bbbench/internal/buildinfo.Date=<RFC 3339 UTC>"
package buildinfo

// Set by -ldflags -X. A plain `go build` reports a development build.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// String returns "<version> (commit <sha>, built <date>)".
func String() string {
	return Version + " (commit " + Commit + ", built " + Date + ")"
}
