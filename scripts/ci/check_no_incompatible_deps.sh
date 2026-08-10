#!/usr/bin/env bash
# check_no_incompatible_deps.sh — fail a PR that pulls a +incompatible
# version of a *direct* go.mod require.
#
# Why this exists
# ---------------
# A `+incompatible` suffix on a module version means Go is fetching it
# at a non-semver import path (the module is either pre-modules or
# refuses to adopt a /vN major suffix). It's a tripwire: the module
# author is signalling "I will not promise anything about backwards
# compatibility" — and Go will never auto-bump past it.
#
# Today this flags exactly one direct dep:
#   github.com/stripe/stripe-go v70.15.0+incompatible
# (go.mod:25). v70 pre-dates the modern lookup_key primitive on
# PlanParams (v74+). A future major bump will require touching every
# call site that uses lookup-by-lookup_key — fine, but the bump must
# happen deliberately, not because Dependabot decided to. The gate
# below makes the hit visible at PR time, and prevents a second
# +incompatible dep from ever slipping in unannounced.
#
# Indirect deps are intentionally allowed: a transitive consumer may
# pin a +incompatible version and we cannot upgrade it without
# bumping the consumer, which is out of scope for a "go.mod hygiene"
# gate. Only direct requires are load-bearing for the release
# invariant (the only path to remove the gate's failure is a manual
# major-version PR, which is the right ceremony).
#
# Convention: mirrors scripts/ci/check_migration_slots.sh — pure
# bash, no `pip install` / no extra runner setup, no Go-built binary.

set -euo pipefail

# `go list -m -f '{{if not .Indirect}}{{.Path}} {{.Version}}{{end}}'`
# emits the (path, version) tuple for direct deps only. `grep` against
# `+incompatible` matches the suffix on the version. The `|| true`
# ensures grep returns 0 when there are no matches (it doesn't change
# the script's overall exit code thanks to `set -e` not triggering on
# commands followed by `||`).
hits=$(go list -m -f '{{if not .Indirect}}{{.Path}} {{.Version}}{{end}}' all \
       | grep '+incompatible' || true)

if [ -n "$hits" ]; then
  echo "::error:: direct go.mod require uses a +incompatible version:" >&2
  echo "$hits" >&2
  echo "::error:: +incompatible means the module is at a non-semver path and Go will never auto-bump it. The release gate is: no direct require may carry this suffix. Open a follow-up issue to upgrade the module, and re-run this gate until it's clean." >&2
  exit 1
fi
