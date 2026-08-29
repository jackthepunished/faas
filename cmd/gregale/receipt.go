// cmd/gregale/receipt.go — `DeployReceipt` DTO emitted by
// `gregale deploy --json` (issue #1182 §P1 follow-up to PR #1187).
//
// The receipt embeds api.DeploymentResponse so existing consumers that
// unmarshal `--json` output into api.DeploymentResponse keep working.
// encoding/json promotes the embedded struct's fields to top level,
// and an Unmarshal target that doesn't list the extra keys simply
// ignores them (see TestCmdDeploy_JSON_SkipsStream extension at
// cli_test.go:760).
//
// Receipt-only fields:
//   - app_url:        customer-facing URL derived from deployment's
//                     app id via FAAS_APPS_DOMAIN (deployedAppURL at
//                     commands2.go:3469). Empty when AppID is empty.
//   - commit_sha:     HEAD SHA from zeroConfigProvenance.SHA. Empty on
//                     non-git cwd-auto-pack, image, and source-ref
//                     paths (no git detection ran).
//   - dirty:          working-tree-is-dirty flag from provenance.
//                     false elsewhere (omitted vs explicit false —
//                     the JSON tag is `omitempty` so a non-git
//                     deploy renders no `dirty` key).
//   - source_sha256:  sha256 hex digest of the tarball bytes shipped
//                     to the server. Empty on image (no source
//                     bytes — dep.ImageDigest carries the OCI digest
//                     instead) and source-ref (server pulls the
//                     tarball, CLI never sees bytes) paths.

package main

import (
	"github.com/onebox-faas/faas/pkg/api"
)

// DeployReceipt is the `gregale deploy --json` wire envelope. See
// cmd/gregale/receipt.go header comment for field provenance.
type DeployReceipt struct {
	api.DeploymentResponse
	AppURL       string `json:"app_url,omitempty"`
	CommitSHA    string `json:"commit_sha,omitempty"`
	Dirty        bool   `json:"dirty,omitempty"`
	SourceSHA256 string `json:"source_sha256,omitempty"`
}

// newDeployReceipt builds a DeployReceipt from the post-deploy
// DeploymentResponse plus optional zero-config provenance and the
// optional source-bytes SHA-256. nil-safe: a zero-value dep gives
// the zero DeploymentResponse on the wire; a nil prov leaves
// commit_sha and dirty at their zero values (matches the "no git
// detection ran" image / source-ref / non-git fallback paths).
func newDeployReceipt(dep api.DeploymentResponse, prov *zeroConfigProvenance, appURL, sourceSHA256 string) *DeployReceipt {
	r := &DeployReceipt{
		DeploymentResponse: dep,
		AppURL:             appURL,
		SourceSHA256:       sourceSHA256,
	}
	if prov != nil {
		r.CommitSHA = prov.SHA
		r.Dirty = prov.Dirty
	}
	return r
}
