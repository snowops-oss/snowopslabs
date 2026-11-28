// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowops-oss/snowopslabs/internal/catalog"
	"github.com/snowops-oss/snowopslabs/internal/labtest"
)

// copyItem copies one fixture item directory to dst under a new name,
// renaming every mention of the old name in its files.
func copyItem(t *testing.T, l *terminal, kindDir, from, dstRoot, to string) {
	t.Helper()
	src := filepath.Join(l.Root, kindDir, from)
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		out := filepath.Join(dstRoot, kindDir, to, rel)
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		return os.WriteFile(out, []byte(strings.ReplaceAll(string(data), from, to)), 0o755)
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Content in a SNOWOPS_CONTENT_PATH root reaches every list, loads by name,
// and an external fault's own script is the one inject runs.
func TestContentPath_ExtraRootReachesEveryLoader(t *testing.T) {
	l := newTerminal(t)
	extra := t.TempDir()
	copyItem(t, l, "scenarios", labtest.Scenario, extra, "ext-scenario")
	copyItem(t, l, "incidents", labtest.Fault, extra, "ext-fault")
	copyItem(t, l, "challenges", labtest.Challenge, extra, "ext-challenge")
	copyItem(t, l, "learn", labtest.Path, extra, "ext-path")
	t.Setenv(catalog.ContentPathEnv, extra)

	checks := []struct {
		args []string
		want []string
	}{
		{[]string{"scenario", "list"}, []string{"ext-scenario", extra, labtest.Scenario}},
		{[]string{"scenario", "list", "-o", "json"}, []string{`"name": "ext-scenario"`, `"source": "` + extra + `"`}},
		{[]string{"scenario", "info", "ext-scenario"}, []string{"Demo scenario"}},
		{[]string{"incident", "list"}, []string{"ext-fault", labtest.Fault}},
		{[]string{"challenge", "list"}, []string{"ext-challenge", labtest.Challenge}},
		{[]string{"learn", "list"}, []string{"ext-path", labtest.Path}},
		{[]string{"validate"}, []string{"2 challenges, 2 incidents, 2 paths, 2 scenarios"}},
	}
	for _, c := range checks {
		r := l.mustRun(c.args...)
		for _, w := range c.want {
			if !strings.Contains(r.stdout, w) {
				t.Errorf("labctl %s does not show %q:\n%s", strings.Join(c.args, " "), w, r.stdout)
			}
		}
		if strings.Contains(r.stderr, "Warning") {
			t.Errorf("labctl %s warned with no collision:\n%s", strings.Join(c.args, " "), r.stderr)
		}
	}

	l.mustRun("incident", "inject", "ext-fault")
	if _, err := os.Stat(filepath.Join(extra, "incidents", "ext-fault", "BROKEN")); err != nil {
		t.Errorf("inject did not run the external fault's own inject.sh: %v", err)
	}
	if l.exists(faultMarker) {
		t.Error("inject ext-fault ran the in-repo fault's script")
	}
}

// An external item with an in-repo name is skipped, the list warns with the
// external directory, and validate fails naming both files.
func TestContentPath_CollisionIsReported(t *testing.T) {
	l := newTerminal(t)
	extra := t.TempDir()
	copyItem(t, l, "scenarios", labtest.Scenario, extra, labtest.Scenario)
	copyItem(t, l, "incidents", labtest.Fault, extra, labtest.Fault)
	copyItem(t, l, "challenges", labtest.Challenge, extra, labtest.Challenge)
	copyItem(t, l, "learn", labtest.Path, extra, labtest.Path)
	t.Setenv(catalog.ContentPathEnv, extra)

	for _, c := range []struct {
		args []string
		dir  string
	}{
		{[]string{"scenario", "list"}, "scenarios"},
		{[]string{"incident", "list"}, "incidents"},
		{[]string{"challenge", "list"}, "challenges"},
		{[]string{"learn", "list"}, "learn"},
	} {
		r := l.mustRun(c.args...)
		skipped := filepath.Join(extra, c.dir)
		if !strings.Contains(r.stderr, skipped) || !strings.Contains(r.stderr, "already provided by") {
			t.Errorf("labctl %s does not report the skipped copy under %s:\n%s", strings.Join(c.args, " "), skipped, r.stderr)
		}
		if strings.Count(r.stdout, extra) > 0 {
			t.Errorf("labctl %s lists the external copy:\n%s", strings.Join(c.args, " "), r.stdout)
		}
	}

	r := l.run("validate")
	if r.exitCode() != 1 {
		t.Fatalf("validate passed with four collisions:\n%s", r.stdout)
	}
	if got := strings.Count(r.stdout, "already provided by"); got != 4 {
		t.Errorf("validate reported %d collisions, want 4:\n%s", got, r.stdout)
	}
}

// writeExtraFault adds a copy of the fixture fault under name with extra
// fault.yaml text spliced in after the severity line.
func writeExtraFault(t *testing.T, l *terminal, name, afterSeverity, prerequisites string) {
	t.Helper()
	copyItem(t, l, "incidents", labtest.Fault, l.Root, name)
	fault := readFixture(t, l, "incidents/"+name+"/fault.yaml")
	fault = strings.Replace(fault, "severity: low\n", "severity: low\n"+afterSeverity, 1)
	fault = strings.Replace(fault, "prerequisites:\n", "prerequisites:\n"+prerequisites, 1)
	l.Write(t, "incidents/"+name+"/fault.yaml", fault)
}

// A fault that needs a scenario is refused, with the command that activates
// it, until the scenario is up; then it injects.
func TestIncident_InjectNeedsTheScenario(t *testing.T) {
	l := newTerminal(t)
	writeExtraFault(t, l, "scenario-fault", "", "  scenarios: ["+labtest.Scenario+"]\n")

	r := l.run("incident", "inject", "scenario-fault")
	want := "labctl scenario up " + labtest.Scenario
	if r.exitCode() != 1 || !strings.Contains(r.stderr, want) {
		t.Fatalf("exit %d, stderr %q; want exit 1 saying %q", r.exitCode(), r.stderr, want)
	}
	if l.exists("incidents/scenario-fault/BROKEN") {
		t.Fatal("the fault was injected without its scenario")
	}
	if r := l.run("incident", "inject", "--random", "--from", "scenario-fault"); r.exitCode() != 1 || !strings.Contains(r.stderr, "no eligible faults") {
		t.Errorf("a random pick offered the fault without its scenario: exit %d, stderr %q", r.exitCode(), r.stderr)
	}

	l.mustRun("scenario", "up", labtest.Scenario)
	l.mustRun("incident", "inject", "scenario-fault")
	if !l.exists("incidents/scenario-fault/BROKEN") {
		t.Error("inject succeeded but did not run the fault once the scenario was active")
	}
}

// A random pick from a tag or a list only ever injects a fault from that set.
func TestIncident_RandomInjectFromANamedSet(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		marker string
	}{
		{name: "by tag", args: []string{"--tag", "ingress-503"}, marker: "incidents/tagged-fault/BROKEN"},
		{name: "by list", args: []string{"--from", "listed-fault"}, marker: "incidents/listed-fault/BROKEN"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, seed := range []string{"1", "2", "3", "4"} {
				l := newTerminal(t)
				writeExtraFault(t, l, "tagged-fault", "tags: [ingress-503]\n", "")
				writeExtraFault(t, l, "listed-fault", "", "")
				args := append([]string{"incident", "inject", "--random", "--seed", seed}, tt.args...)
				l.mustRun(args...)
				if !l.exists(tt.marker) {
					t.Fatalf("labctl %s injected a fault outside the set", strings.Join(args, " "))
				}
			}
		})
	}
}

func TestIncident_NamedSetFailures(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"--tag without --random", []string{"incident", "inject", labtest.Fault, "--tag", "x"}, "--tag, --from, --not-since and --exclude require --random"},
		{"--from without --random", []string{"incident", "inject", labtest.Fault, "--from", labtest.Fault}, "require --random"},
		{"--from with a typo", []string{"incident", "inject", "--random", "--from", "ghost"}, `--from fault "ghost" does not exist`},
		{"a tag no fault has", []string{"incident", "inject", "--random", "--tag", "nope"}, `no fault has tag "nope"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTerminal(t)
			r := l.run(tt.args...)
			if r.exitCode() != 1 || !strings.Contains(r.stderr, tt.wantErr) {
				t.Fatalf("exit %d, stderr %q; want exit 1 saying %q", r.exitCode(), r.stderr, tt.wantErr)
			}
			if l.exists(faultMarker) {
				t.Error("a fault was injected although the pick was refused")
			}
		})
	}
}
