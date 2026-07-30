package reposcan

import (
	"io/fs"
	"sort"

	"github.com/BurntSushi/toml"
)

// fly.toml has a single app + a [processes] section that may name
// per-process types. We read:
//
//	app                       → workload name (the app's name, "name-of-fly-app")
//	[processes]               → per-process names + counts (we treat them as workers)
//	[[services]]              → ports, command (omit for now — Fly doesn't carry these for tier-1)
//
// class is class=http for the app and class=worker for any named
// process. Schedules are not expressible in fly.toml — jobs aren't
// a Fly tier-1 source.
//
// BurntSushi/toml reads key `app` into a struct field named `App`
// (case-insensitive match on first letter, exact match on the rest
// via the `toml:"app"` tag), so we expose both a `Name` (legacy
// `name = "…"` form) and an `App` (`app = "…"` form) — Fly v1.x
// has used both names historically and the field that wins is the
// one that wrote the value.
type flyDoc struct {
	App       string         `toml:"app"`
	Name      string         `toml:"name"`
	Processes map[string]int `toml:"processes"`
}

func detectFly(fsys fs.FS) ([]workloadSeed, []Managed, []string, error) {
	body, src, err := readFirstValidFile(fsys, []string{nameFlyTOML})
	if err != nil || body == nil {
		return nil, nil, nil, err
	}
	var d flyDoc
	if err := toml.Unmarshal(body, &d); err != nil {
		return nil, nil, []string{"reposcan: parse " + src + ": " + err.Error()}, wrapSkipErr(err)
	}
	appName := d.App
	if appName == "" {
		appName = d.Name
	}
	var seeds []workloadSeed
	if appName != "" {
		seeds = append(seeds, workloadSeed{
			name:   appName,
			class:  ClassHTTP,
			source: src + ": " + appName,
		})
	}
	// processes section is "name = count" — count > 0 means a
	// worker slot exists. We don't enumerate "name-1, name-2"; we
	// emit a single Worker per process name (count is informational).
	for pname := range d.Processes {
		if pname == keyWeb {
			// already covered by the app name; merge later
			if len(seeds) > 0 && seeds[0].name == appName {
				seeds[0].class = ClassHTTP
			}
			continue
		}
		seeds = append(seeds, workloadSeed{
			name:   pname,
			class:  ClassWorker,
			source: src + ": " + pname,
		})
	}
	sort.SliceStable(seeds, func(i, j int) bool { return seeds[i].name < seeds[j].name })
	return seeds, nil, nil, nil
}
