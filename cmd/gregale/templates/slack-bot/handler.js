// slack-bot — Wave 0 PR-B stateless-contract template.
//
// This is a SCAFFOLD that implements the Slack Events API endpoint
// with REAL HMAC-SHA256 signature verification (Slack's "v0" signing
// scheme). It is NOT a stub: every incoming request must carry a
// valid `X-Slack-Signature` header derived from `SLACK_SIGNING_SECRET`,
// the raw request body, and the `X-Slack-Request-Timestamp` header —
// otherwise the handler responds 401. The URL-verification challenge
// is also handled, so the customer's first Slack-app configuration
// handshake works out of the box.
//
// Required env vars (set via `gregale secrets set --app <slug> ...`):
//
//	SLACK_SIGNING_SECRET — Slack app "Signing Secret" (App Settings → Basic Information)
//	SLACK_BOT_TOKEN      — optional; only needed if you post replies
//	                       (`xoxb-...`). The scaffold does not post
//	                       anything; uncomment the chat.postMessage
//	                       call below to enable.

import express from "express";
import crypto from "node:crypto";

const REQUIRED_ENV = ["SLACK_SIGNING_SECRET"];

function missingEnv() {
  return REQUIRED_ENV.filter((k) => !process.env[k] || process.env[k].length === 0);
}

const missing = missingEnv();
if (missing.length > 0) {
  console.error("Missing required env vars:", missing.join(", "));
  console.error("Set them via:");
  console.error(
    "  gregale secrets set --app <slug> SLACK_SIGNING_SECRET=... SLACK_BOT_TOKEN=xoxb-...",
  );
  process.exit(1);
}

const SIGNING_SECRET = process.env.SLACK_SIGNING_SECRET;
// Slack rejects requests whose timestamp is more than 5 minutes off.
const MAX_CLOCK_SKEW_SECONDS = 5 * 60;

// constantTimeEqual avoids a timing-attack side channel on the
// signature compare. crypto.timingSafeEqual panics on length
// mismatch, so we always feed it equal-length buffers.
function constantTimeEqual(a, b) {
  const ab = Buffer.from(a, "utf8");
  const bb = Buffer.from(b, "utf8");
  if (ab.length !== bb.length) {
    return false;
  }
  return crypto.timingSafeEqual(ab, bb);
}

function verifySlackSignature(rawBody, timestamp, signature) {
  if (!timestamp || !signature) {
    return false;
  }
  const ts = Number(timestamp);
  if (!Number.isFinite(ts)) {
    return false;
  }
  const now = Math.floor(Date.now() / 1000);
  if (Math.abs(now - ts) > MAX_CLOCK_SKEW_SECONDS) {
    return false;
  }
  // Slack's signing scheme: "v0:" + ts + ":" + rawBody, HMAC-SHA256
  // with the signing secret, prefixed with "v0=" when compared.
  const base = `v0:${timestamp}:${rawBody}`;
  const computed =
    "v0=" + crypto.createHmac("sha256", SIGNING_SECRET).update(base).digest("hex");
  return constantTimeEqual(computed, signature);
}

const app = express();
const port = process.env.PORT || 8080;

// Slack sends `application/x-www-form-urlencoded` for the URL-verification
// handshake AND `application/json` for everything else. We capture the
// raw bytes for the HMAC check (express's built-in JSON parser would
// re-serialise and break the signature), then parse manually. The
// `type: () => true` predicate accepts any content-type so we don't
// reject Slack's two distinct encodings.
app.use(
  express.raw({
    type: () => true,
    limit: "1mb",
  }),
);

app.post("/slack/events", (req, res) => {
  const raw = req.body.toString("utf8");
  const ts = req.get("X-Slack-Request-Timestamp") || "";
  const sig = req.get("X-Slack-Signature") || "";
  if (!verifySlackSignature(raw, ts, sig)) {
    console.error("invalid slack signature");
    return res.status(401).json({ ok: false, error: "invalid signature" });
  }

  let event;
  try {
    event = JSON.parse(raw);
  } catch (err) {
    return res.status(400).json({ ok: false, error: "invalid JSON" });
  }

  // URL-verification handshake: Slack sends a `challenge` field, we
  // echo it back. No event processing, no logging, just respond.
  if (event.type === "url_verification") {
    return res.status(200).json({ challenge: event.challenge });
  }

  // Real event callbacks. Log the essentials so a customer's first
  // smoke test sees something useful in `gregale logs <slug>`.
  if (event.type === "event_callback") {
    const inner = event.event || {};
    console.log(
      `slack event: type=${inner.type} user=${inner.user || "?"} channel=${inner.channel || "?"}`,
    );
    // To reply to messages, uncomment:
    //   if (inner.type === "message" && process.env.SLACK_BOT_TOKEN) {
    //     await fetch("https://slack.com/api/chat.postMessage", { ... });
    //   }
  }

  // Slack expects an immediate 200 so it doesn't retry the event.
  return res.status(200).json({ ok: true });
});

app.get("/healthz", (_req, res) => {
  res.status(200).json({ ok: true });
});

app.listen(port, () => {
  console.log(`slack-bot listening on :${port}`);
});
