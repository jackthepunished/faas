package reposcan

// workloadSeed is the internal carrier produced by each tier
// detector before merge.go collapses them into a sorted
// []Workload. Keeping it lighter than Workload makes per-tier
// code shorter (the merge rule fills the empty fields).
type workloadSeed struct {
	tier       Tier
	source     string // provenance string; carried into Workload.Source
	name       string
	rootDir    string
	dockerfile string
	command    []string
	class      Class
	schedule   string
	ports      []int
	envKeys    []string // KEYS only
}

// workloadKey is the merge-by-(RootDir, Name) key. Two seeds with
// the same Key collapse into one Workload during merge.
type workloadKey struct {
	RootDir string
	Name    string
}

func (s workloadSeed) key() workloadKey {
	return workloadKey{RootDir: s.rootDir, Name: s.name}
}

// seedWarning returns a deterministic warning line emitted for a
// seed that came from a non-default source. Keeps the Warning list
// useful for operator debugging without bloating it.
func seedWarning(s workloadSeed) string {
	if s.name == "" {
		return ""
	}
	return "reposcan: " + s.source + ": workload=" + s.name
}
