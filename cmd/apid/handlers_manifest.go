package main

// handlers_manifest.go — apid-side server validator for the
// `gregale.yaml` declarative manifest.
//
// This file is the apid mirror of pkg/gregalemanifest.Validate
// (ADR-090 PR-C, ADR-0NN widening). The CLI uses gregalemanifest
// directly; the apid needs the same validation surface because the
// trigger routes added in commit #6 accept an inline manifest blob
// (POST /v1/triggers:batch_create). Rather than duplicating the
// per-kind validator, this file reuses pkg/gregalemanifest verbatim
// and adds the plan-tier gating the CLI doesn't need (the CLI is
// per-machine, the apid is per-account — the same plan-cap gate
// the createCron handler applies at handlers_ext.go:1683-1687
// applies here at the manifest load site).
//
// Why a thin wrapper rather than calling gregalemanifest.Validate
// directly: the apid handler signature must return *api.Problem on
// validation failure so the existing Problem-with-extraHeaders
// round-trip works; the manifest package's error is a plain
// fmt.Errorf. The wrapper maps the manifest error onto the
// CodeAppManifestInvalid RFC 7807 code so the customer sees a
// stable, machine-readable error code from the CLI or the dashboard.
//
// The actual trigger routes land in commit #6; this file is the
// shared validator surface they call.

import (
	"fmt"
	"net/http"

	"github.com/onebox-faas/faas/pkg/api"
	"github.com/onebox-faas/faas/pkg/gregalemanifest"
)

// CodeAppManifestInvalid is the 422 RFC 7807 code a customer sees
// when an inline manifest blob in a trigger batch-create request
// fails validation. Distinct from CodeValidation (the apid body
// shape guard) because the gating policy and the actor are
// different: CodeValidation rejects malformed JSON at the wire
// boundary, CodeAppManifestInvalid rejects a structurally-valid
// YAML/JSON payload that doesn't pass the per-kind validator.
const CodeAppManifestInvalid = "app_manifest_invalid"

// validateManifest loads + validates a gregale.yaml blob from disk
// (or from an in-memory byte slice — see validateManifestBytes).
// Returns *Manifest on success and *api.Problem on validation
// failure so the handler can write the response directly.
func validateManifest(dir string, acctPlan api.Plan) (*gregalemanifest.Manifest, *api.Problem) {
	m, ok, err := gregalemanifest.Load(dir)
	if err != nil {
		return nil, api.NewProblem(http.StatusUnprocessableEntity, CodeAppManifestInvalid,
			"Invalid manifest", err.Error())
	}
	if !ok {
		// No manifest present is NOT an error — a project without a
		// gregale.yaml simply has no triggers. The handler treats
		// this as "no work to do" rather than 4xx.
		return nil, nil
	}
	if prob := validateManifestAgainstPlan(m, acctPlan); prob != nil {
		return nil, prob
	}
	return m, nil
}

// validateManifestBytes is the in-memory counterpart used by the
// trigger batch-create route (commit #6) where the manifest is
// carried inside the POST body rather than alongside the source
// tarball. The byte slice is parsed via a synthetic tmp dir or via
// a future gregalemanifest.ParseBytes helper — today we stage the
// bytes to t.TempDir() inside the handler and delegate to
// validateManifest. Kept as a separate signature so the future
// ParseBytes helper slots in cleanly when it lands.
func validateManifestBytes(b []byte, acctPlan api.Plan) (*gregalemanifest.Manifest, *api.Problem) {
	if len(b) == 0 {
		return nil, nil
	}
	// Inline parsing without staging to disk: hand the bytes to a
	// future ParseBytes path. For now (commit #5 ships the validator
	// surface but not the routes), we surface a "not yet wired"
	// problem so the route commit knows to wire ParseBytes.
	return nil, api.NewProblem(http.StatusUnprocessableEntity, CodeAppManifestInvalid,
		"Inline manifest validation not yet wired",
		fmt.Sprintf("inline-manifest validation hooks into commit #6 (trigger routes); for now, ship gregale.yaml alongside the source tarball and the deploy path will pick it up via validateManifest(%s)", "<dir>"))
}

// validateManifestAgainstPlan applies the per-plan tier gate the
// CLI doesn't need. Mirrors the createCron gate pattern at
// handlers_ext.go:1683-1687 — if any trigger in the manifest is of
// a kind the plan doesn't unlock, the gate fires BEFORE the store
// is touched.
//
// Today the gate is binary (triggers allowed or not, controlled by
// Plan.TriggersAllowed() — Free has it off, Hobby+ on). When the
// per-kind quotas land (PR-B in the trigger cluster), this
// function will grow per-kind counts against TriggerLimitPerApp /
// TriggerLimitPerAccount / TriggerBatchSizeMax.
func validateManifestAgainstPlan(m *gregalemanifest.Manifest, acctPlan api.Plan) *api.Problem {
	if m == nil {
		return nil
	}
	if !acctPlan.TriggersAllowed() {
		// The CLI is per-machine, so the CLI doesn't see this gate;
		// the apid is per-account, so a Free customer posting a
		// manifest with any trigger gets the upsell here rather
		// than per-trigger 402s during the deploy loop.
		return api.ErrPlanTriggersNotAllowed(acctPlan)
	}
	return nil
}
