package releaseinstall

import (
	"strings"
	"testing"
)

func TestValidateBundleInputsDoesNotRequireGeneratedFields(t *testing.T) {
	gitSHA := "0123456789abcdef0123456789abcdef01234567"
	manifestHash := "sha256:" + strings.Repeat("a", 64)
	if err := ValidateBundleInputs(gitSHA, manifestHash); err != nil {
		t.Fatalf("ValidateBundleInputs() = %v, want nil", err)
	}
}
