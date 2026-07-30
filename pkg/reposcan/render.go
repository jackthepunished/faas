package reposcan

import (
	"io/fs"
	"sort"

	"gopkg.in/yaml.v3"
)

// renderDoc decodes render.yaml. We read services[].type (web/
// worker) and cronJobs[].schedule + cronJobs[].name.
//
// Render's pserviced-style env/datastore services are not a thing;
// a render.yaml with no `databases:` block and no pserviced is
// honest. Skipped silently.
type renderDoc struct {
	Services []renderService `yaml:"services"`
	CronJobs []renderCron    `yaml:"cronJobs"`
}
type renderService struct {
	Name    string         `yaml:"name"`
	Type    string         `yaml:"type"` // "web" | "worker" | "pserviced"
	Image   string         `yaml:"image"`
	Command any            `yaml:"command"`
	EnvVars map[string]any `yaml:"envVars"`
}
type renderCron struct {
	Name     string `yaml:"name"`
	Schedule string `yaml:"schedule"`
}

var renderFileNames = []string{nameRenderYAML, nameRenderYML}

func detectRender(fsys fs.FS) ([]workloadSeed, []Managed, []string, error) {
	body, src, err := readFirstValidFile(fsys, renderFileNames)
	if err != nil || body == nil {
		return nil, nil, nil, err
	}
	var d renderDoc
	if err := yaml.Unmarshal(body, &d); err != nil {
		// Warn-and-skip: malformed render.yaml is recoverable.
		return nil, nil, []string{"reposcan: parse " + src + ": " + err.Error()}, nil //nolint:nilerr
	}
	var seeds []workloadSeed
	for _, s := range d.Services {
		if s.Type == "pserviced" {
			continue // private infra, not provisioned
		}
		cls := ClassUnknown
		switch s.Type {
		case keyWeb:
			cls = ClassHTTP
		case keyWorker:
			cls = ClassWorker
		}
		if s.Name == "" {
			continue
		}
		seeds = append(seeds, workloadSeed{
			name:    s.Name,
			class:   cls,
			command: commandSlice(s.Command),
			envKeys: envKeys(s.EnvVars),
			source:  src + ": " + s.Name,
		})
	}
	for _, c := range d.CronJobs {
		if c.Name == "" {
			continue
		}
		seeds = append(seeds, workloadSeed{
			name:     c.Name,
			class:    ClassJob,
			schedule: c.Schedule,
			source:   src + ": " + c.Name,
		})
	}
	sort.SliceStable(seeds, func(i, j int) bool { return seeds[i].name < seeds[j].name })
	return seeds, nil, nil, nil
}
