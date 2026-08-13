package watch

import (
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// isGitHubActionsPath reports whether rel points at a GitHub Actions workflow file.
func isGitHubActionsPath(rel string) bool {
	lower := strings.ToLower(filepath.ToSlash(rel))
	return strings.Contains(lower, ".github/workflows/") &&
		(strings.HasSuffix(lower, ".yml") || strings.HasSuffix(lower, ".yaml"))
}

// isGitHubActionManifest reports whether rel points at a composite action manifest
// (action.yml / action.yaml), which may live anywhere in the repo.
func isGitHubActionManifest(rel string) bool {
	switch strings.ToLower(filepath.Base(rel)) {
	case "action.yml", "action.yaml":
		return true
	default:
		return false
	}
}

// scanGitHubWorkflow decodes one or more workflow documents from f. GitHub
// workflow files carry no kind:/services: markers, so they are dispatched by
// path rather than by the generic runtime YAML sniff.
func (c *architectureCollector) scanGitHubWorkflow(f *os.File, rel string) {
	dec := yaml.NewDecoder(f)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		if len(doc) == 0 {
			continue
		}
		c.consumeGitHubWorkflow(doc, rel)
	}
}

// scanGitHubActionManifest decodes composite action manifests so that local
// actions referenced via `uses: ./...` get a stable component even when the
// referencing workflow lives in another repository.
func (c *architectureCollector) scanGitHubActionManifest(f *os.File, rel string) {
	dec := yaml.NewDecoder(f)
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			return
		}
		if err != nil {
			return
		}
		if len(doc) == 0 {
			continue
		}
		c.consumeGitHubActionManifest(doc, rel)
	}
}

// consumeGitHubWorkflow models a workflow document as a component, its jobs as
// components, `needs:` as job dependencies and step `uses:` as action or
// reusable-workflow dependencies.
func (c *architectureCollector) consumeGitHubWorkflow(doc map[string]any, rel string) {
	name := stringValue(doc["name"])
	if name == "" {
		name = strings.TrimSuffix(path.Base(rel), path.Ext(rel))
	}
	if name == "" {
		return
	}
	key := architectureKey("component", name)
	workflow := c.ensureComponent(key, name, "service", "GitHub Actions", rel, architectureEvidence{Kind: "cicd-workflow", Path: rel, Note: "GitHub Actions workflow"})
	workflow.Tags = appendUnique(workflow.Tags, "arch:workflow", "cicd:github")
	for _, trigger := range githubWorkflowTriggers(doc["on"]) {
		workflow.Tags = appendUnique(workflow.Tags, "trigger:"+architectureSlug(trigger))
	}

	jobs := mapValue(doc["jobs"])
	if jobs == nil {
		return
	}
	jobKeys := make(map[string]string, len(jobs))
	for jobName, raw := range jobs {
		job := mapValue(raw)
		if job == nil {
			continue
		}
		jobKey := architectureKey("component", jobName)
		jobComponent := c.ensureComponent(jobKey, jobName, "service", "GitHub Actions", rel, architectureEvidence{Kind: "cicd-job", Path: rel, Note: name})
		jobComponent.Tags = appendUnique(jobComponent.Tags, "arch:workflow", "cicd:github")
		jobKeys[jobName] = jobKey
	}

	// Resolve needs and step uses after all jobs are registered so declaration
	// order does not matter.
	for jobName, raw := range jobs {
		job := mapValue(raw)
		if job == nil {
			continue
		}
		jobKey := jobKeys[jobName]
		for _, need := range stringListOrString(job["needs"]) {
			c.addConnector(architectureKey("component", need), jobKey, "needs", "cicd-dependency", 0.8, architectureEvidence{Kind: "cicd-needs", Path: rel, Note: name})
		}
		for _, stepRaw := range sliceValue(job["steps"]) {
			step := mapValue(stepRaw)
			if step == nil {
				continue
			}
			c.consumeGitHubUses(jobKey, key, stringValue(step["uses"]), rel)
		}
	}
}

