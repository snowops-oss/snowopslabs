// SPDX-License-Identifier: Apache-2.0

package scenario

import (
	"path/filepath"
	"strings"
	"testing"
)

// Scenarios in an extra content root load next to the in-repo ones, carry
// their root as Source, and lose a name collision to the repository.
func TestNewEngine_ExtraRoots(t *testing.T) {
	const body = "displayName: X\ndescription: x\ncategory: testing\n"
	tests := []struct {
		name       string
		setup      func(t *testing.T, repo, extra string)
		wantSource map[string]string // scenario name -> Source; "" is in-repo
		wantErr    string            // substring of a load error, keyed by the extra dir
	}{
		{
			name: "a scenario only in the extra root is listed with its root",
			setup: func(t *testing.T, repo, extra string) {
				writeScenario(t, repo, "in-repo", "name: in-repo\n"+body)
				writeScenario(t, extra, "external", "name: external\n"+body)
			},
			wantSource: map[string]string{"in-repo": "", "external": "EXTRA"},
		},
		{
			name: "the repository wins a name collision and the skipped one is reported",
			setup: func(t *testing.T, repo, extra string) {
				writeScenario(t, repo, "same", "name: same\n"+body)
				writeScenario(t, extra, "same", "name: same\n"+body)
			},
			wantSource: map[string]string{"same": ""},
			wantErr:    `scenario name "same" already provided by the repository - skipped`,
		},
		{
			name: "an invalid scenario in the extra root does not hide the rest",
			setup: func(t *testing.T, repo, extra string) {
				writeScenario(t, extra, "broken", "name: [unclosed\n")
				writeScenario(t, extra, "fine", "name: fine\n"+body)
			},
			wantSource: map[string]string{"fine": "EXTRA"},
			wantErr:    "broken",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repo, extra := t.TempDir(), t.TempDir()
			tt.setup(t, repo, extra)
			eng := NewEngine(repo, "k3d.local", "k3d", extra)

			got := map[string]string{}
			for _, s := range eng.List() {
				got[s.Name] = s.Source
			}
			if len(got) != len(tt.wantSource) {
				t.Fatalf("listed %v, want %v", got, tt.wantSource)
			}
			for name, want := range tt.wantSource {
				if want == "EXTRA" {
					want = extra
				}
				src, ok := got[name]
				if !ok || src != want {
					t.Errorf("scenario %q: source %q (listed %v), want %q", name, src, ok, want)
				}
			}
			if tt.wantErr == "" {
				return
			}
			var msgs []string
			for key, err := range eng.LoadErrors() {
				if !strings.HasPrefix(key, filepath.Join(extra, "scenarios")) {
					t.Errorf("load error key %q does not name the extra root's directory", key)
				}
				msgs = append(msgs, err.Error())
			}
			if joined := strings.Join(msgs, "\n"); !strings.Contains(joined, tt.wantErr) {
				t.Errorf("load errors %q do not contain %q", joined, tt.wantErr)
			}
		})
	}
}

// Status carries the source too, so the API and `scenario list -o json` can
// attribute an external scenario.
func TestStatus_IncludesSource(t *testing.T) {
	repo, extra := t.TempDir(), t.TempDir()
	writeScenario(t, extra, "external", "name: external\ndisplayName: X\ndescription: x\ncategory: testing\n")
	eng := NewEngine(repo, "k3d.local", "k3d", extra)
	st := eng.Status()
	if len(st) != 1 || st[0].Source != extra {
		t.Fatalf("Status() = %+v, want one item with source %q", st, extra)
	}
}
