// Package jsonschemautil hosts the small helpers shared between
// pkg/edgevalidate (gatewayd-internal side of the validate-edge-rule
// path) and pkg/openapiimport (apid write boundary for customer
// imports). Both packages use santhosh-tekuri/jsonschema/v6 and
// both need to translate the validator's *ValidationError into a
// shape the wire layer can render.
//
// The helpers here are intentionally minimal:
//
//   - JoinInstanceLocation: InstanceLocation []string → slash-
//     separated JSON Pointer path. RFC 6901 separator.
//   - DefaultPrinter: the package-private English-language
//     message.Printer that jsonschema/v6 wants passed to
//     ErrorKind.LocalizedString. LocalizedString panics on nil.
//
// The two callers previously duplicated both helpers. This
// package factors them out so a regression in the JSON Pointer
// shape (e.g., percent-encoding per RFC 6901 §3) lands in one
// place.
//
// Note: this is NOT a general-purpose validator. It is the
// thin translation layer between jsonschema/v6 and the Gregale
// wire shape. Tests live in the consuming packages
// (pkg/edgevalidate, pkg/openapiimport).
package jsonschemautil

import (
	"golang.org/x/text/language"
	"golang.org/x/text/message"
)

// DefaultPrinter is the package-private English-language printer
// passed to ErrorKind.LocalizedString. LocalizedString panics
// on a nil printer (per jsonschema/v6 output.go); the variable
// is package-level so each caller doesn't need to instantiate
// its own (the message.Printer is internally synced for
// Printf/String calls — safe for concurrent use).
var DefaultPrinter = message.NewPrinter(language.English)

// JoinInstanceLocation joins an InstanceLocation []string into
// a slash-separated JSON Pointer-style path (e.g. ["address",
// "zip"] → "/address/zip"). Empty slice → empty string.
//
// The separator is "/" per RFC 6901 §3 ("the separator between
// parts is the '/' character"). The wire-shape FieldError in
// pkg/api/dto.go can decide on a different separator at the
// presentation layer; this helper emits the canonical JSON
// Pointer shape.
//
// Mirrored verbatim from the previous pkg/edgevalidate and
// pkg/openapiimport copies; both packages now call this
// shared implementation.
func JoinInstanceLocation(loc []string) string {
	if len(loc) == 0 {
		return ""
	}
	var sz int
	for _, s := range loc {
		sz += 1 + len(s)
	}
	out := make([]byte, 0, sz)
	for _, s := range loc {
		out = append(out, '/')
		out = append(out, s...)
	}
	return string(out)
}
