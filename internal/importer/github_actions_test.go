package importer

import (
	"reflect"
	"sort"
	"testing"
)

func sortElements(els []ParsedElement) {
	sort.Slice(els, func(i, j int) bool { return els[i].ID < els[j].ID })
}

func sortConnectors(cs []ParsedConnector) {
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].SourceID != cs[j].SourceID {
			return cs[i].SourceID < cs[j].SourceID
		}
		if cs[i].TargetID != cs[j].TargetID {
			return cs[i].TargetID < cs[j].TargetID
		}
		return cs[i].Label < cs[j].Label
	})
}

func TestParseGitHubWorkflow(t *testing.T) {
	input := `
name: CI
on:
  push:
    branches: [main]
  pull_request:
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-node@v4
      - uses: ./.github/actions/notify
  deploy:
    runs-on: ubuntu-latest
    needs: test
    steps:
      - uses: octo-org/repo/.github/workflows/release.yml@main
      - uses: docker://alpine:latest
`

	got, err := ParseGitHubWorkflow(input)
	if err != nil {
		t.Fatalf("ParseGitHubWorkflow returned error: %v", err)
	}

	wantElements := []ParsedElement{
		{ID: "workflow:ci", Name: "CI", Kind: "service", Technology: "GitHub Actions"},
		{ID: "job:test", Name: "test", Kind: "service", Technology: "GitHub Actions"},
		{ID: "job:deploy", Name: "deploy", Kind: "service", Technology: "GitHub Actions"},
		{ID: "external:checkout", Name: "checkout", Kind: "external", Technology: "GitHub Actions"},
		{ID: "external:setup-node", Name: "setup-node", Kind: "external", Technology: "GitHub Actions"},
		{ID: "action:notify", Name: "notify", Kind: "service", Technology: "GitHub Actions"},
		{ID: "reusable:release", Name: "release", Kind: "service", Technology: "GitHub Actions"},
		{ID: "external:alpine", Name: "alpine", Kind: "external", Technology: "GitHub Actions"},
	}
	sortElements(got.Elements)
	sortElements(wantElements)
	if !reflect.DeepEqual(got.Elements, wantElements) {
		t.Fatalf("elements mismatch\n got: %#v\nwant: %#v", got.Elements, wantElements)
	}

	wantConnectors := []ParsedConnector{
		{SourceID: "job:test", TargetID: "job:deploy", Label: "needs", Technology: "cicd-dependency"},
		{SourceID: "job:test", TargetID: "external:checkout", Label: "uses", Technology: "cicd-dependency"},
		{SourceID: "job:test", TargetID: "external:setup-node", Label: "uses", Technology: "cicd-dependency"},
		{SourceID: "job:test", TargetID: "action:notify", Label: "uses", Technology: "cicd-dependency"},
		{SourceID: "workflow:ci", TargetID: "reusable:release", Label: "calls", Technology: "cicd-dependency"},
		{SourceID: "job:deploy", TargetID: "external:alpine", Label: "uses", Technology: "cicd-dependency"},
	}
	sortConnectors(got.Connectors)
	sortConnectors(wantConnectors)
	if !reflect.DeepEqual(got.Connectors, wantConnectors) {
		t.Fatalf("connectors mismatch\n got: %#v\nwant: %#v", got.Connectors, wantConnectors)
	}
	if len(got.Warnings) != 0 {
		t.Fatalf("unexpected warnings: %#v", got.Warnings)
	}
}

func TestParseGitHubWorkflowNeedsListAndExpressions(t *testing.T) {
	input := `
name: Multi
on: push
jobs:
  lint:
    steps:
      - run: echo hi
  build:
    needs: [lint]
    steps:
      - uses: actions/checkout@v4
  release:
    needs:
      - lint
      - build
    steps:
      - uses: ${{ fromJSON(...) }}
`

	got, err := ParseGitHubWorkflow(input)
	if err != nil {
		t.Fatalf("ParseGitHubWorkflow returned error: %v", err)
	}

	wantConnectors := []ParsedConnector{
		{SourceID: "job:lint", TargetID: "job:build", Label: "needs", Technology: "cicd-dependency"},
		{SourceID: "job:lint", TargetID: "job:release", Label: "needs", Technology: "cicd-dependency"},
		{SourceID: "job:build", TargetID: "job:release", Label: "needs", Technology: "cicd-dependency"},
		{SourceID: "job:build", TargetID: "external:checkout", Label: "uses", Technology: "cicd-dependency"},
	}
	sortConnectors(got.Connectors)
	sortConnectors(wantConnectors)
	if !reflect.DeepEqual(got.Connectors, wantConnectors) {
		t.Fatalf("connectors mismatch\n got: %#v\nwant: %#v", got.Connectors, wantConnectors)
	}
	// Expression-valued uses must not produce elements or connectors.
	if len(got.Elements) != 5 { // workflow + lint + build + release + checkout
		t.Fatalf("unexpected elements: %#v", got.Elements)
	}
}

func TestDetectFormatGithubWorkflow(t *testing.T) {
	workflow := "name: CI\non: push\njobs:\n  build:\n    steps:\n      - run: echo\n"
	if got := DetectFormat(workflow); got != "github-workflow" {
		t.Fatalf("DetectFormat(workflow) = %q, want github-workflow", got)
	}

	dsl := `workspace "x" {
  model {
    a = component "A"
  }
}`
	if got := DetectFormat(dsl); got != "structurizr" {
		t.Fatalf("DetectFormat(dsl) = %q, want structurizr", got)
	}

	mermaid := "architecture-beta\ngroup api[API]"
	if got := DetectFormat(mermaid); got != "mermaid" {
		t.Fatalf("DetectFormat(mermaid) = %q, want mermaid", got)
	}

	// A nested jobs: key on its own must not trigger workflow detection.
	nested := "model:\n  jobs:\n    - run\n"
	if got := DetectFormat(nested); got != "structurizr" {
		t.Fatalf("DetectFormat(nested) = %q, want structurizr", got)
	}
}

func TestParseRoutesToGithubWorkflow(t *testing.T) {
	input := "name: CI\non: push\njobs:\n  build:\n    steps:\n      - uses: actions/checkout@v4\n"
	got, err := Parse(input)
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	if len(got.Elements) != 3 { // workflow + job + checkout
		t.Fatalf("expected 3 elements, got %#v", got.Elements)
	}
	if got.Connectors[0].Label != "uses" {
		t.Fatalf("expected uses connector, got %#v", got.Connectors)
	}
}
