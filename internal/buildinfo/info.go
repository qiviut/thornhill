// Package buildinfo exposes immutable source metadata injected by the linker.
package buildinfo

import "regexp"

// Commit is set at build time with -X. Development builds intentionally report
// "unknown" rather than pretending to correspond to a Git revision.
var Commit = "unknown"

var fullGitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Valid() bool { return fullGitSHA.MatchString(Commit) }
