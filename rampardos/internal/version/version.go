package version

import "runtime/debug"

// GitCommit is set at build time via -ldflags, falls back to vcs.revision
var GitCommit = func() string {
	// First try ldflags-injected value (for Docker builds)
	if gitCommitFromLdflags != "" && gitCommitFromLdflags != "unknown" {
		return gitCommitFromLdflags
	}
	// Fall back to vcs.revision (for local builds with .git present)
	info, ok := debug.ReadBuildInfo()
	if ok {
		for _, setting := range info.Settings {
			if setting.Key == "vcs.revision" {
				return setting.Value
			}
		}
	}
	return "unknown"
}()

// gitCommitFromLdflags is set via -ldflags at build time
var gitCommitFromLdflags = ""

// ShortCommit returns the first 7 characters of the commit hash
func ShortCommit() string {
	if len(GitCommit) >= 7 {
		return GitCommit[:7]
	}
	return GitCommit
}
