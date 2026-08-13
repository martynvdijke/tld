package watch

import (
	"testing"
)

func TestInferArchitectureDiscoversGitHubWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/workflows/ci.yml", `
name: CI
on:
  push:
    branches: [main]
  pull_request:
    types: [opened]
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
`)
	writeFile(t, dir, ".github/actions/notify/action.yml", `
name: send notification
description: Notify the team
runs:
  using: composite
  steps:
    - run: echo notify
`)

	model := inferArchitectureWithProgress(dir, &recordingProgress{})

	for _, name := range []string{"CI", "test", "deploy"} {
		if model.Components[architectureKey("component", name)] == nil {
			t.Fatalf("expected workflow/job component %q, got %#v", name, model.Components)
		}
	}
	if got := model.Components[architectureKey("component", "CI")]; got == nil || !containsString(got.Tags, "trigger:push") || !containsString(got.Tags, "trigger:pull-request") {
		t.Fatalf("expected trigger tags on workflow, got %#v", got)
	}
	// Local composite action surfaced from its manifest (directory identity).
	if model.Components[architectureKey("component", "notify")] == nil {
		t.Fatalf("expected local action component, got %#v", model.Components)
	}
	// Third-party actions and the reusable workflow target.
	if model.Components[architectureKey("external", "checkout")] == nil {
		t.Fatalf("expected third-party action component, got %#v", model.Components)
	}
	if model.Components[architectureKey("external", "alpine")] == nil {
		t.Fatalf("expected container action component, got %#v", model.Components)
	}
	if model.Components[architectureKey("component", "release")] == nil {
		t.Fatalf("expected reusable workflow component, got %#v", model.Components)
	}

	connectorKey := func(source, target, label string) string {
		return source + "->" + target + ":cicd-dependency:" + label
	}
	jobDeploy := architectureKey("component", "deploy")
	jobTest := architectureKey("component", "test")
	workflowCI := architectureKey("component", "CI")
	actionCheckout := architectureKey("external", "checkout")
	reusableRelease := architectureKey("component", "release")
	for _, key := range []string{
		connectorKey(jobTest, jobDeploy, "needs"),
		connectorKey(jobTest, actionCheckout, "uses"),
		connectorKey(workflowCI, reusableRelease, "calls"),
	} {
		if model.Connectors[key] == nil {
			t.Fatalf("expected connector %q, got %#v", key, model.Connectors)
		}
	}
}

func TestInferArchitectureSkipsMalformedGitHubWorkflow(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, ".github/workflows/broken.yml", "name: broken\non:\n  push: [\n")
	writeFile(t, dir, ".github/workflows/ok.yml", `
name: OK
on: push
jobs:
  build:
    steps:
      - uses: actions/checkout@v4
`)

	model := inferArchitectureWithProgress(dir, &recordingProgress{})

	if model.Components[architectureKey("component", "broken")] != nil {
		t.Fatalf("malformed workflow should be skipped, got %#v", model.Components)
	}
	if model.Components[architectureKey("component", "OK")] == nil {
		t.Fatalf("valid workflow should be discovered, got %#v", model.Components)
	}
	if model.Components[architectureKey("component", "build")] == nil {
		t.Fatalf("expected job component, got %#v", model.Components)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
