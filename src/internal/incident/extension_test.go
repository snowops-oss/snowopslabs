// SPDX-License-Identifier: Apache-2.0

package incident

import (
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Faults in an extra content root load next to the in-repo ones; the
// repository wins a name collision and the skipped fault is reported with
// its directory.
func TestNewEngine_ExtraRoots(t *testing.T) {
	repo, extra := t.TempDir(), t.TempDir()
	createApp(t, repo, "go-api")
	writeFault(t, repo, "fault-a")
	writeFault(t, extra, "fault-a")
	writeFault(t, extra, "fault-x")

	e := NewEngine(repo, "k3d.local", extra)
	var names []string
	for _, f := range e.List() {
		names = append(names, f.Name)
	}
	if !slices.Equal(names, []string{"fault-a", "fault-x"}) {
		t.Fatalf("List() = %v, want [fault-a fault-x]", names)
	}
	a, _ := e.Get("fault-a")
	if !strings.HasPrefix(a.Dir, repo) {
		t.Errorf("fault-a loaded from %s, want the repository's copy", a.Dir)
	}
	x, _ := e.Get("fault-x")
	if !strings.HasPrefix(x.Dir, extra) {
		t.Errorf("fault-x loaded from %s, want the extra root", x.Dir)
	}
	errs := e.LoadErrors()
	key := filepath.Join(extra, "incidents", "fault-a")
	if err, ok := errs[key]; !ok || !strings.Contains(err.Error(), `fault name "fault-a" already provided by`) {
		t.Fatalf("LoadErrors() = %v, want the collision reported under %s", errs, key)
	}
}

// A fault that needs a scenario is refused until the scenario is active, the
// message names the command that activates it, and a random pick leaves it
// out of the pool.
func TestPreflight_ScenarioPrerequisite(t *testing.T) {
	tests := []struct {
		name      string
		active    func(string) bool
		wantErr   string // "" means preflight passes
		wantPicks []string
	}{
		{name: "no lookup wired counts as inactive", active: nil,
			wantErr: "scenario \"gitops-cicd\" must be active: run 'labctl scenario up gitops-cicd' first", wantPicks: []string{"plain"}},
		{name: "another scenario active", active: func(n string) bool { return n == "event-driven-arch" },
			wantErr: "labctl scenario up gitops-cicd", wantPicks: []string{"plain"}},
		{name: "the scenario active", active: func(n string) bool { return n == "gitops-cicd" },
			wantPicks: []string{"needs-scenario", "plain"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			createApp(t, root, "go-api")
			writeFault(t, root, "needs-scenario")
			writeFault(t, root, "plain")
			// writeFault ends with the detection block, so a new top-level
			// key would land inside it; rewrite the prerequisites instead.
			file := filepath.Join(root, "incidents", "needs-scenario", "fault.yaml")
			data, _ := os.ReadFile(file)
			data = []byte(strings.Replace(string(data), "prerequisites:\n", "prerequisites:\n  scenarios: [gitops-cicd]\n", 1))
			if err := os.WriteFile(file, data, 0o644); err != nil {
				t.Fatal(err)
			}
			e := NewEngine(root, "k3d.local")
			e.ScenarioActive = tt.active

			f, err := e.Get("needs-scenario")
			if err != nil {
				t.Fatalf("Get: %v", err)
			}
			err = e.Preflight(f)
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Preflight: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Preflight err = %v, want it to say %q", err, tt.wantErr)
			}

			picked := map[string]bool{}
			for seed := int64(1); seed <= 50; seed++ {
				p, err := e.PickRandom(Pick{Seed: seed})
				if err != nil {
					t.Fatalf("seed %d: %v", seed, err)
				}
				picked[p.Name] = true
			}
			if got := slices.Sorted(maps.Keys(picked)); !slices.Equal(got, tt.wantPicks) {
				t.Errorf("seeds 1 to 50 picked %v, want %v", got, tt.wantPicks)
			}
		})
	}
}

