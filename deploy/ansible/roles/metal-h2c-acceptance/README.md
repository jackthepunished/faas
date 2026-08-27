# metal-h2c-acceptance

G19.3 / ADR-127 Layer 12 provisions the opt-in five-app fixture contract and
the harness for the §14 M8 row 5 H2C acceptance gates.

The role is intentionally absent from `deploy/ansible/bootstrap.yml`. It is
for a designated Linux x86_64 KVM host only and refuses to run unless
`mha_enabled=true` is supplied. Normal control-plane and compute-node
bootstraps therefore remain unchanged.

## Run

Create a private acceptance inventory and variable file outside the repository
containing the five app URLs, the provider-specific provisioning command, and
the operator-approved framing/rollback assertions. Then run:

```sh
ansible-playbook \
  -i deploy/ansible/inventory/metal-acceptance.ini \
  deploy/ansible/metal-h2c-acceptance.yml \
  -e mha_enabled=true \
  -e mha_run_acceptance=true \
  -e @/etc/faas/metal-h2c-acceptance/private-vars.yml
```

`mha_provision_fixtures` is false by default. When enabled, the role requires
an explicit `mha_fixture_provision_command`, `mha_api_base`, and
`mha_api_token`; the token is rendered with `no_log`, stored root-owned with
mode `0640`, and is never committed or printed by the harness.

The default service is installed but disabled and stopped. An operator can
run the repeatable harness with:

```sh
sudo systemctl start faas-metal-h2c-acceptance.service
```

The five named Ansible tasks map directly to the M8 row 5 gates: HTTP/1.1
baseline, H2 prior-knowledge, gRPC unary + streaming trailers, surgical h1
rollback, and wholesale v1 rollback. The two rollback commands must include
their own safe service mutation and restoration; the harness always restores a
successful switch before returning.
