// SPDX-License-Identifier: Apache-2.0

package challenge

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Challenges in an extra directory list and load next to the repository's;
// the repository wins a name collision and the skipped one is reported with
// its directory.
func TestEngine_ExtraDirs(t *testing.T) {
	repo, extra := t.TempDir(), t.TempDir()
	writeChallengeYAML(t, repo, "test-challenge", validChallengeYAML)
	writeChallengeYAML(t, extra, "test-challenge", strings.Replace(validChallengeYAML, `"Test Challenge"`, `"Shadow"`, 1))
	writeChallengeYAML(t, extra, "external", strings.Replace(validChallengeYAML, "name: test-challenge", "name: external", 1))
	e := New(repo, t.TempDir(), "", extra)

	all, err := e.Challenges()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range all {
		names = append(names, c.Name+"="+c.DisplayName)
	}
	slices.Sort(names)
	if want := []string{"external=Test Challenge", "test-challenge=Test Challenge"}; !slices.Equal(names, want) {
		t.Fatalf("Challenges() = %v, want %v", names, want)
	}
	key := filepath.Join(extra, "test-challenge")
	if err, ok := e.LoadErrors()[key]; !ok || !strings.Contains(err.Error(), "already provided by "+filepath.Join(repo, "test-challenge")) {
		t.Fatalf("LoadErrors() = %v, want the collision under %s", e.LoadErrors(), key)
	}
	if c, err := e.Load("external"); err != nil || c.Name != "external" {
		t.Fatalf("Load(external) = %v, %v", c, err)
	}
	if c, _ := e.Load("test-challenge"); c.DisplayName != "Test Challenge" {
		t.Fatalf("Load(test-challenge) took the extra copy %q", c.DisplayName)
	}
	if _, err := e.Load("ghost"); err == nil || !strings.Contains(err.Error(), `challenge "ghost" not found`) {
		t.Fatalf("Load(ghost) err = %v", err)
	}
}
