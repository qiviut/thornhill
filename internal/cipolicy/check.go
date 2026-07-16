// Package cipolicy validates the secretless workflows and their checked-in
// branch-protection contract without relying on grep-shaped YAML parsing.
package cipolicy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type workflow struct {
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Uses string `yaml:"uses"`
	Run  string `yaml:"run"`
}

type dependabotConfig struct {
	Updates []dependabotUpdate `yaml:"updates"`
}

type dependabotUpdate struct {
	PackageEcosystem string         `yaml:"package-ecosystem"`
	Directory        string         `yaml:"directory"`
	Groups           map[string]any `yaml:"groups"`
}

type scannerCompose struct {
	Services map[string]struct {
		Image string `yaml:"image"`
	} `yaml:"services"`
}

type rollbackCompatibility struct {
	SchemaSHA256 string `json:"schema_sha256"`
	Mode         string `json:"mode"`
	Rationale    string `json:"rationale"`
}

type protection struct {
	RequiredStatusChecks struct {
		Strict   bool     `json:"strict"`
		Contexts []string `json:"contexts"`
	} `json:"required_status_checks"`
	EnforceAdmins              bool `json:"enforce_admins"`
	RequiredPullRequestReviews *struct {
		DismissStaleReviews          bool `json:"dismiss_stale_reviews"`
		RequireCodeOwnerReviews      bool `json:"require_code_owner_reviews"`
		RequiredApprovingReviewCount int  `json:"required_approving_review_count"`
	} `json:"required_pull_request_reviews"`
	RequiredLinearHistory          bool `json:"required_linear_history"`
	AllowForcePushes               bool `json:"allow_force_pushes"`
	AllowDeletions                 bool `json:"allow_deletions"`
	RequiredConversationResolution bool `json:"required_conversation_resolution"`
}

var secretlessWorkflows = map[string][]string{
	".github/workflows/ci.yml":   {"pull_request", "push", "workflow_dispatch"},
	".github/workflows/fuzz.yml": {"schedule", "workflow_dispatch"},
}

func Check(root string) error {
	policyPath := filepath.Join(root, ".github/branch-protection.json")
	policyData, err := os.ReadFile(policyPath)
	if err != nil {
		return err
	}
	var policy protection
	if err := json.Unmarshal(policyData, &policy); err != nil {
		return fmt.Errorf("decode branch protection: %w", err)
	}
	const requiredCheck = "Go, web, and image build"
	if len(policy.RequiredStatusChecks.Contexts) != 1 || policy.RequiredStatusChecks.Contexts[0] != requiredCheck {
		return fmt.Errorf("branch protection must require exactly %q", requiredCheck)
	}
	if !policy.RequiredStatusChecks.Strict || !policy.EnforceAdmins || !policy.RequiredLinearHistory ||
		policy.AllowForcePushes || policy.AllowDeletions || !policy.RequiredConversationResolution {
		return fmt.Errorf("branch protection safety invariants are incomplete")
	}
	if policy.RequiredPullRequestReviews == nil || !policy.RequiredPullRequestReviews.DismissStaleReviews ||
		policy.RequiredPullRequestReviews.RequireCodeOwnerReviews || policy.RequiredPullRequestReviews.RequiredApprovingReviewCount != 0 {
		return fmt.Errorf("branch protection must require pull requests with stale-review dismissal and the documented solo-maintainer review policy")
	}

	for relative, wantTriggers := range secretlessWorkflows {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			return err
		}
		if strings.Contains(string(data), "secrets.") {
			return fmt.Errorf("%s references secrets", relative)
		}
		var wf workflow
		if err := yaml.Unmarshal(data, &wf); err != nil {
			return fmt.Errorf("decode %s: %w", relative, err)
		}
		wantPermissions := map[string]string{"contents": "read"}
		if relative == ".github/workflows/ci.yml" {
			wantPermissions["pull-requests"] = "read"
		}
		permissionsMatch := len(wf.Permissions) == len(wantPermissions)
		for name, access := range wantPermissions {
			permissionsMatch = permissionsMatch && wf.Permissions[name] == access
		}
		if !permissionsMatch {
			return fmt.Errorf("%s must have exactly the documented read-only permissions", relative)
		}
		for id, job := range wf.Jobs {
			if len(job.Permissions) != 0 {
				return fmt.Errorf("%s job %s overrides permissions", relative, id)
			}
		}
		gotTriggers := make([]string, 0, len(wf.On))
		for trigger := range wf.On {
			gotTriggers = append(gotTriggers, trigger)
		}
		sort.Strings(gotTriggers)
		sort.Strings(wantTriggers)
		if strings.Join(gotTriggers, ",") != strings.Join(wantTriggers, ",") {
			return fmt.Errorf("%s triggers = %v, want %v", relative, gotTriggers, wantTriggers)
		}
		if relative == ".github/workflows/ci.yml" {
			if err := checkQualificationLanes(wf, requiredCheck); err != nil {
				return err
			}
		}
	}
	if err := checkDependabot(root); err != nil {
		return err
	}
	if err := checkDependabotApproval(root); err != nil {
		return err
	}
	if err := checkPinnedImages(root); err != nil {
		return err
	}
	if err := checkRollbackCompatibility(root); err != nil {
		return err
	}
	return nil
}

