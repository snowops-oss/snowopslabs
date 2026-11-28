// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/snowops-oss/snowopslabs/internal/challenge"
	"github.com/snowops-oss/snowopslabs/internal/incident"
	"github.com/snowops-oss/snowopslabs/internal/learn"
	"github.com/snowops-oss/snowopslabs/internal/target"
	"github.com/snowops-oss/snowopslabs/internal/workload"
	"github.com/snowops-oss/snowopslabs/pkg/scenario"
)

// kindDir maps each kind to the directory and item filename it lives in under a
// content root.
var kindLayout = map[Kind]struct{ dir, file string }{
	KindScenario:  {"scenarios", "scenario.yaml"},
	KindIncident:  {"incidents", "fault.yaml"},
	KindPath:      {"learn", "path.yaml"},
	KindChallenge: {"challenges", "challenge.yaml"},
}

// Load builds a validated snapshot from the given content roots. When two roots
// define an item with the same name, the earlier root wins and the later item
// is a problem, matching how the engines load content. Content errors are
// collected in the Catalog's Problems rather than stopping the load; the
// returned error is only for a root that cannot be read.
func Load(roots ...string) (*Catalog, error) {
	c := newCatalog(roots)
	for _, root := range roots {
		for _, kind := range AllKinds {
			c.loadKind(root, kind)
		}
	}
	c.crossReference()
	c.validateTemplates(defaultProjectRoot(roots))
	return c, nil
}

func defaultProjectRoot(roots []string) string {
	if len(roots) > 0 {
		return roots[0]
	}
	return "."
}

// loadKind scans one kind's directory under one root and loads every item.
func (c *Catalog) loadKind(root string, kind Kind) {
	layout := kindLayout[kind]
	dir := filepath.Join(root, layout.dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Not every root has every kind; an external root may hold only
		// scenarios.
		return
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		file := filepath.Join(dir, entry.Name(), layout.file)
		if _, err := os.Stat(file); err != nil {
			continue // directory without the kind's item file - skip quietly
		}
		c.loadItem(kind, entry.Name(), file)
	}
}

// loadItem decodes, validates and indexes a single content item.
func (c *Catalog) loadItem(kind Kind, dirName, file string) {
	data, err := os.ReadFile(file)
	if err != nil {
		c.problems = append(c.problems, Problem{Kind: kind, Name: dirName, File: file, Message: err.Error()})
		return
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		c.problems = append(c.problems, Problem{Kind: kind, Name: dirName, File: file, Message: "parsing YAML: " + err.Error()})
		return
	}

	switch kind {
	case KindScenario:
		c.loadScenario(dirName, file, &doc)
	case KindIncident:
		c.loadIncident(dirName, file, &doc)
	case KindPath:
		c.loadPath(dirName, file, &doc)
	case KindChallenge:
		c.loadChallenge(dirName, file, &doc)
	}
}

func (c *Catalog) loadScenario(dirName, file string, doc *yaml.Node) {
	var s scenario.Scenario
	if err := doc.Decode(&s); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindScenario, Name: dirName, File: file, Message: "decoding: " + err.Error()})
		return
	}
	s.Dir = filepath.Dir(file)
	s.ApplyDefaults()
	c.checkName(KindScenario, dirName, s.Name, file, doc)
	if err := s.Validate(); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindScenario, Name: nameOr(s.Name, dirName), File: file, Line: problemLine(doc, err.Error()), Message: err.Error()})
	}
	c.checkRequires(KindScenario, nameOr(s.Name, dirName), file, doc, s.Needs)
	// Reject unknown capabilities now; a misspelt one could never be met.
	for _, name := range s.Prerequisites.Capabilities {
		if _, err := workload.ParseCapability(name); err != nil {
			c.problems = append(c.problems, Problem{
				Kind: KindScenario, Name: nameOr(s.Name, dirName), File: file,
				Line: lineOfValue(doc, name), Message: "prerequisite capability: " + err.Error(),
			})
		}
	}
	c.index(KindScenario, dirName, file, doc, func() {
		s.Source = ""
		c.scenarios[dirName] = &s
	})
}

func (c *Catalog) loadIncident(dirName, file string, doc *yaml.Node) {
	var f incident.Fault
	if err := doc.Decode(&f); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindIncident, Name: dirName, File: file, Message: "decoding: " + err.Error()})
		return
	}
	f.Dir = filepath.Dir(file)
	f.ApplyDefaults()
	c.checkName(KindIncident, dirName, f.Name, file, doc)
	if err := f.Validate(); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindIncident, Name: nameOr(f.Name, dirName), File: file, Line: problemLine(doc, err.Error()), Message: err.Error()})
	}
	c.checkRequires(KindIncident, nameOr(f.Name, dirName), file, doc, f.Needs)
	c.index(KindIncident, dirName, file, doc, func() { c.incidents[dirName] = &f })
}

func (c *Catalog) loadPath(dirName, file string, doc *yaml.Node) {
	var p learn.Path
	if err := doc.Decode(&p); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindPath, Name: dirName, File: file, Message: "decoding: " + err.Error()})
		return
	}
	p.Normalize()
	c.checkName(KindPath, dirName, p.Name, file, doc)
	for _, msg := range p.Validate() {
		c.problems = append(c.problems, Problem{Kind: KindPath, Name: nameOr(p.Name, dirName), File: file, Line: problemLine(doc, msg), Message: msg})
	}
	c.checkPathTargets(nameOr(p.Name, dirName), file, doc, &p)
	c.index(KindPath, dirName, file, doc, func() { c.paths[dirName] = &p })
}

