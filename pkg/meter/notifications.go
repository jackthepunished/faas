package meter

import (
	"sync"
)

// notificationsUnsubscribeURL is the boot-set URL the meterd
// quota loop attaches as the List-Unsubscribe + List-Unsubscribe-Post
// header pair on quota-warning mail (issue #246 acceptance item
// 4 — bulk-sender compliance). Set once at process start via
// SetNotificationsUnsubscribeURL; read by EnforceQuota.
//
// Why a package-level singleton rather than a Config field:
// EnforceQuota is a free function called from pkg/meter/loop.go
// with seven positional args; adding an eighth would force every
// existing test (and future extension) to thread the URL through,
// even though only one call site (the quota-warning branch)
// reads it. A package-level get/set pair keeps the seam narrow
// and the blast radius small.
//
// Concurrency: set is called exactly once at boot from
// cmd/meterd/main.go before any goroutine in the meterd process
// starts touching quota state; the rwmutex guards the
// technically-possible case of a later reload / tests that
// reconfigure between ticks.
var (
	notificationsUnsubscribeMu sync.RWMutex
	notificationsUnsubscribe   string
)

// SetNotificationsUnsubscribeURL records the URL the quota
// warning email will advertise for one-click unsubscribe (RFC
// 8058). Called once at process start from cmd/meterd/main.go
// after the env-var / sealed-env wiring is resolved.
//
// Empty string is a valid value: it disables the header on the
// quota-warning template (which is the behaviour for dev boxes
// that have not wired the dashboard yet). The bulk-sender
// compliance implication is owned by the operator — the
// platform does NOT silently substitute a placeholder URL.
func SetNotificationsUnsubscribeURL(s string) {
	notificationsUnsubscribeMu.Lock()
	notificationsUnsubscribe = s
	notificationsUnsubscribeMu.Unlock()
}

// NotificationsUnsubscribeURL returns the URL previously
// installed by SetNotificationsUnsubscribeURL, or "" if none
// has been set. Callers should treat "" as "no unsubscribe
// header" rather than a configuration error — dev boxes ship
// without one.
func NotificationsUnsubscribeURL() string {
	notificationsUnsubscribeMu.RLock()
	defer notificationsUnsubscribeMu.RUnlock()
	return notificationsUnsubscribe
}