func checkRollbackCompatibility(root string) error {
	storeData, err := os.ReadFile(filepath.Join(root, "internal/store/store.go"))
	if err != nil {
		return err
	}
	const marker = "const schema = `"
	start := strings.Index(string(storeData), marker)
	if start < 0 {
		return fmt.Errorf("store schema marker is missing")
	}
	start += len(marker)
	end := strings.Index(string(storeData[start:]), "`")
	if end < 0 {
		return fmt.Errorf("store schema terminator is missing")
	}
	digest := fmt.Sprintf("%x", sha256.Sum256(storeData[start:start+end]))

	policyData, err := os.ReadFile(filepath.Join(root, "docs/rollback-compatibility.json"))
	if err != nil {
		return err
	}
	var policy rollbackCompatibility
	if err := json.Unmarshal(policyData, &policy); err != nil {
		return fmt.Errorf("decode rollback compatibility policy: %w", err)
	}
	if policy.SchemaSHA256 != digest {
		return fmt.Errorf("rollback compatibility policy does not cover current schema: got %q want %q", policy.SchemaSHA256, digest)
	}
	if policy.Mode != "backward-compatible-additive" && policy.Mode != "manual-forward-only" {
		return fmt.Errorf("rollback compatibility mode %q is not recognized", policy.Mode)
	}
	if len(strings.TrimSpace(policy.Rationale)) < 80 {
		return fmt.Errorf("rollback compatibility rationale must explain the migration and rollback consequence")
	}
	deployer, err := os.ReadFile(filepath.Join(root, "scripts/deploy-passed-main.sh"))
	if err != nil {
		return err
	}
	if !strings.Contains(string(deployer), "docs/rollback-compatibility.json") ||
		!strings.Contains(string(deployer), "backward-compatible-additive") {
		return fmt.Errorf("deployer must gate automatic promotion on rollback compatibility mode")
	}
	return nil
}

// checkQualificationLanes preserves the protection contract while allowing
// source analysis/fuzzing and image/security work to run concurrently after a
// history-and-policy gate. The protected check is a fail-closed aggregator:
// it does not evaluate checkout content itself.
func checkQualificationLanes(wf workflow, requiredCheck string) error {
	preflight, ok := wf.Jobs["preflight"]
	if !ok {
		return fmt.Errorf("CI must include history and policy preflight")
	}
	if err := requireLaneSteps("CI preflight", preflight, []string{
		"gitleaks/gitleaks-action@",
		"scripts/check-ci-policy.sh",
	}); err != nil {
		return err
	}

	source, ok := wf.Jobs["source"]
	if !ok || !needs(source, "preflight") {
		return fmt.Errorf("CI source qualification lane must depend on preflight")
	}
	if err := requireLaneSteps("CI source qualification lane", source, []string{
		"go tool actionlint",
		"go tool staticcheck",
		"go tool govulncheck",
		"go test -race ./...",
		"scripts/test-deployer-policy.sh",
		"scripts/test-fuzz.sh",
		"TestProviderProcessConformance",
		"npm run lint",
		"npm test",
		"npm audit --audit-level=high",
	}); err != nil {
		return err
	}

	image, ok := wf.Jobs["image"]
	if !ok || !needs(image, "preflight") {
		return fmt.Errorf("CI image qualification lane must depend on preflight")
	}
	if err := requireLaneSteps("CI image qualification lane", image, []string{
		"docker buildx build --check",
		"scripts/test-container-hardening.sh",
		"scripts/test-postgres-integration.sh",
		"scripts/run-security-scans.sh",
		"actions/upload-artifact@",
	}); err != nil {
		return err
	}

	verify, ok := wf.Jobs["verify"]
	if !ok || verify.Name != requiredCheck {
		return fmt.Errorf("CI verify job must retain required check name %q", requiredCheck)
	}
	if !strings.Contains(verify.If, "always()") || !needs(verify, "source") || !needs(verify, "image") {
		return fmt.Errorf("CI verify job must fail closed after source and image qualification lanes")
	}
	if len(verify.Steps) != 1 || !strings.Contains(verify.Steps[0].Run, "needs.source.result") ||
		!strings.Contains(verify.Steps[0].Run, "needs.image.result") {
		return fmt.Errorf("CI verify job must explicitly require source and image success")
	}
	return nil
}

