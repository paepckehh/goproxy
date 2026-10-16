// Package version reports the build version, injected via -ldflags -X.
package version

import "runtime/debug"

// Version is set at link time with -ldflags
// "-X paepcke.de/goproxy/internal/version.Version=vX.Y.Z".
var Version string

// Get returns the linked version, the module version from build info
// (go install / go run), or "dev".
func Get() string {
	if Version != "" {
		return Version
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			return v
		}
	}
	return "dev"
}
