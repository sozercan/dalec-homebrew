// Package prebuilt verifies narrowly profiled prebuilt executable archives and
// deterministically derives receiptless Homebrew bottles from them.
//
// Verification is entirely static. Archive members are never written to disk
// or executed, Formula Ruby is copied byte-for-byte without evaluation, and
// executable inspection uses Go's ELF and build-information parsers.
package prebuilt
