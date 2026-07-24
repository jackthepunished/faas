package gateway

// unlimitedLimiter returns a Limiter whose Allow always returns true. Used
// by the fan-out / cold-coalesce / load tests to disable the per-app rate
// limit so the harness isn't constrained by the default token-bucket
// configuration. Lives in a non-build-tagged file so it is available to
// both the regular suite (handler_test.go) and the load-tagged suite
// (handler_load_test.go) without colliding under `-tags=load`.
func unlimitedLimiter() *Limiter {
	return NewLimiter().WithNoop()
}
