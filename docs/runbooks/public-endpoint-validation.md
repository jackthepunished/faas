# Public endpoint validation

`gatewayd-public` intentionally listens on loopback. Caddy (and, where used,
the CDN) owns the public TLS edge and proxies to `http://127.0.0.1:8080`.
Therefore a local `curl http://127.0.0.1:8080` is not proof that the public
release endpoint works.

Run this from a machine that can reach the public DNS name:

```sh
cd /opt/faas
PUBLIC_ENDPOINT_URL=https://apps.example.com \
PUBLIC_HTTP_URL=http://apps.example.com \
make public-endpoint-check
```

The check verifies all of the following:

- the URL is HTTPS and the certificate/hostname validates through curl;
- the configured probe path (default `/status`) returns 2xx;
- `Strict-Transport-Security` is present with `max-age >= 31536000`;
- optional HTTP traffic redirects to HTTPS.

For a site whose authenticated status page is not public, choose another
unauthenticated 2xx path:

```sh
PUBLIC_ENDPOINT_URL=https://apps.example.com \
PUBLIC_ENDPOINT_PATH=/ \
make public-endpoint-check
```

On the control-plane host, separately verify the private service and Caddy
upstream:

```sh
systemctl is-active --quiet faas-gatewayd-public
curl --fail --silent http://127.0.0.1:9092/readyz
curl --fail --silent -o /dev/null http://127.0.0.1:8080/
```

A public pass is required before announcing a release. The local checks are
diagnostics only; they do not replace the external HTTPS check.
