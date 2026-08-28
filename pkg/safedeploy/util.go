// Internal helpers for pkg/safedeploy. Kept in a separate file
// so the orchestrator + action files read at the policy level
// without the parseDeploymentUUID plumbing at every site.
package safedeploy

import "github.com/google/uuid"

// parseDeploymentUUID parses the string-form deployment_id or
// account_id that the orchestrator pulls from the deployment
// row into the uuid.UUID type the deployment_audit table
// requires. The orchestrator never writes deployment_audit
// rows without parsing first (the row's DeploymentID column is
// NOT NULL — emitting nil would error out at the SQL layer).
//
// The parse is strict: an unparseable id is a configuration bug
// (deployments.id is uuid in the schema) and surfaces to the
// caller via the returned error.
func parseDeploymentUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, errEmptyDeploymentUUID
	}
	return uuid.Parse(s)
}

// errEmptyDeploymentUUID is returned by parseDeploymentUUID
// when the input is the empty string. Sentinel so callers can
// distinguish "no id provided" from "malformed id provided".
var errEmptyDeploymentUUID = &safedeployUUIDError{msg: "empty deployment_id"}

// safedeployUUIDError is a typed error so the orchestrator's
// emitAudit can log it with the right context without an
// errors.Is/As round-trip.
type safedeployUUIDError struct {
	msg string
}

func (e *safedeployUUIDError) Error() string { return e.msg }
