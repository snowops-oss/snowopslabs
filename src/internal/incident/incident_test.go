// SPDX-License-Identifier: Apache-2.0

package incident

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/snowops-oss/snowopslabs/internal/executor"
	"github.com/snowops-oss/snowopslabs/internal/results"
	"github.com/snowops-oss/snowopslabs/internal/workload"
	"github.com/snowops-oss/snowopslabs/pkg/checks"
)

const testFaultYAML = `name: %s
displayName: "Test Fault"
description: "breaks things for tests"
category: workload
severity: medium
target:
  namespace: go-api
  workload: go-api
prerequisites:
  apps:
    - go-api
detection:
  name: resolved-when-marker-gone
  type: script
  script: checks/resolved.sh
  timeoutSeconds: 5
`

// writeFault creates a complete fault dir whose inject.sh drops a marker
// file and whose detection check passes once the marker is gone - a fully
// working incident without a cluster.
func writeFault(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "incidents", name)
	if err := os.MkdirAll(filepath.Join(dir, "checks"), 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		"fault.yaml":         strings.Replace(testFaultYAML, "%s", name, 1),
		"inject.sh":          "#!/usr/bin/env bash\ntouch \"$(dirname \"$0\")/BROKEN\"\necho injected \"$TARGET_NAMESPACE/$TARGET_WORKLOAD\"\n",
		"resolve.sh":         "#!/usr/bin/env bash\nrm -f \"$(dirname \"$0\")/BROKEN\"\necho resolved\n",
		"checks/resolved.sh": "#!/usr/bin/env bash\n[ ! -f \"$(dirname \"$0\")/../BROKEN\" ]\n",
		"hints.md":           "# Hints\n\n## Hint 1\nLook closer.\n\n## Hint 2\nCloser still.\n\n## Hint 3\nIt's the marker file.\n",
		"solution.md":        "# Solution\nRemove the marker.\n",
	}
	for f, content := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(content), 0755); err != nil {
			t.Fatal(err)
		}
	}
}

func createApp(t *testing.T, root, name string) {
	t.Helper()
	dir := filepath.Join(root, "apps", name)
	os.MkdirAll(dir, 0755)
	os.WriteFile(filepath.Join(dir, "app.env"), []byte("APP_NAME="+name+"\n"), 0644)
}

func testEngine(t *testing.T, faults ...string) (*Engine, string) {
	t.Helper()
	root := t.TempDir()
	createApp(t, root, "go-api")
	for _, f := range faults {
		writeFault(t, root, f)
	}
	return NewEngine(root, "k3d.local"), root
}

func testRunner() *checks.Runner {
	r := checks.NewRunner()
	r.DefaultTimeout = 5 * time.Second
	return r
}

func TestEngine_DiscoveryAndContract(t *testing.T) {
	e, root := testEngine(t, "fault-a", "fault-b")
	if len(e.List()) != 2 {
		t.Fatalf("expected 2 faults, got %d", len(e.List()))
	}

	// A dir violating the contract (missing resolve.sh) must not load.
	dir := filepath.Join(root, "incidents", "broken")
	writeFault(t, root, "broken")
	os.Remove(filepath.Join(dir, "resolve.sh"))
	e = NewEngine(root, "k3d.local")
	if len(e.List()) != 2 {
		t.Fatalf("contract-violating fault must not load, got %d faults", len(e.List()))
	}
	if err := e.LoadErrors()["broken"]; err == nil || !strings.Contains(err.Error(), "resolve.sh") {
		t.Fatalf("load error should name the missing file: %v", err)
	}
}

func TestEngine_NameMustMatchDir(t *testing.T) {
	root := t.TempDir()
	writeFault(t, root, "dir-name")
	yaml := strings.Replace(testFaultYAML, "%s", "other-name", 1)
	os.WriteFile(filepath.Join(root, "incidents", "dir-name", "fault.yaml"), []byte(yaml), 0644)

	e := NewEngine(root, "k3d.local")
	if len(e.List()) != 0 {
		t.Fatal("name/dir mismatch must not load")
	}
	if err := e.LoadErrors()["dir-name"]; err == nil || !strings.Contains(err.Error(), "must match") {
		t.Fatalf("got %v", err)
	}
}

