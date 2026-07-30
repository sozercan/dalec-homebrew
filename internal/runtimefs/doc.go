// Package runtimefs assembles a verified Homebrew materialization into a clean,
// immutable runtime prefix.
//
// The package never copies the materializer prefix recursively. Callers provide
// an explicit allowlist, and Assemble copies only the exact closure described by
// a resolution.Record plus approved global and runtime-state paths. Assembly is
// followed by a second filesystem verification pass before the output is made
// visible.
package runtimefs
