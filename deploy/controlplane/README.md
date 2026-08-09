# Deploy FaaS Control Plane (Standalone Host)

Deploys the non-KVM parts of the platform on a standalone Cloud VM (GCP, DigitalOcean, Hetzner Cloud, etc.) for development, testing, and staging. `vmmd` and `builderd` are **not deployed** (no `/dev/kvm` on standard non-nested VMs) — VM lifecycle operations return errors, which is expected. Some Gregale features (TLS DNS-01, Hetzner DNS zone updates) reference specific providers and need configuration changes when run elsewhere.

## What runs

| Daemon | Port / Socket | Purpose |
|--------|---------------|---------|
| `gatewayd` | `:8080` (public HTTP) | Edge proxy, routing, rate limits |
| `apid` | `127.0.0.1:8081` | REST API, auth, quotas |
| `schedd` | `/run/faas/schedd.sock` | Scheduler, instance state machine |
| `imaged` | (event-driven) | OCI pull + ext4 layer builder |
| `meterd` | (timer loops) | Metering, quota, billing |
| `githubd` | `127.0.0.1:8083` + socket | GitHub App OAuth, webhooks |
| Postgres 15/16 | Unix socket | Database |

## Base staging layout (imaged)

`imaged` stages function runtime bases on first start. The controller
(`deployctl`) creates and cleans two staging roots before each deploy:

- `/srv/fc/base-staging` — **disk-backed** full OCI layer extraction
  (`FAAS_BASE_EXTRACT_ROOT`). The unpacked tree can be gigabytes (a Go
  toolchain base is ~850 MB); it must NOT live on the 2 GiB `/dev/shm`
  tmpfs or imaged crash-loops with `no space left on device`.
- `/dev/shm/faas-base-staging` — **tmpfs** staging for the parent-ref
  overlay upper/work dirs (`FAAS_BASE_STAGING_ROOT`). The kernel
  rejects overlay mounts whose upper fs doesn't support `tmpfile`, and
  host `/tmp` is ext4, so this one stays on tmpfs (ADR-053).

`deployctl`'s `Preflight`/`Activate` also ensures `/srv/fc/scans`
exists (base scan sidecars) and the CD workflow provisions the
checksum-pinned `grype`/`syft` binaries into `/usr/local/bin` — imaged
fails-closed (`CRITICAL=9999`) when they're missing, which makes vmmd
refuse every boot.

## Quick start

### 1. Create a VM / Droplet

- **Image:** Ubuntu 24.04 LTS
- **Plan:** 4 GB RAM / 2 vCPU — the control plane is lightweight
- **Region:** Pick closest to you
- **Auth:** Your SSH key

### 2. Bootstrap

SSH into the server and run:

```bash
# Option A: From the repo (if you've cloned it on the host)
sudo bash deploy/controlplane/bootstrap.sh

# Option B: One-liner from GitHub
curl -sSf https://raw.githubusercontent.com/poyrazK/faas/main/deploy/controlplane/bootstrap.sh | sudo bash
```

The script:
- Installs Postgres, Go, system deps
- Creates system users (`faas`, `faas-apid`, `faas-schedd`, …)
- Clones the repo to `/opt/faas/src`, builds all binaries to `/opt/faas/bin`
- Drops TOML configs + `sealed.env` to `/etc/faas/`
- Installs systemd units and slices
- Runs DB migrations
- Generates an SSH deploy key for GitHub Actions CD
- Starts all services

### 3. Verify

```bash
# From the host
curl http://127.0.0.1:8080/healthz                              # gatewayd → apid
curl http://127.0.0.1:9090/healthz                              # gatewayd control
TOKEN=$(awk -F= '/^FAAS_DEV_TOKEN=/{print $2; exit}' /root/faas-dev-credentials.txt | awk '{print $1}')
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/v1/account

# From your laptop
curl http://<HOST_IP>:8080/status                               # status page
open http://<HOST_IP>:8080/dashboard/                           # dashboard UI
```

### 4. Set up GitHub Actions CD

After `bootstrap.sh` finishes, it has written a deploy SSH key to
`/opt/faas/.ssh/deploy_ed25519` on the host — it is **never printed to
stdout** (which systemd-journal and agent logs both capture). Retrieve it:

