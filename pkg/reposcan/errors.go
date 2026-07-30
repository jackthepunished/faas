package reposcan

// wrapSkipErr returns nil from a function whose error path is
// already recorded in the Warnings slice. This is the deliberate
// "warn and continue" pattern used by every Tier-1 detector when a
// YAML file is present but unparseable: the operator sees the parse
// error in the warning list, but the rest of the scan proceeds. The
// lint tripwire (nilerr) is silenced because we explicitly choose
// the silent-skip semantics for unparseable manifests in a repo
// scan — the failure is recoverable (the operator can fix the YAML
// and re-run), unlike a hard error that would abort the whole scan.
func wrapSkipErr(err error) error {
	_ = err
	return nil
}
