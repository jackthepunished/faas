// cmd/githubd/caps.go — DEPLOY-1 / ADR-075 cap declaration.
//
// githubd is the GitHub auth + webhook daemon (PR-B, ADR-052).
// It needs NO caps. Every filesystem op (ReadWritePaths=
// /run/faas + /var/lib/faas/githubd) goes through the
// User=faas-githubd + NoNewPrivileges=yes envelope, which
// is configured by the systemd unit and does not require
// CAP_SYS_ADMIN. The empty declaration matches the unit file.
//
// A future PR that adds a real privilege requirement to
// githubd MUST route through vmmd instead — the lint rule
// blocks pkg/vmmdgrpc / pkg/vmmdmount imports outside
// cmd/vmmd/ + pkg/vmmd/. githubd auth/binding/repo-list
// do not need any cap; webhook delivery is over the public
// internet (already egress-allowed by the nftables set
// ADR-052 ships).
package main

import "github.com/onebox-faas/faas/pkg/capdecl"

var capsDecl = capdecl.Declaration{
	Allow: nil,
	Deny:  nil,
}