func requireLaneSteps(name string, job workflowJob, required []string) error {
	var lane strings.Builder
	for _, step := range job.Steps {
		lane.WriteString(step.Uses)
		lane.WriteByte('\n')
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
	}
	for _, want := range required {
		if !strings.Contains(lane.String(), want) {
			return fmt.Errorf("%s must include %q", name, want)
		}
	}
	return nil
}

func needs(job workflowJob, wanted string) bool {
	switch raw := job.Needs.(type) {
	case string:
		return raw == wanted
	case []any:
		for _, value := range raw {
			if name, ok := value.(string); ok && name == wanted {
				return true
			}
		}
	}
	return false
}

func checkDependabotApproval(root string) error {
	relative := ".github/workflows/dependabot-auto-approve.yml"
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	text := string(data)
	if strings.Contains(text, "secrets.") || strings.Contains(text, "actions/checkout@") || strings.Contains(text, "pull_request_target") {
		return fmt.Errorf("%s must not access secrets, check out code, or use pull_request_target", relative)
	}

	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("decode %s: %w", relative, err)
	}
	if len(wf.On) != 1 || wf.On["workflow_run"] == nil {
		return fmt.Errorf("%s must trigger only from workflow_run", relative)
	}
	wantPermissions := map[string]string{"actions": "read", "contents": "read", "pull-requests": "write"}
	if len(wf.Permissions) != len(wantPermissions) {
		return fmt.Errorf("%s must have exactly the documented approval permissions", relative)
	}
	for name, access := range wantPermissions {
		if wf.Permissions[name] != access {
			return fmt.Errorf("%s permission %s must be %s", relative, name, access)
		}
	}
	approve, ok := wf.Jobs["approve"]
	if len(wf.Jobs) != 1 || !ok || approve.If != "github.event.workflow_run.conclusion == 'success'" || len(approve.Permissions) != 0 {
		return fmt.Errorf("%s must contain only the success-gated approval job without permission overrides", relative)
	}

	var lane strings.Builder
	for _, step := range approve.Steps {
		if step.Uses != "" {
			return fmt.Errorf("%s approval job must not run external actions", relative)
		}
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
	}
	for _, required := range []string{
		".actor.login",
		".head_repository.full_name",
		".head_branch",
		".head_sha",
		`"${actor}" != 'dependabot[bot]'`,
		`"${source_repository}" != "${REPOSITORY}"`,
		`"${head_branch}" != dependabot/*`,
		`.user.login == "dependabot[bot]"`,
		`.head.repo.full_name == .base.repo.full_name`,
		`.base.ref == "main"`,
		`.head.sha == $sha`,
		`repos/${REPOSITORY}/pulls/${pull_request}/reviews`,
		"-f event=APPROVE",
		`-f "commit_id=${head_sha}"`,
	} {
		if !strings.Contains(lane.String(), required) {
			return fmt.Errorf("%s approval lane must include %q", relative, required)
		}
	}
	return nil
}

func checkDependabot(root string) error {
	data, err := os.ReadFile(filepath.Join(root, ".github/dependabot.yml"))
	if err != nil {
		return err
	}
	var config dependabotConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode Dependabot config: %w", err)
	}
	want := map[string]bool{
		"github-actions|/":                 false,
		"gomod|/":                          false,
		"npm|/web":                         false,
		"docker|/":                         false,
		"docker-compose|/":                 false,
		"docker-compose|/.github/scanners": false,
	}
	for _, update := range config.Updates {
		key := update.PackageEcosystem + "|" + update.Directory
		if _, ok := want[key]; ok {
			want[key] = true
		}
		if key == "docker-compose|/.github/scanners" && len(update.Groups) == 0 {
			return fmt.Errorf("dependabot scanner-image entry must group scanner and embedded-rule updates")
		}
	}
	for key, found := range want {
		if !found {
			return fmt.Errorf("dependabot must cover %s", key)
		}
	}
	return nil
}

func checkPinnedImages(root string) error {
	for _, relative := range []string{"Dockerfile", "Dockerfile.postgres"} {
		data, err := os.ReadFile(filepath.Join(root, relative))
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "FROM ") && !strings.Contains(strings.Fields(line)[1], "@sha256:") {
				return fmt.Errorf("%s contains an unpinned base image: %s", relative, line)
			}
		}
	}
	data, err := os.ReadFile(filepath.Join(root, ".github/scanners/compose.yml"))
	if err != nil {
		return err
	}
	var config scannerCompose
	if err := yaml.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("decode scanner Compose model: %w", err)
	}
	for _, name := range []string{"hadolint", "shellcheck", "trivy"} {
		service, ok := config.Services[name]
		if !ok || !strings.Contains(service.Image, "@sha256:") {
			return fmt.Errorf("scanner %s must use a tag plus immutable manifest digest", name)
		}
	}
	return nil
}
