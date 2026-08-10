// Test-only helpers. These functions are intended for use in
// test binaries only; production code MUST NOT call them. The
// package-level sync.Map is process-local by design; the helpers
// exist so external test packages (cmd/gatewayd-internal, cmd/apid) that
// share the store with their own webhook tests can drop rows
// between tests without each test having to forge a unique
// delivery ID.
//
// The naming convention (ResetForTest) flags these as
// test-only.

package webhookdedupe

// ResetForTest wipes the package-level sync.Map.
func ResetForTest() {
	store.Range(func(k, _ any) bool { store.Delete(k); return true })
}
