// envtesthelpers_test.go — shared env-handling test helpers used by
// both cache_test.go (package storage_test) and env_test.go
// (package storage). Lives in its own file because Go forbids a
// _test.go file from being imported across test packages, but
// helpers in the same test package ARE accessible.

package storage_test

import (
	"os"
	"testing"
)

// unsetEnvForTest unsets key for the duration of the test, restoring
// the prior value (set or unset) on cleanup. Use when a test wants
// the genuine "unset" state rather than "set to empty" — t.Setenv
// cannot produce unset, and bare os.Unsetenv leaks the unset into
// any parallel test that runs after this one returns.
//
// Mirrors the helper in env_test.go (package storage). Duplicated
// because Go's test-package boundary prevents sharing. The two
// implementations MUST stay byte-for-byte identical.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	prev, ok := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("unsetenv %s: %v", key, err)
	}
	t.Cleanup(func() {
		if ok {
			_ = os.Setenv(key, prev)
		} else {
			_ = os.Unsetenv(key)
		}
	})
}