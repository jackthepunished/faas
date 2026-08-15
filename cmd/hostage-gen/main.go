// Command hostage-gen — generate /etc/faas/secrets/host.age on first
// boot when vmmd is not present (DigitalOcean droplet: no /dev/kvm so
// vmmd isn't deployed, but apid still needs the private half to
// unseal TOTP secrets for the IAM-2 MFA handlers /verify +
// /confirm + /recover + /disable). The exact same secretbox helper
// vmmd calls is reused here so the on-disk shape matches
// pkg/secretbox.DefaultHostKeyPath / DefaultHostAgeRecipientPath
// verbatim. Mode 0440 root:faas is set after the write so vmmd (root)
// + apid (faas group) can both read; no other service user can.
//
// Called from deploy/controlplane/bootstrap.sh on the first install
// (the v1 installer — RETIRED 2026-08-15 by issue #911 / PR-1;
// v2 path is PR-X `gregale secrets init`) and on re-bootstrap when the
// perms drifted. Idempotent: a second invocation with the file already
// present refuses to overwrite so a half-applied bootstrap can't
// accidentally rotate the key and invalidate every customer's MFA
// enrollment.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/onebox-faas/faas/pkg/secretbox"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: hostage-gen <priv-path> <pub-path>")
		os.Exit(2)
	}
	privPath, pubPath := os.Args[1], os.Args[2]
	if _, err := os.Stat(privPath); err == nil {
		fmt.Fprintf(os.Stderr, "hostage-gen: %s already exists (refusing to overwrite — delete first if you mean to rotate)\n", privPath)
		os.Exit(1)
	} else if !errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(os.Stderr, "hostage-gen: stat %s: %v\n", privPath, err)
		os.Exit(1)
	}
	// GenerateAndSaveHostKey writes mode 0440 — same as vmmd's
	// first-boot path in cmd/vmmd/main.go:336. The function takes
	// only the identity-path arg; recipient writes are a separate
	// step (mirrors vmmd's loadOrGenerateHostIdentity).
	id, err := secretbox.GenerateAndSaveHostKey(privPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "hostage-gen: generate host key: %v\n", err)
		os.Exit(1)
	}
	if err := os.Chmod(privPath, 0o440); err != nil {
		fmt.Fprintf(os.Stderr, "hostage-gen: chmod %s: %v\n", privPath, err)
		os.Exit(1)
	}
	if err := secretbox.WriteRecipientFile(pubPath, id); err != nil {
		fmt.Fprintf(os.Stderr, "hostage-gen: write recipient %s: %v\n", pubPath, err)
		os.Exit(1)
	}
	fmt.Printf("host.age -> %s (0440)\n", privPath)
	fmt.Printf("host.age.pub -> %s (0444)\n", pubPath)
}
