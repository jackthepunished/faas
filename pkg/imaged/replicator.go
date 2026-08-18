package imaged

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/onebox-faas/faas/pkg/cosign"
)

// CommandArtifactReplicator adapts an operator-owned artifact handoff helper
// to ArtifactReplicator. The helper receives exactly two positional
// arguments: the layer key and its derived signature key. Keeping the
// transfer policy outside the daemon lets a local split-box install use SSH,
// while an OCI deployment leaves the hook unset and uses the shared backend.
type CommandArtifactReplicator struct {
	Path     string
	ExtraEnv []string
}

// Replicate runs the configured helper with a cancellable context. Helper
// output is included only on failure and is capped so a broken transport
// cannot flood imaged's error log.
func (r CommandArtifactReplicator) Replicate(ctx context.Context, layerKey string) error {
	if r.Path == "" {
		return fmt.Errorf("empty artifact replicator path")
	}
	if layerKey == "" {
		return fmt.Errorf("empty layer key")
	}
	cmd := exec.CommandContext(ctx, r.Path, layerKey, cosign.SigKeyFor(layerKey))
	if len(r.ExtraEnv) > 0 {
		cmd.Env = append(os.Environ(), r.ExtraEnv...)
	}
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(out))
	if len(detail) > 2048 {
		detail = detail[:2048] + "…"
	}
	if detail == "" {
		return fmt.Errorf("helper %q: %w", r.Path, err)
	}
	return fmt.Errorf("helper %q: %w: %s", r.Path, err, detail)
}
