// Function template for gregale (node24).
//
// Mirrors cmd/gregale/templates/function-node/handler.js (node22) —
// the only differences are the runtime id (node24) and the handler
// filename (/app/node24.js inside the microVM, set by imaged's
// function-layer manifest). The CLI forces --runtime node24
// --handler handler.handler when deploying this template so the
// wiring is automatic; the `handler.handler` value in --handler is
// the customer's tarball stem (handler.js — the runner resolves the
// underlying filename per runtime).

export async function handler(event, ctx) {
  // event.body is the parsed JSON request body (string for non-JSON).
  // ctx.log is the structured logger guest-init wires up. Surface
  // both so a smoke test sees something useful. The runtime id is
  // also reachable via env if a customer needs to branch on version.
  ctx.log.info("function invoked", { event, invocation_id: ctx.invocation_id, runtime: process.env.FAAS_RUNTIME });
  return {
    statusCode: 200,
    headers: { "content-type": "application/json" },
    body: JSON.stringify({
      ok: true,
      invocation_id: ctx.invocation_id,
      runtime: process.env.FAAS_RUNTIME,
      received: event,
    }),
  };
}
