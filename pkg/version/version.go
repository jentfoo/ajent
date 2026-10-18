// Package version holds the build version and the self-update paths.
package version

import "runtime/debug"

// Version is injected via ldflags, else the module version from build info, else dev.
var Version = "dev"

var userAgent string

func init() {
	var bi *debug.BuildInfo
	if v, ok := debug.ReadBuildInfo(); ok {
		bi = v
	}
	Version = resolveVersion(Version, bi)
	userAgent = "ajent/" + Version // after Version resolves, or a go install build reports dev
}

// UserAgent returns the User-Agent every outbound ajent request carries.
func UserAgent() string { return userAgent }

// resolveVersion returns injected when set, else the module version, else dev.
func resolveVersion(injected string, bi *debug.BuildInfo) string {
	if injected != "dev" {
		return injected
	} else if bi != nil && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		return bi.Main.Version
	}
	return "dev"
}