func (c *Catalog) loadChallenge(dirName, file string, doc *yaml.Node) {
	var ch challenge.Challenge
	if err := doc.Decode(&ch); err != nil {
		c.problems = append(c.problems, Problem{Kind: KindChallenge, Name: dirName, File: file, Message: "decoding: " + err.Error()})
		return
	}
	ch.ApplyDefaults()
	c.checkName(KindChallenge, dirName, ch.Name, file, doc)
	for _, msg := range ch.Validate() {
		c.problems = append(c.problems, Problem{Kind: KindChallenge, Name: nameOr(ch.Name, dirName), File: file, Line: problemLine(doc, msg), Message: msg})
	}
	c.checkRequires(KindChallenge, nameOr(ch.Name, dirName), file, doc, ch.Needs)
	c.index(KindChallenge, dirName, file, doc, func() { c.challenges[dirName] = &ch })
}

// checkName reports an item whose declared name differs from its directory
// name. Engines find an item by its name, so the two must match.
func (c *Catalog) checkName(kind Kind, dirName, declared, file string, doc *yaml.Node) {
	if declared != "" && declared != dirName {
		c.problems = append(c.problems, Problem{
			Kind: kind, Name: dirName, File: file, Line: lineOfValue(doc, declared),
			Message: fmt.Sprintf("name %q must match its directory name %q", declared, dirName),
		})
	}
}

// index stores the item with its source file and YAML document. An item with
// the same name as one from an earlier root is skipped and reported, since
// every engine keeps the earlier one too.
func (c *Catalog) index(kind Kind, name, file string, doc *yaml.Node, store func()) {
	key := sourceKey(kind, name)
	if first, ok := c.sources[key]; ok {
		c.problems = append(c.problems, Problem{
			Kind: kind, Name: name, File: file,
			Message: fmt.Sprintf("name %q already provided by %s; this copy is skipped - rename it to load both", name, filepath.Dir(first)),
		})
		return
	}
	c.sources[key] = file
	c.nodes[key] = doc
	store()
}

func nameOr(name, fallback string) string {
	if name != "" {
		return name
	}
	return fallback
}

// metaFields maps the start of each track, level and minutes problem message
// to the YAML key it is about.
var metaFields = []struct{ prefix, key string }{
	{"track \"", "track"},
	{"level \"", "level"},
	{"minutes ", "minutes"},
	{"estimatedMinutes ", "estimatedMinutes"},
}

// targetInMessage finds the target name a problem message is about.
var targetInMessage = regexp.MustCompile(`target "([^"]+)"`)

// checkRequires reports every requires entry that is not a target this lab
// can supply. The list of known targets is the registry's, so a new target is
// accepted here once it is registered.
func (c *Catalog) checkRequires(kind Kind, name, file string, doc *yaml.Node, needs scenario.Needs) {
	known := target.NewRegistry(nil)
	for _, t := range needs.Requires {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if _, err := known.Lookup(t); err != nil {
			c.problems = append(c.problems, Problem{
				Kind: kind, Name: name, File: file, Line: lineOfValue(doc, t),
				Message: fmt.Sprintf("requires: unknown target %q (use %s)", t, strings.Join(known.Names(), ", ")),
			})
		}
	}
}

// checkPathTargets reports a module check whose target is not a known target,
// or is not the default one: a path has no requires, so only k8s is available
// to its checks.
func (c *Catalog) checkPathTargets(name, file string, doc *yaml.Node, p *learn.Path) {
	known := target.NewRegistry(nil)
	for i, m := range p.Modules {
		t := strings.TrimSpace(m.Check.Target)
		if t == "" || t == target.DefaultName {
			continue
		}
		msg := fmt.Sprintf("modules[%d]: check target %q is not available: a learning path has no requires and uses %s", i, t, target.DefaultName)
		if _, err := known.Lookup(t); err != nil {
			msg = fmt.Sprintf("modules[%d]: check target %q is unknown (use %s)", i, t, strings.Join(known.Names(), ", "))
		}
		c.problems = append(c.problems, Problem{Kind: KindPath, Name: name, File: file, Line: lineOfValue(doc, t), Message: msg})
	}
}

// problemLine returns the line of the key or value a validation message is
// about: the track, level or minutes key, or a target name. It is 0 when the
// message is about something else.
func problemLine(doc *yaml.Node, msg string) int {
	if doc == nil || len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return 0
	}
	if m := targetInMessage.FindStringSubmatch(msg); m != nil {
		return lineOfValue(doc, m[1])
	}
	for _, f := range metaFields {
		if !strings.Contains(msg, f.prefix) {
			continue
		}
		top := doc.Content[0].Content
		for i := 0; i+1 < len(top); i += 2 {
			if top[i].Value == f.key {
				return top[i].Line
			}
		}
	}
	return 0
}

// lineOfValue returns the 1-based line of the first scalar node in doc whose
// value equals want, or 0 if none is found. It lets a cross-reference or naming
// problem point the author at the exact line.
func lineOfValue(doc *yaml.Node, want string) int {
	var walk func(n *yaml.Node) int
	walk = func(n *yaml.Node) int {
		if n == nil {
			return 0
		}
		if n.Kind == yaml.ScalarNode && n.Value == want {
			return n.Line
		}
		for _, ch := range n.Content {
			if line := walk(ch); line != 0 {
				return line
			}
		}
		return 0
	}
	return walk(doc)
}
