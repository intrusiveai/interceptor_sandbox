package buildinfo

// Version is replaced with the candidate branch version for release builds.
var Version = "0.1.0-dev"

// SourceCommit is set alongside Version for both executables in a release.
// An empty value is reserved for source/development builds.
var SourceCommit = ""
