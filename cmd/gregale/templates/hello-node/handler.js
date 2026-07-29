// Hello-world handler for gregale.
//
// Listens on :8080 (the port guest-init forwards to). Returns a tiny
// JSON greeting so a curl check is enough to verify a deploy landed.
// The handler also reads a few env vars (set via `gregale env push`) to
// show how secrets surface inside the guest — values are NEVER
// returned in the response, only the key names, so logging them
// doesn't leak plaintext.

import express from "express";

const app = express();
const port = process.env.PORT || 8080;

app.get("/", (_req, res) => {
  res.json({
    message: "hello from gregale",
    node: process.version,
    secretKeys: Object.keys(process.env)
      .filter((k) => !k.startsWith("FAAS_") && k !== "PATH" && k !== "HOME" && k !== "NODE_VERSION")
      .sort(),
  });
});

app.get("/healthz", (_req, res) => {
  res.status(200).json({ ok: true });
});

app.listen(port, () => {
  console.log(`hello-gregale listening on :${port}`);
});