func TestFaultValidate(t *testing.T) {
	base := func() *Fault {
		return &Fault{
			Name: "x", Category: "network", Severity: "high",
			Target:    Target{Namespace: "go-api", Workload: "go-api"},
			Detection: checks.Check{Name: "d", Type: "http", URL: "http://x"},
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("base fault should be valid: %v", err)
	}

	tests := []struct {
		name    string
		mutate  func(*Fault)
		wantErr string
	}{
		{"bad category", func(f *Fault) { f.Category = "cosmic" }, "unknown category"},
		{"bad severity", func(f *Fault) { f.Severity = "apocalyptic" }, "unknown severity"},
		{"missing target", func(f *Fault) { f.Target = Target{} }, "target.namespace"},
		{"invalid detection", func(f *Fault) { f.Detection.URL = "" }, "detection:"},
		{"detection script traversal", func(f *Fault) {
			f.Detection = checks.Check{Name: "d", Type: "script", Script: "../../etc/evil.sh"}
		}, "relative path inside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := base()
			tt.mutate(f)
			err := f.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestInject_StatusResolutionLoop(t *testing.T) {
	e, root := testEngine(t, "fault-a")
	ex := executor.New(root)
	ctx := context.Background()

	// Nothing active initially.
	if _, err := e.Status(ctx, testRunner(), ""); !errors.Is(err, ErrNoActive) {
		t.Fatalf("expected ErrNoActive, got %v", err)
	}

	f, err := e.Inject("fault-a", ex, false, false)
	if err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if f.Name != "fault-a" {
		t.Fatalf("injected %q", f.Name)
	}

	// Status while broken: not resolved, state stays.
	res, err := e.Status(ctx, testRunner(), "")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if res.Resolved || res.Check.Pass {
		t.Fatalf("should not be resolved yet: %+v", res.Check)
	}
	if active, _ := e.Active(); active == nil {
		t.Fatal("active state must persist while unresolved")
	}

	// "Manually fix" by removing the marker, then Status detects resolution.
	os.Remove(filepath.Join(root, "incidents", "fault-a", "BROKEN"))
	res, err = e.Status(ctx, testRunner(), "")
	if err != nil {
		t.Fatalf("Status after fix: %v", err)
	}
	if !res.Resolved {
		t.Fatalf("expected resolved, got %+v", res.Check)
	}
	if active, _ := e.Active(); active != nil {
		t.Fatal("active state must clear on resolution")
	}
}

func TestInject_SecondIncidentNeedsForce(t *testing.T) {
	e, root := testEngine(t, "fault-a", "fault-b")
	ex := executor.New(root)

	if _, err := e.Inject("fault-a", ex, false, false); err != nil {
		t.Fatal(err)
	}
	_, err := e.Inject("fault-b", ex, false, false)
	if err == nil || !strings.Contains(err.Error(), "fault-a") {
		t.Fatalf("expected active-incident error naming fault-a, got %v", err)
	}
	if _, err := e.Inject("fault-b", ex, true, false); err != nil {
		t.Fatalf("force inject: %v", err)
	}
	if active, _ := e.Active(); active == nil || active.Fault != "fault-b" {
		t.Fatalf("active = %+v", active)
	}
}

// The refusal of a second inject names the active fault only when it was not
// injected silently.
func TestInject_SecondIncidentOverASilentOne(t *testing.T) {
	tests := []struct {
		name     string
		silent   bool
		wantName bool
	}{
		{"an open incident is named", false, true},
		{"a silent incident is not", true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, root := testEngine(t, "fault-a", "fault-b")
			ex := executor.New(root)
			if _, err := e.Inject("fault-a", ex, false, tt.silent); err != nil {
				t.Fatal(err)
			}
			_, err := e.Inject("fault-b", ex, false, false)
			if !errors.Is(err, ErrIncidentActive) {
				t.Fatalf("second inject = %v, want ErrIncidentActive", err)
			}
			if named := strings.Contains(err.Error(), "fault-a"); named != tt.wantName {
				t.Errorf("error %q names fault-a = %v, want %v", err, named, tt.wantName)
			}
			if !strings.Contains(err.Error(), "--force") {
				t.Errorf("error %q does not say how to go on", err)
			}
		})
	}
}

func TestInject_PrereqMissing(t *testing.T) {
	root := t.TempDir() // no apps/go-api
	writeFault(t, root, "fault-a")
	e := NewEngine(root, "k3d.local")

	_, err := e.Inject("fault-a", executor.New(root), false, false)
	if err == nil || !strings.Contains(err.Error(), "go-api") {
		t.Fatalf("expected prereq failure, got %v", err)
	}
	if active, _ := e.Active(); active != nil {
		t.Fatal("failed inject must not record active state")
	}
}

func TestResolve_EscapeHatch(t *testing.T) {
	e, root := testEngine(t, "fault-a")
	ex := executor.New(root)

	if _, err := e.Inject("fault-a", ex, false, false); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Resolve("", ex, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if active, _ := e.Active(); active != nil {
		t.Fatal("resolve must clear active state")
	}
	if _, err := os.Stat(filepath.Join(root, "incidents", "fault-a", "BROKEN")); !os.IsNotExist(err) {
		t.Fatal("resolve.sh must have run")
	}

	// No active incident: bare resolve errors, explicit name still works.
	if _, err := e.Resolve("", ex, ""); err == nil {
		t.Fatal("expected ErrNoActive")
	}
	if _, err := e.Resolve("fault-a", ex, ""); err != nil {
		t.Fatalf("explicit resolve must work without active state: %v", err)
	}
}

func TestPickRandom(t *testing.T) {
	e, _ := testEngine(t, "fault-a", "fault-b", "fault-c")

	first, err := e.PickRandom(42, "", 0, nil)
	if err != nil {
		t.Fatalf("PickRandom: %v", err)
	}
	for range 5 {
		again, _ := e.PickRandom(42, "", 0, nil)
		if again.Name != first.Name {
			t.Fatalf("seeded pick must be deterministic: %q vs %q", again.Name, first.Name)
		}
	}

	if _, err := e.PickRandom(1, "network", 0, nil); err == nil {
		t.Fatal("no eligible faults in category network - expected error")
	}
	if f, err := e.PickRandom(1, "workload", 0, nil); err != nil || f == nil {
		t.Fatalf("category filter: %v", err)
	}
}

func TestPickRandom_NoEligible(t *testing.T) {
	root := t.TempDir() // prereq app missing
	writeFault(t, root, "fault-a")
	e := NewEngine(root, "k3d.local")
	if _, err := e.PickRandom(1, "", 0, nil); err == nil {
		t.Fatal("expected no-eligible error")
	}
}

func TestPickRandom_NotSince(t *testing.T) {
	// writeHistoryRecord appends a results record with an old timestamp to create
	// history that is outside the notSince window.
	writeHistoryRecord := func(e *Engine, faultName string, age time.Duration) {
		now := time.Now().UTC()
		oldStartedAt := now.Add(-age - time.Hour) // Well outside the window
		oldEndedAt := oldStartedAt.Add(time.Hour)
		rec := results.Record{
			Kind:      results.KindIncident,
			Name:      faultName,
			User:      "test-user",
			StartedAt: oldStartedAt,
			EndedAt:   oldEndedAt,
			Elapsed:   int64(time.Hour.Seconds()),
			Score:     -1,
			Outcome:   results.OutcomeResolved,
			Workload:  "go-api",
			Meta: map[string]any{
				"category":   "workload",
				"severity":   "medium",
				"silent":     false,
				"resolvedBy": "fix",
			},
		}
		if err := e.resultsStore().Append(rec); err != nil {
			t.Fatalf("append history record: %v", err)
		}
	}

	tests := []struct {
		name              string
		faults            []string      // faults that exist
		injectedInHistory []string      // faults with recent history (within window)
		historyAge        time.Duration // age of history records for injectedInHistory
		outsideHistory    []string      // faults with old history (outside window)
		notSince          time.Duration
		exclude           []string
		forbiddenFaults   []string // faults that must never be picked
		mustReturnAtLeast []string // faults that must be returned at some seed
	}{
		{
			name:              "recent fault excluded by notSince",
			faults:            []string{"fault-a", "fault-b", "fault-c"},
			injectedInHistory: []string{"fault-a"},
			notSince:          time.Hour,
			forbiddenFaults:   []string{"fault-a"},
		},
		{
			name:            "fault excluded by exclude flag",
			faults:          []string{"fault-a", "fault-b", "fault-c"},
			exclude:         []string{"fault-a"},
			forbiddenFaults: []string{"fault-a"},
		},
		{
			name:              "both notSince and exclude applied",
			faults:            []string{"fault-a", "fault-b", "fault-c", "fault-d"},
			injectedInHistory: []string{"fault-a"},
			exclude:           []string{"fault-b"},
			notSince:          time.Hour,
			forbiddenFaults:   []string{"fault-a", "fault-b"},
		},
		{
			name:              "zero notSince excludes nothing from history",
			faults:            []string{"fault-a", "fault-b"},
			injectedInHistory: []string{"fault-a"},
			notSince:          0,
		},
		{
			name:              "history outside window is not excluded",
			faults:            []string{"fault-a", "fault-b", "fault-c"},
			outsideHistory:    []string{"fault-a"},
			historyAge:        24 * time.Hour,
			notSince:          time.Hour,
			mustReturnAtLeast: []string{"fault-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			createApp(t, root, "go-api")
			for _, f := range tt.faults {
				writeFault(t, root, f)
			}
			e := NewEngine(root, "k3d.local")

			// Create recent history records
			for _, f := range tt.injectedInHistory {
				ex := executor.New(root)
				if _, err := e.Inject(f, ex, false, false); err != nil {
					t.Fatalf("Inject %s: %v", f, err)
				}
				if _, err := e.Resolve("", ex, ""); err != nil {
					t.Fatalf("Resolve: %v", err)
				}
			}

			// Create old history records (outside notSince window)
			for _, f := range tt.outsideHistory {
				writeHistoryRecord(e, f, tt.historyAge)
			}

			// Test seeds 1-200 to verify determinism and exclusions
			for seed := int64(1); seed <= 200; seed++ {
				f, err := e.PickRandom(seed, "", tt.notSince, tt.exclude)
				if err != nil {
					t.Fatalf("PickRandom with seed %d: %v", seed, err)
				}

				// Verify returned fault is not in forbidden set
				for _, forb := range tt.forbiddenFaults {
					if f.Name == forb {
						t.Fatalf("seed %d: forbidden fault %q was picked", seed, f.Name)
					}
				}

				// Same seed + same history = same pick
				f2, err := e.PickRandom(seed, "", tt.notSince, tt.exclude)
				if err != nil {
					t.Fatalf("PickRandom repeat with seed %d: %v", seed, err)
				}
				if f2.Name != f.Name {
					t.Fatalf("seed %d: repeat pick %q != first pick %q", seed, f2.Name, f.Name)
				}
			}

			// Verify faults that "must return at least once" are returned over some seed
			for _, mustFault := range tt.mustReturnAtLeast {
				found := false
				for seed := int64(1); seed <= 200; seed++ {
					f, err := e.PickRandom(seed, "", tt.notSince, tt.exclude)
					if err != nil {
						continue
					}
					if f.Name == mustFault {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("fault %q should be returned at least once over seeds 1-200", mustFault)
				}
			}
		})
	}
}

func TestPickRandom_Exclude(t *testing.T) {
	tests := []struct {
		name              string
		faults            []string
		exclude           []string
		forbiddenFaults   []string
		mustReturnAtLeast []string
	}{
		{
			name:            "single fault excluded",
			faults:          []string{"fault-a", "fault-b", "fault-c"},
			exclude:         []string{"fault-a"},
			forbiddenFaults: []string{"fault-a"},
		},
		{
			name:            "multiple faults excluded",
			faults:          []string{"fault-a", "fault-b", "fault-c"},
			exclude:         []string{"fault-a", "fault-b"},
			forbiddenFaults: []string{"fault-a", "fault-b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			createApp(t, root, "go-api")
			for _, f := range tt.faults {
				writeFault(t, root, f)
			}
			e := NewEngine(root, "k3d.local")

			// Test seeds 1-200 to verify determinism and exclusions
			for seed := int64(1); seed <= 200; seed++ {
				f, err := e.PickRandom(seed, "", 0, tt.exclude)
				if err != nil {
					t.Fatalf("PickRandom with seed %d: %v", seed, err)
				}

				// Verify returned fault is not in forbidden set
				for _, forb := range tt.forbiddenFaults {
					if f.Name == forb {
						t.Fatalf("seed %d: forbidden fault %q was picked", seed, f.Name)
					}
				}

				// Same seed + same history = same pick
				f2, err := e.PickRandom(seed, "", 0, tt.exclude)
				if err != nil {
					t.Fatalf("PickRandom repeat with seed %d: %v", seed, err)
				}
				if f2.Name != f.Name {
					t.Fatalf("seed %d: repeat pick %q != first pick %q", seed, f2.Name, f.Name)
				}
			}

			// Verify faults that "must return at least once" are returned over some seed
			for _, mustFault := range tt.mustReturnAtLeast {
				found := false
				for seed := int64(1); seed <= 200; seed++ {
					f, err := e.PickRandom(seed, "", 0, tt.exclude)
					if err != nil {
						continue
					}
					if f.Name == mustFault {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("fault %q should be returned at least once over seeds 1-200", mustFault)
				}
			}
		})
	}
}

func TestPickRandom_ExcludeUnknownFault(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeFault(t, root, "fault-a")
	e := NewEngine(root, "k3d.local")

	// Try to exclude unknown fault
	_, err := e.PickRandom(42, "", 0, []string{"unknown-fault"})
	if err == nil {
		t.Fatal("expected error for unknown fault in exclude")
	}
	if !strings.Contains(err.Error(), "excluded fault") || !strings.Contains(err.Error(), "unknown-fault") {
		t.Fatalf("error should name the unknown fault: %v", err)
	}
}

func TestPickRandom_NotSinceAndExclude(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeFault(t, root, "fault-a")
	writeFault(t, root, "fault-b")
	writeFault(t, root, "fault-c")
	writeFault(t, root, "fault-d")
	e := NewEngine(root, "k3d.local")

	// Inject fault-a and fault-b to create history
	ex := executor.New(root)
	if _, err := e.Inject("fault-a", ex, false, false); err != nil {
		t.Fatalf("Inject fault-a: %v", err)
	}
	if _, err := e.Resolve("", ex, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, err := e.Inject("fault-b", ex, false, false); err != nil {
		t.Fatalf("Inject fault-b: %v", err)
	}
	if _, err := e.Resolve("", ex, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	// Exclude fault-c, and use notSince to also exclude fault-a and fault-b
	f, err := e.PickRandom(42, "", time.Hour, []string{"fault-c"})
	if err != nil {
		t.Fatalf("PickRandom with notSince and exclude: %v", err)
	}
	// Only fault-d should remain
	if f.Name != "fault-d" {
		t.Fatalf("expected fault-d, got %s", f.Name)
	}
}

func TestPickRandom_AllExcluded(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeFault(t, root, "fault-a")
	writeFault(t, root, "fault-b")
	e := NewEngine(root, "k3d.local")

	// Exclude all faults - should error
	_, err := e.PickRandom(42, "", 0, []string{"fault-a", "fault-b"})
	if err == nil {
		t.Fatal("expected error when all faults excluded")
	}
	if !strings.Contains(err.Error(), "no eligible faults") || !strings.Contains(err.Error(), "filters") {
		t.Fatalf("error should mention filters: %v", err)
	}

	// Verify excluded faults are never picked for seeds 1-200
	for seed := int64(1); seed <= 200; seed++ {
		_, err := e.PickRandom(seed, "", 0, []string{"fault-a", "fault-b"})
		if err == nil {
			t.Fatalf("seed %d: expected error, got success", seed)
		}
	}
}

func TestPickRandom_NotSinceExcludesActive(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeFault(t, root, "fault-a")
	writeFault(t, root, "fault-b")
	e := NewEngine(root, "k3d.local")

	// Inject fault-a (make it active)
	ex := executor.New(root)
	if _, err := e.Inject("fault-a", ex, false, false); err != nil {
		t.Fatalf("Inject fault-a: %v", err)
	}

	// Pick with notSince that covers the active injection time
	f, err := e.PickRandom(42, "", time.Hour, nil)
	if err != nil {
		t.Fatalf("PickRandom with notSince: %v", err)
	}
	// Active fault-a should be excluded, so fault-b should be picked
	if f.Name != "fault-b" {
		t.Fatalf("expected fault-b (fault-a is active), got %s", f.Name)
	}

	// Resolve and try again - now fault-a is in history and should still be excluded
	if _, err := e.Resolve("", ex, ""); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	f, err = e.PickRandom(42, "", time.Hour, nil)
	if err != nil {
		t.Fatalf("PickRandom after resolve: %v", err)
	}
	if f.Name != "fault-b" {
		t.Fatalf("expected fault-b (fault-a in history), got %s", f.Name)
	}
}

func TestResolveCheckTemplates(t *testing.T) {
	e := NewEngine(t.TempDir(), "lab.example")
	c := e.resolveCheck(checks.Check{Name: "x", Type: "http", URL: "http://go-api.{{.DomainSuffix}}/health"})
	if c.URL != "http://go-api.lab.example/health" {
		t.Fatalf("url not resolved: %q", c.URL)
	}
}

// The challenge grader calls ResolveCheck, so it must expand the check's URL.
func TestResolveCheck_Exported(t *testing.T) {
	e := NewEngine(t.TempDir(), "lab.example")
	tests := []struct {
		name string
		in   checks.Check
		want checks.Check
	}{
		{
			name: "http url",
			in:   checks.Check{Name: "x", Type: "http", URL: "http://go-api.{{.DomainSuffix}}/health"},
			want: checks.Check{Name: "x", Type: "http", URL: "http://go-api.lab.example/health"},
		},
		{
			name: "kubectl resource and namespace",
			in:   checks.Check{Name: "y", Type: "kubectl", Resource: "svc/{{.DomainSuffix}}", Namespace: "{{.DomainSuffix}}"},
			want: checks.Check{Name: "y", Type: "kubectl", Resource: "svc/lab.example", Namespace: "lab.example"},
		},
		{
			name: "no templates is unchanged",
			in:   checks.Check{Name: "z", Type: "http", URL: "http://go-api.k3d.local/health"},
			want: checks.Check{Name: "z", Type: "http", URL: "http://go-api.k3d.local/health"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := e.ResolveCheck(tt.in)
			if got != tt.want {
				t.Fatalf("ResolveCheck() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

// A fault injected into one app must be detected and resolved against that
// app, even when the current binding names another.
func TestActiveRecordsTheAppItWasInjectedInto(t *testing.T) {
	e, _ := testEngine(t, "marker-fault")
	e.Workload = workload.Default("java-api")
	if err := e.MarkInjected("marker-fault", false); err != nil {
		t.Fatalf("MarkInjected: %v", err)
	}
	active, err := e.Active()
	if err != nil || active == nil {
		t.Fatalf("Active: %v (active=%v)", err, active)
	}
	if active.App != "java-api" {
		t.Errorf("Active.App = %q, want java-api", active.App)
	}
}

// Describing a fault for another app must not change the shared engine's
// binding.
func TestListBoundResolvesWithoutRebindingTheEngine(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeBoundFault(t, root, "bound-fault")
	e := NewEngine(root, "k3d.local")
	e.Workload = workload.Default("go-api")

	faults := e.ListBound(workload.Default("java-api"))
	if len(faults) != 1 {
		t.Fatalf("got %d faults, want 1", len(faults))
	}
	f := faults[0]
	if f.Target.Namespace != "java-api" || f.Target.Workload != "java-api" {
		t.Errorf("target = %s/%s, want java-api/java-api", f.Target.Namespace, f.Target.Workload)
	}
	if !strings.Contains(f.Description, "java-api") {
		t.Errorf("description was not resolved against the binding: %q", f.Description)
	}
	if e.Workload.Name != "go-api" {
		t.Errorf("ListBound rebound the engine to %q", e.Workload.Name)
	}
}

// Every field shown to a reader, snippets included, must have its templates
// expanded.
func TestResolvedCoversEverythingAReaderSees(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeBoundFault(t, root, "bound-fault")
	e := NewEngine(root, "k3d.local")
	e.Workload = workload.Default("java-api")

	f, err := e.Get("bound-fault")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	for label, got := range map[string]string{
		"description":     f.Description,
		"snippet label":   f.Snippets[0].Label,
		"snippet body":    f.Snippets[0].YAML,
		"reference note":  f.References[0].Note,
		"prerequisite ap": f.Prerequisites.Apps[0],
	} {
		if strings.Contains(got, "{{") {
			t.Errorf("%s left a raw template: %q", label, got)
		}
	}
	if f.Prerequisites.Apps[0] != "java-api" {
		t.Errorf("prerequisite app = %q, want java-api", f.Prerequisites.Apps[0])
	}
}

// An app prerequisite written as a template follows the binding, so it must
// not be reported as a fixed requirement.
func TestPinnedAppsExcludesTheBinding(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeBoundFault(t, root, "bound-fault")
	e := NewEngine(root, "k3d.local")

	if got := e.PinnedApps("bound-fault"); len(got) != 0 {
		t.Errorf("PinnedApps = %v, want none - the only entry is the binding", got)
	}
	if got := e.PinnedApps("no-such-fault"); got == nil || len(got) != 0 {
		t.Errorf("PinnedApps for an unknown fault = %v, want an empty slice", got)
	}
}

const boundFaultYAML = `name: %s
displayName: "Bound Fault"
description: "{{.WorkloadName}} is on fire"
category: workload
severity: medium
target:
  namespace: "{{.WorkloadNamespace}}"
  workload: "{{.WorkloadName}}"
prerequisites:
  apps:
    - "{{.WorkloadName}}"
references:
  - label: "Docs"
    url: "https://example.invalid/"
    note: "how {{.WorkloadName}} is wired"
snippets:
  - label: "Patch {{.WorkloadName}}"
    yaml: |
      metadata:
        namespace: "{{.WorkloadNamespace}}"
detection:
  name: resolved-when-marker-gone
  type: script
  script: checks/resolved.sh
  timeoutSeconds: 5
`

// writeBoundFault creates a fault that names its workload through the binding
// rather than pinning an app - the shape all shipped content uses.
func writeBoundFault(t *testing.T, root, name string) {
	t.Helper()
	writeFault(t, root, name)
	path := filepath.Join(root, "incidents", name, "fault.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(boundFaultYAML, "%s", name, 1)), 0644); err != nil {
		t.Fatal(err)
	}
}

// Every string a reader is shown must have its templates expanded. The test
// walks the whole response, so a newly added field is covered too.
func TestResolvedLeavesNoTemplateAReaderCouldSee(t *testing.T) {
	root := t.TempDir()
	createApp(t, root, "go-api")
	writeBoundFault(t, root, "bound-fault")
	e := NewEngine(root, "k3d.local")

	faults := e.ListBound(workload.Default("java-api"))
	if len(faults) != 1 {
		t.Fatalf("got %d faults, want 1", len(faults))
	}
	f := faults[0]
	body, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if left := regexp.MustCompile(`{{\.\w[^}]*}}`).FindAllString(string(body), -1); len(left) > 0 {
		t.Errorf("unresolved templates reached the reader: %v", left)
	}
	if !strings.Contains(f.Description, "java-api") {
		t.Errorf("resolved against the wrong binding: %q", f.Description)
	}
}

// A status request that is cancelled before its check finishes never
// resolves the incident, even on a fixed lab: the check did not pass, it was
// not run.
func TestStatus_CancelledContextNeverResolves(t *testing.T) {
	e, root := testEngine(t, "fault-a")
	if _, err := e.Inject("fault-a", executor.New(root), false, false); err != nil {
		t.Fatalf("Inject: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "incidents", "fault-a", "BROKEN")); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	res, err := e.Status(ctx, testRunner(), "")
	if err == nil && res.Resolved {
		t.Fatal("a cancelled status check resolved the incident")
	}
	if active, _ := e.Active(); active == nil {
		t.Error("a cancelled status check cleared the active incident")
	}
}

// Detection is what a challenge grades with, so it must hand the check the
// same target environment Status runs it with.
func TestDetection(t *testing.T) {
	e, root := testEngine(t, "demo")
	e.DomainSuffix = "lab.test"
	tests := []struct {
		name    string
		fault   string
		wantErr bool
	}{
		{"a fault's check, its directory and its target", "demo", false},
		{"an unknown fault", "ghost", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k, dir, env, err := e.Detection(tt.fault)
			if tt.wantErr {
				if err == nil {
					t.Fatal("Detection() of an unknown fault returned no error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if k.Name != "resolved-when-marker-gone" || dir != filepath.Join(root, "incidents", "demo") {
				t.Errorf("check %q from %q, want resolved-when-marker-gone from the fault's directory", k.Name, dir)
			}
			joined := strings.Join(env, " ")
			for _, want := range []string{"TARGET_NAMESPACE=go-api", "TARGET_WORKLOAD=go-api", "DOMAIN_SUFFIX=lab.test"} {
				if !strings.Contains(joined, want) {
					t.Errorf("env %q is missing %s", joined, want)
				}
			}
		})
	}
}

func TestInject_SilentFlagReachesTheScript(t *testing.T) {
	tests := []struct {
		name   string
		silent bool
		want   string
	}{
		{"silent inject tells the script", true, "1"},
		{"visible inject leaves it clear", false, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, root := testEngine(t, "fault-a")
			out := filepath.Join(root, "silent.out")
			script := "#!/bin/sh\nprintf '%s' \"$FAULT_SILENT\" > " + out + "\n"
			if err := os.WriteFile(filepath.Join(root, "incidents", "fault-a", "inject.sh"), []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			ex := executor.New(root)
			// A silent inject earlier on the same executor must not leak into this one.
			ex.SetEnv("FAULT_SILENT", "1")
			if _, err := e.Inject("fault-a", ex, false, tt.silent); err != nil {
				t.Fatalf("Inject: %v", err)
			}
			got, err := os.ReadFile(out)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tt.want {
				t.Fatalf("FAULT_SILENT = %q, want %q", got, tt.want)
			}
		})
	}
}
