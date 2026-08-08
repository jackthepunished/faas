package main

import (
	"os"
	"testing"
)

// TestTierCAppsRmUsage pins the dispatcher shape of cmdAppsRm:
// missing positional argument => usage banner + rc=1.
func TestTierCAppsRmUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdAppsRm(nil); code != 1 {
		t.Errorf("cmdAppsRm() = %d, want 1", code)
	}
}

// TestTierCRollbackUsage pins cmdRollback's argument count check.
func TestTierCRollbackUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdRollback(nil); code != 1 {
		t.Errorf("cmdRollback() = %d, want 1", code)
	}
}

// TestTierCParkUsage pins cmdPark's single-arg requirement.
func TestTierCParkUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdPark(nil); code != 1 {
		t.Errorf("cmdPark() = %d, want 1", code)
	}
}

// TestTierCWakeUsage pins cmdWake's single-arg requirement.
func TestTierCWakeUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdWake(nil); code != 1 {
		t.Errorf("cmdWake() = %d, want 1", code)
	}
}

// TestTierCDomainsUsage pins cmdDomains's argument count check.
func TestTierCDomainsUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdDomains(nil); code != 1 {
		t.Errorf("cmdDomains() = %d, want 1", code)
	}
}

// TestTierCKeysUsage pins cmdKeys's argument count check.
func TestTierCKeysUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdKeys(nil); code != 1 {
		t.Errorf("cmdKeys() = %d, want 1", code)
	}
}

// TestTierCInvoicesUsage pins cmdInvoices's unauthorized branch.
// cmdInvoices doesn't take a positional slug; it parses flags then
// calls authedClient(). With no FAAS_TOKEN, authedClient returns an
// error and cmdInvoices returns a non-zero exit. The exact code is
// order-dependent (other tests may have flipped jsonOutput before
// this one runs, which changes the printErr path) so we only assert
// the unauthorized outcome, not the specific code.
func TestTierCInvoicesUsage(t *testing.T) {
	resetJSONOut(t)
	t.Setenv("FAAS_TOKEN", "")
	t.Setenv("FAAS_API", "")
	_ = os.Unsetenv // keep import
	if code := cmdInvoices(nil); code == 0 {
		t.Errorf("cmdInvoices() = 0, want non-zero (unauthorized)")
	}
}

// TestTierCBackupUsage pins cmdBackup's argument count check.
func TestTierCBackupUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdBackup(nil); code != 1 {
		t.Errorf("cmdBackup() = %d, want 1", code)
	}
}

// TestTierCBuildUsage pins cmdBuild's argument count check.
func TestTierCBuildUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdBuild(nil); code != 1 {
		t.Errorf("cmdBuild() = %d, want 1", code)
	}
}

// TestTierCBuildSbomUsage pins cmdBuildSbom's argument count check.
func TestTierCBuildSbomUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdBuildSbom(nil); code != 1 {
		t.Errorf("cmdBuildSbom() = %d, want 1", code)
	}
}

// TestTierCBuildProvenanceUsage pins cmdBuildProvenance's argument count check.
func TestTierCBuildProvenanceUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdBuildProvenance(nil); code != 1 {
		t.Errorf("cmdBuildProvenance() = %d, want 1", code)
	}
}

// TestTierCInvocationsUsage pins cmdInvocations's argument count check.
func TestTierCInvocationsUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdInvocations(nil); code != 1 {
		t.Errorf("cmdInvocations() = %d, want 1", code)
	}
}

// TestTierCOrgsKeysUsage pins cmdOrgsKeys's argument count check.
func TestTierCOrgsKeysUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdOrgsKeys(nil); code != 1 {
		t.Errorf("cmdOrgsKeys() = %d, want 1", code)
	}
}

// TestTierCAlertInfoUsage pins cmdAlertInfo's argument count check.
func TestTierCAlertInfoUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdAlertInfo(nil); code != 1 {
		t.Errorf("cmdAlertInfo() = %d, want 1", code)
	}
}

// TestTierCAlertRmUsage pins cmdAlertRm's argument count check.
func TestTierCAlertRmUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdAlertRm(nil); code != 1 {
		t.Errorf("cmdAlertRm() = %d, want 1", code)
	}
}

// TestTierCHostAgeInitUsage pins cmdHostAgeInit's argument count check.
func TestTierCHostAgeInitUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdHostAgeInit(nil); code != 1 {
		t.Errorf("cmdHostAgeInit() = %d, want 1", code)
	}
}

// TestTierCHostAgeRotateUsage pins cmdHostAgeRotate's argument count check.
func TestTierCHostAgeRotateUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdHostAgeRotate(nil); code != 1 {
		t.Errorf("cmdHostAgeRotate() = %d, want 1", code)
	}
}

// TestTierCHostAgePrunePreviousUsage pins cmdHostAgePrunePrevious's argument count check.
func TestTierCHostAgePrunePreviousUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdHostAgePrunePrevious(nil); code != 1 {
		t.Errorf("cmdHostAgePrunePrevious() = %d, want 1", code)
	}
}

// TestTierCOrgsKeysInfoUsage pins cmdOrgsKeysInfo's argument count check.
func TestTierCOrgsKeysInfoUsage(t *testing.T) {
	resetJSONOut(t)
	if code := cmdOrgsKeysInfo(nil); code != 1 {
		t.Errorf("cmdOrgsKeysInfo() = %d, want 1", code)
	}
}
