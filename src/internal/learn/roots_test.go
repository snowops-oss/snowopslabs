// SPDX-License-Identifier: Apache-2.0

package learn

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// Paths in an extra directory list and load next to the repository's; the
// repository wins a name collision and the skipped one is reported with its
// directory.
func TestEngine_ExtraDirs(t *testing.T) {
	repo, extra := t.TempDir(), t.TempDir()
	writePathYAML(t, repo, "test-path", validPathYAML)
	writePathYAML(t, extra, "test-path", strings.Replace(validPathYAML, `"Test Path"`, `"Shadow"`, 1))
	writePathYAML(t, extra, "external", strings.Replace(validPathYAML, "name: test-path", "name: external", 1))
	e := New(repo, t.TempDir(), "", extra)

	all, err := e.Paths()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, p := range all {
		names = append(names, p.Name+"="+p.DisplayName)
	}
	slices.Sort(names)
	if want := []string{"external=Test Path", "test-path=Test Path"}; !slices.Equal(names, want) {
		t.Fatalf("Paths() = %v, want %v", names, want)
	}
	key := filepath.Join(extra, "test-path")
	if err, ok := e.LoadErrors()[key]; !ok || !strings.Contains(err.Error(), "already provided by "+filepath.Join(repo, "test-path")) {
		t.Fatalf("LoadErrors() = %v, want the collision under %s", e.LoadErrors(), key)
	}
	if p, err := e.LoadPath("external"); err != nil || p.Name != "external" {
		t.Fatalf("LoadPath(external) = %v, %v", p, err)
	}
	if p, _ := e.LoadPath("test-path"); p.DisplayName != "Test Path" {
		t.Fatalf("LoadPath(test-path) took the extra copy %q", p.DisplayName)
	}
	if _, err := e.LoadPath("ghost"); err == nil || !strings.Contains(err.Error(), `learning path "ghost" not found`) {
		t.Fatalf("LoadPath(ghost) err = %v", err)
	}
}