```bash
# From your laptop
scp root@<HOST_IP>:/opt/faas/.ssh/deploy_ed25519 ./cp_ssh_key
```

Add these to your GitHub repo (Settings → Secrets and variables → Actions):

| Secret | Value |
|--------|-------|
| `CP_SSH_KEY` (or `DO_SSH_KEY`) | The ed25519 private key you just scp'd |
| `CP_HOST` (or `DO_HOST`) | Control Plane host public IPv4 address |

Now every push to `main` that passes CI will auto-deploy to the Droplet.

> 🔁 **Rotate DO_SSH_KEY if it was ever printed in logs.** The PR before
> issue #85 used `cat` on the private key during bootstrap, so anything that
> captured bootstrap output (CI logs, screen shares, agent transcripts)
> should treat the prior key as exposed. See
> [Post-merge operator actions](#post-merge-operator-actions) below for the
> rotation steps.

### 5. Set up GitHub App (optional)

1. Go to https://github.com/settings/apps → **New GitHub App**
2. Configure:
   - **Homepage URL:** `http://<DROPLET_IP>:8080`
   - **Callback URL:** `http://<DROPLET_IP>:8080/oauth/github/callback`
   - **Webhook URL:** `http://<DROPLET_IP>:8080/webhooks/github`
   - **Webhook secret:** Generate one, save it
   - **Permissions:** Contents (read), Checks (write), Metadata (read)
   - **Events:** Push, Check suite
3. After creation, note the **App ID** and download the **private key** (.pem)
4. On the Droplet:
   ```bash
   # Copy the .pem file
   sudo cp github-app.pem /etc/faas/secrets/github-app.pem
   sudo chmod 0400 /etc/faas/secrets/github-app.pem
   sudo chown root:faas /etc/faas/secrets/github-app.pem

   # Add to sealed.env
   sudo tee -a /etc/faas/sealed.env <<EOF
   FAAS_GITHUB_APP_ID=<your-app-id>
   FAAS_GITHUB_APP_KEY_PATH=/etc/faas/secrets/github-app.pem
   FAAS_GITHUB_WEBHOOK_SECRET=<your-webhook-secret>
   EOF

   # Restart githubd
   sudo systemctl restart faas-githubd
   ```

## Manual redeploy

Manual deployment uses the same immutable release controller as GitHub Actions:

```bash
sudo bash /opt/faas/src/deploy/controlplane/deploy.sh <release-id>
```

The release must already exist under `/opt/faas/releases/<release-id>/` and pass
manifest verification. The controller acquires the host deployment lock, runs
preflight and migrations, activates the release atomically, waits for readiness,
and rolls back to the previous verified release on failure.

The host no longer builds from `/opt/faas/src`; CI and manual operations use the
same release artifact and activation path.

## OAuth sign-in (optional)

apid boots in three modes with respect to OAuth sign-in
(issue #419 / ADR-046):

| `GOOGLE_*` envs | `GITHUB_*` envs | Outcome |
| --- | --- | --- |
| both unset | both unset | OAuth disabled — `/v1/auth/google` and `/v1/auth/github` return 503 `oauth_provider_unavailable`; `/login` hides the buttons |
| both set per provider | both set per provider | OAuth enabled for that provider — `/v1/auth/<provider>` 302s to the consent screen |
| exactly one of `(ID, SECRET)` set | (same shape) | apid refuses to boot with a wrapped error naming which vars diverge |

To enable Google sign-in, set:

```bash
GOOGLE_CLIENT_ID=<oauth-client-id>.apps.googleusercontent.com
GOOGLE_CLIENT_SECRET=<oauth-client-secret>
# Optional: override the redirect_uri (default = host-derived https://<box>/v1/auth/google/callback)
# GOOGLE_REDIRECT_URI=
```

To enable GitHub sign-in, set:

```bash
GITHUB_CLIENT_ID=<oauth-app-client-id>
GITHUB_CLIENT_SECRET=<oauth-app-client-secret>
# Optional: override the redirect_uri (default = host-derived https://<box>/v1/auth/github/callback)
# GITHUB_REDIRECT_URI=
```

After editing `/etc/faas/sealed.env`, restart apid: `sudo systemctl restart faas-apid`.

For the Paddle-specific cutover (flipping `FAAS_BILLING_PROVIDER` from `stripe` to `paddle`, rotating `FAAS_PADDLE_API_KEY`, hardening the webhook tolerance), see the operator runbook:

- [`docs/ops/billing-provider-switch.md`](../../docs/ops/billing-provider-switch.md) — canonical Stripe ↔ Paddle cutover procedure (PR-P4).
- [`docs/ops/secrets-rotation.md`](../../docs/ops/secrets-rotation.md) — cadence + per-secret rotation steps.
- [`make verify-secrets`](../../Makefile) — static + content gate against `/etc/faas/sealed.env`; safe to run on a control-plane node.

Verify with:

```bash
# Should redirect to https://accounts.google.com/o/oauth2/v2/auth?... when configured.
curl -i https://<box>/v1/auth/google
# Should report the per-provider enabled flag.
curl -b cookies.txt https://<box>/v1/auth/capabilities
```

## Logs & debugging

```bash
# Follow all FaaS service logs
journalctl -u 'faas-*' -f

# Single service
journalctl -u faas-apid -n 50

# Service status
systemctl status faas-apid faas-schedd faas-gatewayd faas-imaged faas-meterd faas-githubd

# Postgres
sudo -u postgres psql faas
```

## What does NOT work (expected)

- **`faas deploy`** — goes through apid/imaged but fails at snapshot/wake (no vmmd)
- **Wake requests** — schedd can't dial vmmd, returns 503
- **Builds** — builderd is not deployed (needs vmmd for builder VMs)
- **Snapshot/restore latency** — no Firecracker on DO

These require a KVM-capable host (a Hetzner Cloud CCX instance, a bare-metal x86_64 server, or a similar KVM-enabled provider).

## Production / TLS

The default bootstrap lands on **plain HTTP over `<DROPLET_IP>.nip.io`** —
fine for dev, but a non-resolving DNS suffix and an HTTP-only surface break
OAuth callbacks, GitHub webhooks, and Stripe webhooks (all of which require
HTTPS). To enable TLS:

1. **Point a real domain at the Droplet.** Buy / repoint a domain
   (e.g. `apps.gregale.dev`), set an A record to the Droplet's public IPv4,
   and (optional) set a wildcard `*.apps.gregale.dev` CNAME.
2. **Update `gatewayd.toml`:**
   ```toml
   apps_domain = "apps.gregale.dev"

   [tls]
   disabled = false
   wildcard_cert_domain = "example.com"
   hetzner_dns_api_token_path = "/etc/faas/secrets/hetzner-dns-token"
   hetzner_zone = "example.com"
   storage_dir = "/var/lib/faas/certs"
   contact_email = "ops@example.com"
   ```
   `gatewayd` will then request a Let's Encrypt wildcard via the Hetzner
   DNS-01 challenge on first start. (Drop the `wildcard_cert_domain` +
   Hetzner knobs if you'd rather use the HTTP-01 challenge; for that you
   also need port 80 open and the `OnDemandHTTP01Allowlist` populated.)
3. **Open 443 (and 80) in UFW** — the bootstrap only opens 22/tcp + 8080/tcp:
   ```bash
   sudo ufw allow 443/tcp   # public TLS
   sudo ufw allow 80/tcp    # ACME http-01 challenges (CertMagic uses it)
   ```
4. **Regenerate the GitHub App** (or edit it) with HTTPS URLs:
   - Homepage: `https://apps.gregale.dev`
   - Callback: `https://apps.gregale.dev/oauth/github/callback`
   - Webhook:  `https://apps.gregale.dev/webhooks/github`
5. **Restart gatewayd** so it picks up the new TOML:
   ```bash
   sudo systemctl restart faas-gatewayd
   ```
6. (Optional) **Add a CDN / WAF** in front for DDoS protection. This guide
   intentionally leaves CDN choice to the operator.

⚠ The droplet in question is exposed to the public internet. Pinning a real
domain to it without TLS is a credential-leak / phishing risk — enable
TLS before sharing the URL with anyone beyond the operator.

## Post-merge operator actions

These are one-time steps the operator should run after the PR that introduces
issue #85's hardenings merges to main and CD deploys. They are listed here
because the PR cannot perform them automatically (they require either new
humans or live coordination).

### Rotate `DO_SSH_KEY`

The deploy SSH key was previously printed to stdout during bootstrap. If any
bootstrap output was captured (CI logs, terminal scrollback, agent
transcripts), treat that key as exposed and rotate:

```bash
# 1. On the droplet, generate a new deploy key.
ssh root@<DROPLET_IP>
ssh-keygen -t ed25519 -N '' -C 'faas-cd-deploy-v2' -f /opt/faas/.ssh/deploy_ed25519.new
cat /opt/faas/.ssh/deploy_ed25519.new.pub >> /root/.ssh/authorized_keys
# Atomically swap (overwrites the old private key on disk).
mv /opt/faas/.ssh/deploy_ed25519.new      /opt/faas/.ssh/deploy_ed25519
mv /opt/faas/.ssh/deploy_ed25519.new.pub  /opt/faas/.ssh/deploy_ed25519.pub
chmod 0600 /opt/faas/.ssh/deploy_ed25519

# 2. From your laptop, download the new private key.
scp root@<DROPLET_IP>:/opt/faas/.ssh/deploy_ed25519 ./do_ssh_key

# 3. Update the GitHub DO_SSH_KEY secret with the new contents (Settings
#    → Secrets and variables → Actions → DO_SSH_KEY → Update).

# 4. Drop the old key from any runner SSH cache.
ssh-keyscan -R <DROPLET_IP> 2>/dev/null || true
```

The old key remains authorized on the droplet until you remove it from
`/root/.ssh/authorized_keys` by hand. **This step is required, not optional**:
once a private key has been printed in any log, treat it as exposed and
yank the corresponding public key from `authorized_keys`. Leaving an old
key authorized means anyone with the leaked private key still has shell:

```bash
# 5. On the droplet, remove the old public key from authorized_keys.
ssh root@<DROPLET_IP>
# Find the line matching the old comment ('faas-cd-deploy') and delete it.
# Don't blanket-delete authorized_keys — other keys may be in there.
ssh-keygen -y -f /opt/faas/.ssh/deploy_ed25519.pub   # confirm the new pubkey
grep -n 'faas-cd-deploy' /root/.ssh/authorized_keys  # locate old entries
# Manually edit /root/.ssh/authorized_keys and remove every line whose
# comment starts with 'faas-cd-deploy' (excluding the one you just rotated
# in). Then:
systemctl reload sshd   # or: service ssh reload
```

### Rotate the dev API token

`FAAS_DEV_TOKEN` was the public `fp_live_8888…` placeholder before
PR 2 of #85. After the bootstrap runs once post-merge, the file
`/root/faas-dev-credentials.txt` holds a fresh `fp_live_<48-hex>`. If
you want to rotate without re-running bootstrap:

```bash
ssh root@<DROPLET_IP>
NEW_TOKEN="fp_live_$(openssl rand -hex 24)"
sudo sed -i "s/^FAAS_DEV_TOKEN=.*/FAAS_DEV_TOKEN=${NEW_TOKEN}/" /etc/faas/sealed.env
# Mirror the format bootstrap.sh writes — overwrite the file with the
# new token + a rotation timestamp so future readers can tell when it
# changed.
sudo tee /root/faas-dev-credentials.txt > /dev/null <<EOF
# FaaS dev credentials — mode 0600, root:root. Do NOT commit.
# Authoritative value lives in /etc/faas/sealed.env.
FAAS_DEV_TOKEN=${NEW_TOKEN}  # rotated $(date -u +%Y-%m-%dT%H:%M:%SZ)
EOF
sudo chmod 0600 /root/faas-dev-credentials.txt
sudo systemctl restart faas-apid
```


## File layout on the Droplet

```
/opt/faas/bin/          compiled daemons (apid, schedd, …)
/opt/faas/src/          git checkout of the repo
/etc/faas/              TOML configs + sealed.env
/etc/faas/secrets/      GitHub App key, age key (mode 0400)
/run/faas/              Unix sockets (runtime, tmpfs)
/var/log/faas/          daemon logs
/var/spool/faas/        async work queue
/srv/fc/snap/           snapshot storage (mostly empty on DO)
```
