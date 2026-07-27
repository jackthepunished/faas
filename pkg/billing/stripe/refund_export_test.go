// Same-package test bridge: exposes the unexported centsToMillicents
// helper to refund_test.go (which runs in package stripe_test) so the
// outbound cents→millicents conversion at the Refund call site is
// pinned without standing up the stripe-go SDK.
//
// The bridge is the standard Go test-only-export pattern (named with
// the ForTest suffix to make its scope obvious) — it doesn't widen
// the package's production surface.
package stripe

// CentsToMillicentsForTest returns cents*10 (the Stripe wire-quantity
// factor). Mirrors centsToMillicents at client.go. Lives here so the
// package-stripe_test test in refund_test.go can pin the conversion.
func CentsToMillicentsForTest(cents int64) int64 {
	return centsToMillicents(cents)
}
