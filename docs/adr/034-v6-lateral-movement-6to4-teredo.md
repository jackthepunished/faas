# ADR-034 · IPv6 lateral-movement: 6to4 + Teredo deny

- **Status:** accepted
- **Date:** 2026-07-25
- **Decision:** Add `2002::/16` (6to4) and `2001::/32` (Teredo)
  to the IPv6 egress denylist shared by the per-netns renderer,
  the host renderer, and the OCI puller
  (`pkg/netns.NewDefaultDenySet()`). No wire / DB / plan-gate
  change. Documented as the v6 lateral-movement counterpart of
  ADR-023's ULA / link-local / multicast deny.

## Context

ADR-023 (merged earlier in tier-1) extended spec §11's IPv4-only
denylist to IPv6 by adding `fe80::/10` (link-local), `fc00::/7`
(ULA), `ff00::/8` (multicast), `::1/128` (loopback), and `::/128`
(unspecified) to the per-netns forward chain + the host nftables
ruleset + the OCI puller's user-space check. The ADR left two
adjacent IPv6 ranges unaddressed because their security
significance is tunnel-specific:

- **`2002::/16` — 6to4** ([RFC3056](https://datatracker.ietf.org/doc/html/rfc3056)):
  anycast relays carry IPv6 traffic over IPv4. A guest can
  encapsulate an IPv6 packet whose destination is a public v6
  address inside an IPv4 packet addressed to a public 6to4 relay;
  the relay decapsulates and forwards. The wire at the IPv4 layer
  is "guest → relay → IPv6 destination", but the relay-side
  security posture is whatever the relay operator runs — not our
  firewall. Worse: a 6to4 endpoint address embeds an IPv4 address
  (`2002:V4ADDR::/48`), so `2002:0a00:0001::/48` is *literally*
  `10.0.0.1` rendered as a v6 prefix. A guest that forwards into
  6to4 effectively re-uses the v6 route as a back-channel into
  the RFC1918 ranges the v4 deny list already blocks.
- **`2001::/32` — Teredo** ([RFC4380](https://datatracker.ietf.org/doc/html/rfc4380)):
  same problem class as 6to4 (IPv6-over-UDP/3544), different
  prefix. Same lateral-movement risk profile. Tunnel server
  selection is done via DNS to a `teredo.ipv6.microsoft.com` (now
  deprecated, but the prefix is still allocated) — modern guests
  won't have a usable Teredo relay, but a maliciously crafted
  inner packet can still reach a relay run by the attacker.

These two ranges were the documented gap in ADR-023
("spec §11 is IPv4-only; fe80::/10 + ULA + multicast unblocked").
ADR-031 + ADR-033 added the per-app allowlist (deny > allow on
overlap) but did not address the underlying denylist. PR-A in the
tier-2 follow-up plan ships the fix.

## Threat model

Both 6to4 and Teredo are *IPv6 transition mechanisms* — they let
a host speak IPv6 when only an IPv4 path is available. The
security model we're building on is "the per-netns forward chain
sees everything the guest sends out". That guarantee falls apart
the moment a guest can pick a different wire format to escape
the rule:

**Path 1 — v6-in-IPv4 to reach a v4 destination indirectly.**
The Per-App allowlist (ADR-031) is partitioned by
`prefix.Addr().Is4()`. A CIDR like `8.8.8.0/24` is recognised as
v4 and emits the `ip daddr { 8.8.8.0/24 } accept` rule on the
`ip faas forward` chain. A guest that wants to reach `8.8.8.8`
but is blocked by the allowlist can package the v4 packet as
`2002:0808:0808::` (the 6to4-encoded equivalent of `8.8.8.8`) and
send it as v6. The allowlist chain has no `accept` rule for
`2002:0808:0808::/48` (the prefix is not in the customer's list
because the customer only knows v4), so the packet would normally
drop. **But** — the packet does not match the v6 deny set either
(because `2002::/16` is not in the v6 deny list today). Result:
the packet lands on the host, the host's 6to4 anycast relay
decapsulates it, and the inner v4 packet is forwarded to
`8.8.8.8`. The firewall saw *no* allow + *no* deny = the packet
escaped.

**Path 2 — v6-in-IPv4 to reach an RFC1918 destination.**
Same mechanism, worse target. A 6to4-encoded `2002:0a00:0001::`
decapsulates to `10.0.0.1`. The v4 deny list blocks the direct
v4 attempt, but the v6 path lets the guest bypass the v4 deny
and reach the internal network *through the relay*. The relay
is on the public internet, so the inner packet emerges from a
public IP — which is exactly the lateral-movement vector the v4
deny list is meant to close.

**Path 3 — Teredo-as-UDP-app.**
Teredo tunnels over UDP/3544. A guest cannot trivially establish
a Teredo tunnel without a server handshake (the protocol is
stateful), but the *handshake packets* themselves are UDP/3544
that the host bridge will happily forward. A guest that resolves
`teredo.ipv6.microsoft.com` (or any attacker-controlled server)
can complete the handshake and then send arbitrary v6 over the
established tunnel. The v6 deny list today doesn't include
`2001::/32`, so the tunnel's traffic flows.

**Why these are not caught by the per-app allowlist.**
The allowlist is opt-in per-app. A customer's allowlist is
typically `["8.8.8.0/24", "1.1.1.0/24"]` — the addresses the
customer's code needs to reach. Anything not in the list is
implicitly denied. The implicit deny is the *v4 allowlist chain*
(`ip daddr { … } accept` followed by `policy accept`) and the
*v6 deny chain* (`ip6 daddr { fe80::/10, fc00::/7, … } drop`
followed by `policy accept`). The v6 deny chain is
over-permissive today: it drops link-local, ULA, multicast, and
loopback, but it permits public v6 traffic. That permissiveness
is what makes Path 1 (and Path 2 / 3) work.

**Closing the gap.** Adding `2002::/16` and `2001::/32` to the
v6 deny chain forces the guest to encode the v4 destination as
v6 *and* have the v6 form pass the chain. Both 6to4 and Teredo
prefixes are now drops. The customer who legitimately needs
6to4 (none have asked) has no path; the customer who
maliciously reaches for the tunnel has no path. The fix is
symmetric to the v4 deny list's posture and matches the
"no path that bypasses the egress allowlist" first principle.

## Decision

`pkg/netns.NewDefaultDenySet()` adds two new v6 entries:

```
{FamilyV6, netip.MustParsePrefix("2002::/16"), "ADR-034", "6to4 (RFC3056); tunnels IPv6 over IPv4 — lateral movement into 10/8 etc."},
{FamilyV6, netip.MustParsePrefix("2001::/32"), "ADR-034", "Teredo (RFC4380); tunnels IPv6 over UDP/3544 — same lateral-movement risk as 6to4"},
```

The two entries propagate to all three renderers (per-netns, host,
oci) without further edits — that is the contract of the typed
`DenySet` introduced in PR-A. Both renderers were already emitting
the v6 deny list verbatim from `V6CommaSet()`; only the source list
grew.

No DB migration. No proto change. No plan-gate change. No new deny
semantics beyond 6to4 + Teredo. The conntrack cap value (4096) is
unchanged.

## Consequences

- `pkg/netns/config_test.go::TestNftCommandsEnforceEgressPolicy`
  is updated to assert `2002::/16` and `2001::/32` are present in
  the rendered per-netns v6 deny argv. Pre-PR-A the assertion only
  pinned the five pre-existing v6 entries; missing 6to4/Teredo
  would have silently regressed.
- `pkg/netns/policy_test.go::TestHostPolicyRenderDeniesIPv6LinkLocalAndULA`
  iterates over `DefaultHostPolicy.DenySet.V6DenyCIDRs` (PR-A
  refactor — the typed struct is the regression source), so the
  same assertion covers the host side.
- `pkg/oci/egress.go::deniedCIDRv6` is built from
  `NewDefaultDenySet()` (PR-A refactor); the OCI test
  `TestIPAllowed_DeniedRanges` doesn't probe 6to4 / Teredo
  addresses today (the puller is an HTTP client, not a guest
  kernel) but adding the entries is free defence-in-depth and
  matches the "deny by default" contract.
- The `denylist.md` operator-facing artifact (see Cross-reference)
  lists every deny line with source ADR + rationale + test pin.

## Rejected alternatives

- **Allow 6to4 / Teredo for Pro+** — would carve a tunnel escape
  for paying tenants. The lateral-movement profile is the same
  for all plans (a guest reaching `2002:0a00:0001::/48` is
  reaching `10.0.0.0/8` from the host's perspective). No operator
  has asked for it; no spec section contemplates it; rejected on
  first principles.
- **Block at the nft level with `meta nfproto ipv6 ip6 daddr`
  wrappers** — would be needed today because the v6 deny is in a
  separate `ip6 faas` chain. ADR-023 explicitly rejected the
  `meta nfproto` wrapper (the table is `inet faas` at the host
  layer, `ip6 faas` at the per-netns layer — both implicit). The
  same-shape deny set works without changing the family split.
- **Defer to "if a customer complaint comes in"** — the gap is
  documented (ADR-023 §"rejected alternatives") and the fix is
  one-line in `NewDefaultDenySet()`. Deferring leaves a known
  lateral-movement channel open for the lifetime of tier-1.

## Cross-reference

- `pkg/netns/denylist.go` — `NewDefaultDenySet()`, the typed
  single source of truth for all deny lines. Two new entries
  added by this ADR (with `SourceADR = "ADR-034"` for traceability).
- `pkg/netns/config.go::denyV6Set` — per-netns renderer reads the
  v6 deny list from `DenySet.V6CommaSet()`. Default falls back to
  `NewDefaultDenySet()`.
- `pkg/netns/policy.go::HostPolicy.DenySet` — host renderer reads
  the same struct. `DefaultHostPolicy` constructs from
  `NewDefaultDenySet()`.
- `pkg/oci/egress.go::deniedCIDRv6` — OCI puller's user-space
  check reads from the same struct.
- `docs/adr/034-v6-lateral-movement-6to4-teredo.md` — this ADR.
- `docs/adr/031-app-egress-allowlist.md` + `033-app-egress-allowlist-v6.md`
  — the per-app allowlist ADRs that consume the same DenySet and
  pin deny > allow on overlap.
- `docs/adr/023-...` — the IPv6 family split ADR this slice extends.