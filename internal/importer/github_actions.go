package importer

import (
	"io"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var (
	ghOnRE   = regexp.MustCompile(`(?m)^\s*on\s*:`)
	ghJobsRE = regexp.MustCompile(`(?m)^\s*jobs\s*:`)
)

// looksLikeGitHubWorkflow reports whether the input looks like a GitHub
// Actions workflow: it must declare both a top-level `on:` trigger and a
// `jobs:` map within the first 8KB.
func looksLikeGitHubWorkflow(input string) bool {
	if len(input) > 8*1024 {
		input = input[:8*1024]
	}
	return ghOnRE.MatchString(input) && ghJobsRE.MatchString(input)
}

// ParseGitHubWorkflow parses one or more GitHub Actions workflow documents
// into a workspace. Workflows, jobs and local actions become service
// elements; third-party and container actions become external elements.
// `needs:` produces job dependencies, step `uses:` produces job→action
// dependencies, and reusable workflow references produce workflow→workflow
// "calls" dependencies.
func ParseGitHubWorkflow(input string) (*ParsedWorkspace, error) {
	ws := &ParsedWorkspace{}

	elements := map[string]*ParsedElement{}
	var elementOrder []string
	connectors := map[string]ParsedConnector{}
	var connectorOrder []string

	ensureElement := func(id, name, kind string) {
		if _, ok := elements[id]; ok {
			return
		}
		elements[id] = &ParsedElement{ID: id, Name: name, Kind: kind, Technology: "GitHub Actions"}
		elementOrder = append(elementOrder, id)
	}

	addConnector := func(source, target, label string) {
		key := source + "\x00" + target + "\x00" + label
		if _, ok := connectors[key]; ok {
			return
		}
		connectors[key] = ParsedConnector{SourceID: source, TargetID: target, Label: label, Technology: "cicd-dependency"}
		connectorOrder = append(connectorOrder, key)
	}

	dec := yaml.NewDecoder(strings.NewReader(input))
	docIndex := 0
	for {
		var doc map[string]any
		err := dec.Decode(&doc)
		if err == io.EOF {
			break
		}
		if err != nil {
			ws.Warnings = append(ws.Warnings, "skipped invalid workflow document: "+err.Error())
			break
		}
		if len(doc) == 0 {
			continue
		}
		consumeGitHubWorkflowDoc(doc, docIndex, ensureElement, addConnector)
		docIndex++
	}

	for _, id := range elementOrder {
		ws.Elements = append(ws.Elements, *elements[id])
	}
	for _, key := range connectorOrder {
		ws.Connectors = append(ws.Connectors, connectors[key])
	}
	return ws, nil
}

func consumeGitHubWorkflowDoc(doc map[string]any, docIndex int, ensureElement func(id, name, kind string), addConnector func(source, target, label string)) {
	name := ghStringValue(doc["name"])
	if name == "" {
		name = "Workflow"
		if docIndex > 0 {
			name += " " + itoa(docIndex+1)
		}
	}
	workflowID := "workflow:" + importSlug(name)
	ensureElement(workflowID, name, "service")

	jobs := ghMapValue(doc["jobs"])
	if jobs == nil {
		return
	}

	// Register every job first so `needs:` resolution is order-independent.
	jobIDs := make(map[string]string, len(jobs))
	for jobName := range jobs {
		jobID := "job:" + importSlug(jobName)
		jobIDs[jobName] = jobID
		ensureElement(jobID, jobName, "service")
	}

	for jobName, raw := range jobs {
		job := ghMapValue(raw)
		if job == nil {
			continue
		}
		jobID := jobIDs[jobName]

		for _, need := range ghStringList(job["needs"]) {
			if target, ok := jobIDs[need]; ok {
				addConnector(target, jobID, "needs")
			}
		}
		for _, stepRaw := range ghSlice(job["steps"]) {
			step := ghMapValue(stepRaw)
			if step == nil {
				continue
			}
			consumeGitHubUses(workflowID, jobID, ghStringValue(step["uses"]), ensureElement, addConnector)
		}
	}
}

// consumeGitHubUses models a step's `uses:` reference. Expression-valued
// references are skipped.
func consumeGitHubUses(workflowID, jobID, uses string, ensureElement func(id, name, kind string), addConnector func(source, target, label string)) {
	if uses == "" || strings.Contains(uses, "${{") {
		return
	}
	uses = strings.TrimSpace(uses)

	var (
		elementID  string
		elementName string
		elementKind string
		connector  string
	)
	switch {
	case strings.HasPrefix(uses, "./"):
		elementName = path.Base(strings.TrimSuffix(uses, "/"))
		elementID = "action:" + importSlug(elementName)
		elementKind = "service"
		connector = "uses"
	case strings.HasPrefix(uses, "docker://"):
		image := strings.Split(strings.TrimPrefix(uses, "docker://"), "@")[0]
		if i := strings.LastIndex(image, ":"); i > strings.LastIndex(image, "/") {
			image = image[:i] // strip tag, keeping registry ports intact
		}
		elementName = path.Base(image)
		elementID = "external:" + importSlug(elementName)
		elementKind = "external"
		connector = "uses"
	default:
		ref := strings.SplitN(uses, "@", 2)[0]
		if strings.Contains(ref, "/.github/workflows/") {
			// Reusable workflow, e.g. octo-org/repo/.github/workflows/ci.yml@main.
			elementName = strings.TrimSuffix(path.Base(ref), path.Ext(ref))
			elementID = "reusable:" + importSlug(elementName)
			elementKind = "service"
			ensureElement(elementID, elementName, elementKind)
			addConnector(workflowID, elementID, "calls")
			return
		}
		// Third-party action, e.g. actions/checkout@v4.
		elementName = path.Base(ref)
		elementID = "external:" + importSlug(elementName)
		elementKind = "external"
		connector = "uses"
	}

	if elementName == "" || elementName == "." {
		return
	}
	ensureElement(elementID, elementName, elementKind)
	addConnector(jobID, elementID, connector)
}

func ghStringValue(raw any) string {
	if s, ok := raw.(string); ok {
		return s
	}
	return ""
}

func ghMapValue(raw any) map[string]any {
	if m, ok := raw.(map[string]any); ok {
		return m
	}
	return nil
}

func ghSlice(raw any) []any {
	if s, ok := raw.([]any); ok {
		return s
	}
	return nil
}

// ghStringList accepts both the single-string and list forms used by GitHub
// Actions fields such as `needs:`.
func ghStringList(raw any) []string {
	if s, ok := raw.(string); ok && s != "" {
		return []string{s}
	}
	var out []string
	for _, item := range ghSlice(raw) {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// importSlug lowercases s and replaces runs of non-alphanumeric characters
// with a single hyphen.
func importSlug(s string) string {
	var sb strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			sb.WriteRune('-')
			lastDash = true
		}
	}
	return strings.Trim(sb.String(), "-")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
