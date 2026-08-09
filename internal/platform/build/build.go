// Package build exposes the identity of the running binary.
//
// Version, Commit, and BuildTime are injected at link time (see the Makefile's
// LDFLAGS). Everything else is read from the Go build info the toolchain
// embeds automatically.
//
// This matters more for Atlas than for most services: it is the thing
// operators consult during an incident to answer "which version is running on
// this host?", and in a fleet it is what distinguishes a metric collected by
// an old agent from one collected by a new one.
package build

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"time"
)

// Injected at link time with -X. The defaults are what a plain `go build`
// produces, so a developer build is identifiable as such rather than
// masquerading as a release.
var (
	// Version is the release version, such as "1.4.0".
	Version = "dev"
	// Commit is the git revision the binary was built from.
	Commit = "unknown"
	// BuildTime is the RFC3339 build timestamp.
	BuildTime = "unknown"
)

// Info describes the running binary.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildTime string `json:"build_time"`
	GoVersion string `json:"go_version"`
	Platform  string `json:"platform"`
	// Dirty reports whether the working tree had uncommitted changes at build
	// time. A dirty binary in production is worth knowing about: its commit
	// hash does not fully describe what is running.
	Dirty bool `json:"dirty"`
}

// Current returns the running binary's identity.
func Current() Info {
	info := Info{
		Version:   Version,
		Commit:    Commit,
		BuildTime: BuildTime,
		GoVersion: runtime.Version(),
		Platform:  runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Fall back to VCS stamping when link-time flags were not supplied, so a
	// plain `go build` still yields a usable commit.
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.BuildTime == "unknown" {
					info.BuildTime = setting.Value
				}
			case "vcs.modified":
				info.Dirty = setting.Value == "true"
			}
		}
	}
	return info
}

// String renders a one-line summary for startup logs and --version.
func (i Info) String() string {
	dirty := ""
	if i.Dirty {
		dirty = "-dirty"
	}
	return fmt.Sprintf("atlas %s (%s%s) built %s with %s for %s",
		i.Version, shortCommit(i.Commit), dirty, i.BuildTime, i.GoVersion, i.Platform)
}

func shortCommit(commit string) string {
	if len(commit) > 12 {
		return commit[:12]
	}
	return commit
}

// UserAgent returns the value Atlas sends when it acts as an HTTP client.
func UserAgent() string { return "atlas/" + Version }

// Started records process start, so uptime can be reported without every
// component tracking its own.
var Started = time.Now()

// Uptime returns how long the process has been running.
func Uptime() time.Duration { return time.Since(Started) }
