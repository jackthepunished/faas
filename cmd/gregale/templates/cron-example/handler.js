// cron-example handler for gregale.
//
// Designed to be hit by `gregale crons add` so you can verify your
// schedule fires. Returns the UTC timestamp + a per-invocation UUID
// (the guest RNG is reseeded post-restore, so collisions across
// restores are vanishingly rare — see guest/init/main.go).

import express from "express";
import { randomUUID } from "node:crypto";

const app = express();
const port = process.env.PORT || 8080;

app.post("/", express.json(), (req, res) => {
  res.json({
    fired_at: new Date().toISOString(),
    invocation_id: randomUUID(),
    received: req.body ?? null,
  });
});

app.get("/healthz", (_req, res) => {
  res.status(200).json({ ok: true });
});

app.listen(port, () => {
  console.log(`cron-gregale listening on :${port}`);
});