// A random pick from a tag or a list never leaves the set, composes with
// exclude, and a set with nothing eligible is an error that says so.
func TestPickRandom_NamedSet(t *testing.T) {
	all := []string{"fault-a", "fault-b", "fault-c", "fault-d"}
	tests := []struct {
		name    string
		in      PickInput
		want    []string
		wantErr string // from ParsePick or PickRandom
	}{
		{name: "tag keeps only tagged faults", in: PickInput{Tag: "ingress-503"}, want: []string{"fault-a", "fault-b"}},
		{name: "from keeps only the listed faults", in: PickInput{From: []string{"fault-c,fault-d"}}, want: []string{"fault-c", "fault-d"}},
		{name: "tag and from together keep the overlap", in: PickInput{Tag: "ingress-503", From: []string{"fault-b", "fault-c"}}, want: []string{"fault-b"}},
		{name: "exclude applies inside the set", in: PickInput{Tag: "ingress-503", Exclude: []string{"fault-a"}}, want: []string{"fault-b"}},
		{name: "an unknown name in from fails before the pick", in: PickInput{From: []string{"fault-a,ghost"}}, wantErr: `--from fault "ghost" does not exist`},
		{name: "a tag no fault has fails before the pick", in: PickInput{Tag: "nope"}, wantErr: `no fault has tag "nope"`},
		{name: "a set emptied by exclude says so", in: PickInput{From: []string{"fault-c"}, Exclude: []string{"fault-c"}}, wantErr: "after applying filters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, root := testEngine(t)
			for _, f := range all {
				writeFault(t, root, f)
			}
			for _, f := range []string{"fault-a", "fault-b"} {
				file := filepath.Join(root, "incidents", f, "fault.yaml")
				data, _ := os.ReadFile(file)
				data = []byte(strings.Replace(string(data), "severity: medium\n", "severity: medium\ntags: [ingress-503, network]\n", 1))
				if err := os.WriteFile(file, data, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			e := NewEngine(root, "k3d.local")

			p, err := e.ParsePick(tt.in)
			if err == nil {
				var picked map[string]bool
				picked, err = pickMany(e, p)
				if err == nil {
					if got := slices.Sorted(maps.Keys(picked)); !slices.Equal(got, tt.want) {
						t.Errorf("seeds 1 to 50 picked %v, want %v", got, tt.want)
					}
				}
			}
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want it to say %q", err, tt.wantErr)
			}
		})
	}
}

// pickMany returns every fault seeds 1 to 50 pick for p.
func pickMany(e *Engine, p Pick) (map[string]bool, error) {
	picked := map[string]bool{}
	for seed := int64(1); seed <= 50; seed++ {
		p.Seed = seed
		f, err := e.PickRandom(p)
		if err != nil {
			return nil, err
		}
		picked[f.Name] = true
	}
	return picked, nil
}

// The same seed and set pick the same fault every time.
func TestPickRandom_NamedSetIsRepeatable(t *testing.T) {
	e, _ := testEngine(t, "fault-a", "fault-b", "fault-c")
	p := Pick{Seed: 7, From: []string{"fault-a", "fault-c"}}
	first, err := e.PickRandom(p)
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if again, _ := e.PickRandom(p); again.Name != first.Name {
			t.Fatalf("seeded pick changed: %s then %s", first.Name, again.Name)
		}
	}
}

// Tags and prerequisite scenarios must be names.
func TestValidate_TagsAndScenarios(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Fault)
		wantErr string
	}{
		{name: "valid tags and scenario", mutate: func(f *Fault) { f.Tags = []string{"ingress-503"}; f.Prerequisites.Scenarios = []string{"gitops-cicd"} }},
		{name: "a tag with capitals", mutate: func(f *Fault) { f.Tags = []string{"Ingress"} }, wantErr: `tag "Ingress" must be lowercase`},
		{name: "a scenario that is not a name", mutate: func(f *Fault) { f.Prerequisites.Scenarios = []string{"../x"} }, wantErr: `prerequisite scenario "../x" is not a scenario name`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := testEngine(t, "fault-a")
			f, _ := e.Get("fault-a")
			c := *f
			tt.mutate(&c)
			err := c.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("err = %v, want it to say %q", err, tt.wantErr)
			}
		})
	}
}
