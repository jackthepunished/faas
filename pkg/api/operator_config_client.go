package api

import (
	"context"
	"net/url"
)

// PostAdminConfigKeyRollback applies a historical hot runtime-config value
// as a new revision without restarting apid. The server preserves the
// original history and enforces optimistic concurrency when ExpectedVersion
// is supplied.
func (c *Client) PostAdminConfigKeyRollback(ctx context.Context, key string, req RollbackOperatorRuntimeConfigRequest) (OperatorRuntimeConfig, error) {
	var out OperatorRuntimeConfig
	path := "/v1/admin/config/" + url.PathEscape(key) + "/rollback"
	return out, c.do(ctx, "POST", path, req, &out)
}
