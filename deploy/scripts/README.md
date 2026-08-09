# deploy/scripts

Operator-runnable scripts invoked by the bootstrap playbook and the
ops runbooks. Each script is intentionally named after the daemons
or milestone it supports, not the brand (`faas-m8-restore-drill.sh`,
`faas-m75-smoke.sh`, `verify-secrets.sh`).

The `faas-*` script prefixes are a **filesystem-identity carryover**.
The Gregale docs-rebrand pass intentionally does not rename the
scripts in the same PR — a follow-up code-identity pass renames the
faas-*.service systemd units, the deploy script filenames, and the
`/etc/faas/` and `/var/lib/faas/` paths together, since they're
referenced across bootstrap, ansible, and operator runbooks in a way
that requires an atomic rename. Until that pass lands, the script
names stay stable.
