package main

// Release and local build scripts inject the Git-derived version with -X.
// Plain go build remains usable and reports an unversioned development build.
var version = "dev"
