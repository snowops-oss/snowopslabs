// SPDX-License-Identifier: Apache-2.0

package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/snowops-oss/snowopslabs/internal/catalog"
	"github.com/snowops-oss/snowopslabs/internal/labtest"
)

// copyLabItem copies one fixture item to dstRoot under a new name, renaming
// every mention of the old name in its files.
func copyLabItem(t *testing.T, lab *labtest.Lab, kindDir, from, dstRoot, to string) {
	t.Helper()
	src := filepath.Join(lab.Root, kindDir, from)
	err := filepath.WalkDir(src, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		writeLabFile(t, filepath.Join(dstRoot, kindDir, to, rel), strings.ReplaceAll(string(data), from, to))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Every catalog route lists content from a SNOWOPS_CONTENT_PATH root, a
// scenario says which root it came from, and on a name collision the
// repository's copy is the one served.
func TestCatalogRoutes_ContentPath(t *testing.T) {
	lab := labtest.New(t)
	extra := t.TempDir()
	copyLabItem(t, lab, "scenarios", labScenario, extra, "ext-scenario")
	copyLabItem(t, lab, "scenarios", labScenario, extra, labScenario)
	copyLabItem(t, lab, "incidents", labFault, extra, "ext-fault")
	copyLabItem(t, lab, "challenges", labChallenge, extra, "ext-challenge")
	copyLabItem(t, lab, "learn", labPath, extra, "ext-path")
	t.Setenv(catalog.ContentPathEnv, extra)
	s := contractServer(t, lab)

	for _, tt := range []struct{ path, want string }{
		{"/incidents?app=" + labApp, `"name":"ext-fault"`},
		{"/challenges", `"name":"ext-challenge"`},
		{"/learn/paths", `"name":"ext-path"`},
		{"/learn/paths/ext-path", `"name":"ext-path"`},
	} {
		w := do(s, http.MethodGet, tt.path, "", nil)
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), tt.want) {
			t.Errorf("GET %s = %d %s, want 200 with %s", tt.path, w.Code, w.Body, tt.want)
		}
	}

	w := do(s, http.MethodGet, "/scenarios", "", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /scenarios = %d %s", w.Code, w.Body)
	}
	var body struct {
		Items []struct {
			Name   string `json:"name"`
			Source string `json:"source"`
		} `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decoding %s: %v", w.Body, err)
	}
	got := map[string]string{}
	for _, it := range body.Items {
		got[it.Name] = it.Source
	}
	if src, ok := got["ext-scenario"]; !ok || src != extra {
		t.Errorf("ext-scenario source = %q (listed %v), want %q", src, ok, extra)
	}
	if src, ok := got[labScenario]; !ok || src != "" {
		t.Errorf("%s source = %q, want the repository's copy (no source)", labScenario, src)
	}
}

// A random inject from a tag or a list only picks inside the set; a bad set
// is a 400 before anything is picked.
func TestHandleIncidentInjectRandom_NamedSet(t *testing.T) {
	tests := []struct {
		name    string
		query   string
		status  int
		problem string
		want    string
	}{
		{"from picks inside the list", "&seed=3&from=other-fault", http.StatusOK, "", `"name":"other-fault"`},
		{"tag picks inside the tag", "&seed=3&tag=ingress-503", http.StatusOK, "", `"name":"other-fault"`},
		{"an unknown fault in from", "&from=ghost", http.StatusBadRequest, "invalid_input", `--from fault \"ghost\" does not exist`},
		{"a tag no fault has", "&tag=nope", http.StatusBadRequest, "invalid_input", `no fault has tag \"nope\"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lab := labtest.New(t)
			fault := strings.ReplaceAll(readLabFile(t, lab, "incidents/"+labFault+"/fault.yaml"), labFault, "other-fault")
			lab.Write(t, "incidents/other-fault/fault.yaml", strings.Replace(fault, "severity: low\n", "severity: low\ntags: [ingress-503]\n", 1))
			for _, f := range []string{"inject.sh", "resolve.sh", "checks/resolved.sh", "hints.md", "solution.md"} {
				lab.Write(t, "incidents/other-fault/"+f, readLabFile(t, lab, "incidents/"+labFault+"/"+f))
			}
			s := contractServer(t, lab)

			path := "/incidents/inject-random?app=" + labApp + tt.query
			w := do(s, http.MethodPost, path, "", nil)
			waitForJobs(t, s)
			if tt.problem != "" {
				assertProblem(t, w, tt.status, tt.problem, instanceOf(t, path))
			} else if w.Code != tt.status {
				t.Fatalf("status = %d, want %d\nbody: %s", w.Code, tt.status, w.Body)
			}
			if !strings.Contains(w.Body.String(), tt.want) {
				t.Errorf("body = %s, want it to contain %s", w.Body, tt.want)
			}
		})
	}
}

// A fault that needs an inactive scenario answers 409 with the command that
// activates it, and nothing is injected.
func TestHandleIncidentInject_NeedsTheScenario(t *testing.T) {
	lab := labtest.New(t)
	fault := strings.ReplaceAll(readLabFile(t, lab, "incidents/"+labFault+"/fault.yaml"), labFault, "scenario-fault")
	lab.Write(t, "incidents/scenario-fault/fault.yaml", strings.Replace(fault, "prerequisites:\n", "prerequisites:\n  scenarios: ["+labScenario+"]\n", 1))
	for _, f := range []string{"inject.sh", "resolve.sh", "checks/resolved.sh", "hints.md", "solution.md"} {
		lab.Write(t, "incidents/scenario-fault/"+f, readLabFile(t, lab, "incidents/"+labFault+"/"+f))
	}
	s := contractServer(t, lab)

	path := "/incidents/scenario-fault/inject?app=" + labApp
	w := do(s, http.MethodPost, path, "", nil)
	waitForJobs(t, s)
	assertProblem(t, w, http.StatusConflict, "prerequisite_missing", instanceOf(t, path))
	if !strings.Contains(w.Body.String(), "labctl scenario up "+labScenario) {
		t.Errorf("body = %s, want the command that activates %s", w.Body, labScenario)
	}
	if _, err := os.Stat(filepath.Join(lab.Root, "incidents", "scenario-fault", "BROKEN")); err == nil {
		t.Error("the fault was injected without its scenario")
	}
}