// consumeGitHubActionManifest registers a composite action manifest as a
// component. Local actions are identified by their directory (which is what
// `uses: ./...` references), falling back to the manifest name.
func (c *architectureCollector) consumeGitHubActionManifest(doc map[string]any, rel string) {
	name := stringValue(doc["name"])
	dir := path.Dir(rel)
	if dir != "." && dir != "" {
		name = firstNonEmpty(path.Base(dir), name)
	}
	if name == "" {
		return
	}
	key := architectureKey("component", name)
	component := c.ensureComponent(key, name, "service", "GitHub Actions", rel, architectureEvidence{Kind: "cicd-action", Path: rel, Note: "composite action"})
	component.Tags = appendUnique(component.Tags, "arch:action", "cicd:github", "cicd:local")
}

// consumeGitHubUses models a step's `uses:` reference. Reusable workflows form
// a workflow-to-workflow "calls" edge; actions (local, container or
// third-party) form job-to-action "uses" edges. Expression-valued references
// are skipped.
func (c *architectureCollector) consumeGitHubUses(jobKey, workflowKey, uses, rel string) {
	if uses == "" || strings.Contains(uses, "${{") {
		return
	}
	uses = strings.TrimSpace(uses)

	var (
		componentKey  string
		componentName string
		componentKind string
		connector     string
		confidence    float64
		evidenceKind  string
	)
	switch {
	case strings.HasPrefix(uses, "./"):
		componentName = path.Base(strings.TrimSuffix(uses, "/"))
		componentKey = architectureKey("component", componentName)
		componentKind = "service"
		connector = "uses"
		confidence = 0.85
		evidenceKind = "cicd-local-action"
	case strings.HasPrefix(uses, "docker://"):
		image := strings.Split(strings.TrimPrefix(uses, "docker://"), "@")[0]
		if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
			image = image[:i] // strip tag, keeping registry ports intact
		}
		componentName = path.Base(image)
		componentKey = architectureKey("external", componentName)
		componentKind = "external"
		connector = "uses"
		confidence = 0.8
		evidenceKind = "cicd-container-action"
	default:
		ref := strings.SplitN(uses, "@", 2)[0]
		if strings.Contains(ref, "/.github/workflows/") {
			// Reusable workflow, e.g. octo-org/repo/.github/workflows/ci.yml@main.
			componentName = strings.TrimSuffix(path.Base(ref), path.Ext(ref))
			componentKey = architectureKey("component", componentName)
			componentKind = "service"
			connector = "calls"
			confidence = 0.85
			evidenceKind = "cicd-reusable-workflow"
			c.ensureComponent(componentKey, componentName, componentKind, "GitHub Actions", rel, architectureEvidence{Kind: evidenceKind, Path: rel, Note: uses})
			c.addConnector(workflowKey, componentKey, connector, "cicd-dependency", confidence, architectureEvidence{Kind: evidenceKind, Path: rel, Note: uses})
			return
		}
		// Third-party action, e.g. actions/checkout@v4.
		componentName = path.Base(ref)
		componentKey = architectureKey("external", componentName)
		componentKind = "external"
		connector = "uses"
		confidence = 0.8
		evidenceKind = "cicd-third-party-action"
	}

	if componentName == "" || componentName == "." {
		return
	}
	c.ensureComponent(componentKey, componentName, componentKind, "GitHub Actions", rel, architectureEvidence{Kind: evidenceKind, Path: rel, Note: uses})
	c.addConnector(jobKey, componentKey, connector, "cicd-dependency", confidence, architectureEvidence{Kind: evidenceKind, Path: rel, Note: uses})
}

// githubWorkflowTriggers extracts the event names from a workflow `on:` value,
// which may be a single string, a list, or a map of events.
func githubWorkflowTriggers(raw any) []string {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		return stringList(v)
	case map[string]any:
		out := make([]string, 0, len(v))
		for key := range v {
			out = append(out, key)
		}
		sort.Strings(out)
		return out
	default:
		return nil
	}
}

// stringListOrString accepts both the single-string and list forms used by
// GitHub Actions fields such as `needs:`.
func stringListOrString(raw any) []string {
	if value := stringValue(raw); value != "" {
		return []string{value}
	}
	return stringList(raw)
}
