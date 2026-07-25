package api

// This file holds SDK methods for endpoints the CLI doesn't (yet)
// wrap but the spec exposes. The list below is the complete diff
// between the OpenAPI routes and the methods declared in
// pkg/api/client.go — every entry here is reachable via
// `pkg/api.Client.<Method>` even though `faas <subcommand>` doesn't
// invoke it today.
//
// As of 2026-07-25 there are no entries here: every spec endpoint
// has a corresponding `faas <subcommand>` wrapper. When the CLI adds
// a new endpoint before the wrapper lands, ADD a typed method here;
// `make sdk-check` will fail if you ship an OpenAPI route without
// one. Once the wrapper lands, MOVE the method to client.go and
// delete the doc block.
//
// The empty file is the signal that the CLI tracks the API surface
// 1:1 — don't delete it; the next unwrapped endpoint will be added
// back here.
