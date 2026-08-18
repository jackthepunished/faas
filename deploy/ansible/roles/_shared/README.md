# _shared ansible role

File-only role; has no `tasks/main.yml` and never appears in a play. Holds
artifacts that the terminal `<role>_service/` roles include via `src:` traversal
so the deploy tree stays DRY without inventing a synthetic role to anchor the
artifact.

Conventions:

- `templates/*.j2` — templates every service role can render via
  `src: ../../_shared/templates/<name>.j2`. The `../../` walks back from the
  role's `templates/` to the `roles/` directory, then into `_shared/`.
- `files/*` — files every service role can `copy:` via the same `src:` traversal.

Current contents:

- `templates/99-faas-node-name.conf.j2` — the `FAAS_NODE_NAME` systemd drop-in
  installed alongside every daemon's unit (one per role). The body is
  intentionally identical across roles; per-role rationale (which daemon reads
  the env, what it does with it) lives in the consuming role's README.md.

To add a new shared artifact, drop it here and update each consuming role's
`tasks/main.yml` to point at the new path. Keep the comment at the top of every
shared template short — the per-role context belongs in the consuming role's
README.md, not in the canonical body.
