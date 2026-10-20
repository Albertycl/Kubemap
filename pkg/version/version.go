// Package version holds the build version, set at link time with
// -ldflags "-X kubemap/pkg/version.Version=...".
package version

// Version is "dev" unless overridden at build time.
var Version = "dev"
