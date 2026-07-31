// Command faas-nft-render prints the host nftables ruleset to stdout.
//
// Used by `make egress-render` to (re)generate the checked-in artifact at
// `deploy/ansible/roles/nftables/files/policy_nftables.conf`. The artifact is
// what ansible copies onto the host at `make bootstrap` time; this binary is
// the single source of truth.
//
// Per-host rendering (ADR-055). The renderer accepts two optional
// overrides:
//
//   - --public-iface <name>      (env: FAAS_PUBLIC_IFACE)
//   - --masquerade-cidr <cidr>  (env: FAAS_MASQUERADE_CIDR)
//
// When both are unset, the render uses `pkg/netns.DefaultHostPolicy`
// (the EX44 default-local node shape: `eth0` + `10.100.0.0/16`). A
// Hetzner compute node on a different NIC name (e.g. `ens5`) overrides
// via the flag or env so the rendered artifact matches the per-host
// deployment. `make egress-render` and the new `make egress-render-cross-check`
// target exercise both paths.
//
// Flag precedence: explicit flag > env var > `DefaultHostPolicy`.
//
// stdout, exit 0 only. Failure to render panics — that's a build-time bug,
// not a runtime concern.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/onebox-faas/faas/pkg/netns"
)

func main() {
	// Default-empty string + explicit fallback inside render via
	// `DefaultHostPolicy`: an empty flag/env means "use the Go
	// default", which is the source-of-truth for the EX44 shape.
	// We don't fail-open on garbage here because the renderer
	// itself panics on empty required fields (policy.go:111-120).
	publicIface := flag.String("public-iface", "",
		"host's outward-facing NIC (e.g. eth0, ens5). Defaults to pkg/netns.DefaultHostPolicy.PublicIface (=eth0). Env: FAAS_PUBLIC_IFACE.")
	masqueradeCIDR := flag.String("masquerade-cidr", "",
		"source-address CIDR for postrouting MASQUERADE (e.g. 10.100.0.0/16). Defaults to pkg/netns.DefaultHostPolicy.MasqueradeCIDR (=10.100.0.0/16). Env: FAAS_MASQUERADE_CIDR.")
	flag.Parse()

	policy := netns.DefaultHostPolicy
	if iface := pickValue(*publicIface, "FAAS_PUBLIC_IFACE"); iface != "" {
		policy.PublicIface = iface
	}
	if cidr := pickValue(*masqueradeCIDR, "FAAS_MASQUERADE_CIDR"); cidr != "" {
		policy.MasqueradeCIDR = cidr
	}

	if _, err := os.Stdout.WriteString(policy.Render()); err != nil {
		// Render() never returns an error today; this branch is the future
		// hook if the renderer ever returns one.
		fmt.Fprintln(os.Stderr, "faas-nft-render: write stdout:", err)
		os.Exit(1)
	}
}

// pickValue returns the explicit flag value if non-empty, otherwise
// the env var (which may itself be empty). The renderer's panic-on-
// empty contract (policy.go:111-120) means an empty result here
// falls back to the package default downstream.
func pickValue(flagVal, envName string) string {
	if flagVal != "" {
		return flagVal
	}
	return os.Getenv(envName)
}
