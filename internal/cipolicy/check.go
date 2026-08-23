// Package cipolicy validates the secretless workflows and their checked-in
// branch-protection contract without relying on grep-shaped YAML parsing.
package cipolicy

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

type workflow struct {
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Concurrency workflowConcurrency    `yaml:"concurrency"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowConcurrency struct {
	Group            string `yaml:"group"`
	CancelInProgress bool   `yaml:"cancel-in-progress"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
	If          string            `yaml:"if"`
	Environment string            `yaml:"environment"`
	Needs       any               `yaml:"needs"`
	Permissions map[string]string `yaml:"permissions"`
	Steps       []workflowStep    `yaml:"steps"`
}

type workflowStep struct {
	Name string            `yaml:"name"`
	If   string            `yaml:"if"`
	Env  map[string]string `yaml:"env"`
	With map[string]any    `yaml:"with"`
	Uses string            `yaml:"uses"`
	Run  string            `yaml:"run"`
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
	if err := checkDependabotMerge(root); err != nil {
		return err
	}
	if err := checkPinnedWorkflowActions(root); err != nil {
		return err
	}
	if err := checkScorecard(root); err != nil {
		return err
	}
	if err := checkTrustedImagePublisher(root); err != nil {
		return err
	}
	if err := checkProtectedCanary(root); err != nil {
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

// checkTrustedImagePublisher confines package-write authority to a protected
// workflow_run lane. It re-derives the source run and main SHA before checkout,
// and publishes only full-SHA image tags after final-image tests.
func checkTrustedImagePublisher(root string) error {
	relative := ".github/workflows/publish-images.yml"
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	text := string(data)
	if strings.Contains(text, "secrets.") || strings.Contains(text, "pull_request_target") {
		return fmt.Errorf("%s must not access repository secrets or use pull_request_target", relative)
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("decode %s: %w", relative, err)
	}
	if len(wf.On) != 1 || wf.On["workflow_run"] == nil {
		return fmt.Errorf("%s must trigger only from workflow_run", relative)
	}
	wantDefaults := map[string]string{"actions": "read", "contents": "read"}
	if len(wf.Permissions) != len(wantDefaults) {
		return fmt.Errorf("%s must default to exactly read-only permissions", relative)
	}
	for name, access := range wantDefaults {
		if wf.Permissions[name] != access {
			return fmt.Errorf("%s default permission %s must be %s", relative, name, access)
		}
	}
	publish, ok := wf.Jobs["publish"]
	if len(wf.Jobs) != 1 || !ok || publish.If != "github.event.workflow_run.conclusion == 'success'" {
		return fmt.Errorf("%s must contain only the success-gated publish job", relative)
	}
	wantJob := map[string]string{"actions": "read", "contents": "read", "packages": "write"}
	if len(publish.Permissions) != len(wantJob) {
		return fmt.Errorf("%s publish job must hold exactly the documented package-write permissions", relative)
	}
	for name, access := range wantJob {
		if publish.Permissions[name] != access {
			return fmt.Errorf("%s publish permission %s must be %s", relative, name, access)
		}
	}
	var lane strings.Builder
	for _, step := range publish.Steps {
		lane.WriteString(step.Uses)
		lane.WriteByte('\n')
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
		if len(step.With) != 0 {
			with, err := yaml.Marshal(step.With)
			if err != nil {
				return fmt.Errorf("encode %s step inputs: %w", relative, err)
			}
			lane.Write(with)
		}
	}
	laneText := lane.String()
	for _, required := range []string{
		".head_repository.full_name",
		".head_branch",
		".head_sha",
		"git/ref/heads/main",
		`[[ "${event}" == push || "${event}" == workflow_dispatch ]]`,
		`[[ "${head_branch}" == main ]]`,
		`[[ "${head_sha}" == "${main_sha}" ]]`,
		"actions/checkout@",
		"actions/download-artifact@",
		"run-id:",
		"github-token:",
		"thornhill-images-",
		"docker load",
		"docker tag",
		"docker login ghcr.io",
		"docker push",
		"org.opencontainers.image.revision",
		"scripts/test-container-hardening.sh",
		"@sha256:",
		"actions/upload-artifact@",
	} {
		if !strings.Contains(laneText, required) {
			return fmt.Errorf("%s must include %q", relative, required)
		}
	}
	for _, forbidden := range []string{"docker buildx build", "docker build ", "docker compose build"} {
		if strings.Contains(laneText, forbidden) {
			return fmt.Errorf("%s must promote downloaded qualified images without rebuilding: %q", relative, forbidden)
		}
	}
	return nil
}

func checkProtectedCanary(root string) error {
	relative := ".github/workflows/canary.yml"
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("decode %s: %w", relative, err)
	}
	if len(wf.On) != 1 || wf.On["workflow_dispatch"] == nil {
		return fmt.Errorf("%s must be opt-in workflow_dispatch only", relative)
	}
	wantPermissions := map[string]string{"actions": "read", "contents": "read"}
	if len(wf.Permissions) != len(wantPermissions) {
		return fmt.Errorf("%s must default to read-only permissions", relative)
	}
	for name, access := range wantPermissions {
		if wf.Permissions[name] != access {
			return fmt.Errorf("%s permission %s must be %s", relative, name, access)
		}
	}
	job, ok := wf.Jobs["canary"]
	if len(wf.Jobs) != 1 || !ok || job.If != "github.ref == 'refs/heads/main'" || job.Environment != "production-canary" {
		return fmt.Errorf("%s must have one protected-main environment canary job", relative)
	}
	if len(job.Permissions) != len(wantPermissions) {
		return fmt.Errorf("%s canary job must retain read-only permissions", relative)
	}
	for name, access := range wantPermissions {
		if job.Permissions[name] != access {
			return fmt.Errorf("%s canary job permission %s must be %s", relative, name, access)
		}
	}
	var lane strings.Builder
	for _, step := range job.Steps {
		lane.WriteString(step.Uses)
		lane.WriteByte('\n')
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
		for name, value := range step.Env {
			lane.WriteString(name)
			lane.WriteByte('=')
			lane.WriteString(value)
			lane.WriteByte('\n')
		}
	}
	laneText := lane.String()
	if strings.Contains(laneText, "inputs.base_url") || strings.Contains(laneText, "inputs.provider_url") {
		return fmt.Errorf("%s must not route a protected provider token to dispatch-controlled URLs", relative)
	}
	for _, required := range []string{
		"git/ref/heads/main",
		"actions/checkout@",
		"scripts/run-canary.sh",
		"vars.THORNHILL_CANARY_BASE_URL",
		"vars.THORNHILL_CANARY_PROVIDER_URL",
		"THORNHILL_CANARY_PROVIDER_TOKEN",
		"secrets.THORNHILL_CANARY_PROVIDER_TOKEN",
	} {
		if !strings.Contains(laneText, required) {
			return fmt.Errorf("%s must include %q", relative, required)
		}
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
	if subtle.ConstantTimeCompare([]byte(policy.SchemaSHA256), []byte(digest)) != 1 {
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
	if err := checkCIDispatchContract(wf, preflight); err != nil {
		return err
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
		"go test -race -covermode=atomic",
		"scripts/check-coverage.py",
		"scripts/high-risk-review.py",
		"github.event.before",
		"scripts/test-deployer-policy.sh",
		"scripts/test-deployer-transition-recovery.sh",
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
		"scripts/test-local-recovery.sh",
		"scripts/run-security-scans.sh",
		"docker save thornhill:ci",
		"docker save thornhill-postgres:ci",
		"thornhill-images-${{ github.sha }}",
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

// checkCIDispatchContract binds every explicit CI dispatch to one exact
// protected-main revision. The dispatch API accepts only a branch or tag ref,
// so the caller supplies the landed SHA as a required input and preflight checks
// GitHub's resolved SHA before any checkout or contributor-controlled command.
func checkCIDispatchContract(wf workflow, preflight workflowJob) error {
	dispatch, ok := wf.On["workflow_dispatch"].(map[string]any)
	if !ok {
		return fmt.Errorf("CI workflow_dispatch must define the exact-SHA input contract")
	}
	inputs, ok := dispatch["inputs"].(map[string]any)
	if !ok || len(inputs) != 1 {
		return fmt.Errorf("CI workflow_dispatch must define only expected_sha")
	}
	expected, ok := inputs["expected_sha"].(map[string]any)
	if !ok || expected["required"] != true || expected["type"] != "string" {
		return fmt.Errorf("CI workflow_dispatch expected_sha must be a required string")
	}
	const dispatchGroup = "${{ github.workflow }}-${{ github.ref }}-${{ github.event_name }}-${{ inputs.expected_sha || '' }}"
	if wf.Concurrency.Group != dispatchGroup || !wf.Concurrency.CancelInProgress {
		return fmt.Errorf("CI concurrency must isolate pushes from exact-SHA dispatches while cancelling duplicates")
	}

	if len(preflight.Steps) == 0 || preflight.Steps[0].Name != "Verify dispatched protected-main revision" {
		return fmt.Errorf("CI preflight must verify the dispatched protected-main revision as its first step")
	}
	step := preflight.Steps[0]
	if step.If != "github.event_name == 'workflow_dispatch'" ||
		len(step.Env) != 1 || step.Env["EXPECTED_SHA"] != "${{ inputs.expected_sha }}" {
		return fmt.Errorf("CI dispatch verification step must be isolated to workflow_dispatch and bind expected_sha")
	}
	for _, required := range []string{
		`[[ "${GITHUB_REF}" == 'refs/heads/main' ]]`,
		`[[ "${EXPECTED_SHA}" =~ ^[0-9a-f]{40}$ ]]`,
		`[[ "${GITHUB_SHA}" == "${EXPECTED_SHA}" ]]`,
	} {
		if !strings.Contains(step.Run, required) {
			return fmt.Errorf("CI dispatch verification step must include %q", required)
		}
	}
	return nil
}

func requireLaneSteps(name string, job workflowJob, required []string) error {
	lane, err := workflowLaneText(job)
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	for _, want := range required {
		if !strings.Contains(lane, want) {
			return fmt.Errorf("%s must include %q", name, want)
		}
	}
	return nil
}

func workflowLaneText(job workflowJob) (string, error) {
	var lane strings.Builder
	for _, step := range job.Steps {
		lane.WriteString(step.Name)
		lane.WriteByte('\n')
		lane.WriteString(step.If)
		lane.WriteByte('\n')
		for name, value := range step.Env {
			lane.WriteString(name)
			lane.WriteByte('=')
			lane.WriteString(value)
			lane.WriteByte('\n')
		}
		if len(step.With) != 0 {
			with, err := yaml.Marshal(step.With)
			if err != nil {
				return "", err
			}
			lane.Write(with)
		}
		lane.WriteString(step.Uses)
		lane.WriteByte('\n')
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
	}
	return lane.String(), nil
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

// checkDependabotMerge pins the one lane that can write to the protected branch.
// The grant must stay confined to a single job whose workflow default is
// read-only, the lane must run no actions at all so nothing third-party executes
// with that token, and it must re-derive its own guards from the workflow_run
// metadata rather than trusting the review lane. The merge must name the
// CI-tested SHA so a rebase landing between qualification and this request is
// refused instead of merged. Since GITHUB_TOKEN-created commits suppress push
// workflows, the lane must also dispatch full CI on protected main after a
// successful landing so the deployed revision earns post-merge proof.
func checkDependabotMerge(root string) error {
	relative := ".github/workflows/dependabot-auto-merge.yml"
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	text := string(data)
	if strings.Contains(text, "secrets.") || strings.Contains(text, "actions/checkout@") ||
		strings.Contains(text, "pull_request_target") {
		return fmt.Errorf("%s must not access secrets, check out code, or use pull_request_target", relative)
	}

	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("decode %s: %w", relative, err)
	}
	if len(wf.On) != 1 || wf.On["workflow_run"] == nil {
		return fmt.Errorf("%s must trigger only from workflow_run", relative)
	}
	wantDefaults := map[string]string{"actions": "read", "contents": "read"}
	if len(wf.Permissions) != len(wantDefaults) {
		return fmt.Errorf("%s must default to exactly the documented read-only permissions", relative)
	}
	for name, access := range wantDefaults {
		if wf.Permissions[name] != access {
			return fmt.Errorf("%s default permission %s must be %s", relative, name, access)
		}
	}

	merge, ok := wf.Jobs["merge"]
	if len(wf.Jobs) != 1 || !ok || merge.If != "github.event.workflow_run.conclusion == 'success'" {
		return fmt.Errorf("%s must contain only the success-gated merge job", relative)
	}
	wantJob := map[string]string{"contents": "write", "pull-requests": "write", "actions": "write"}
	if len(merge.Permissions) != len(wantJob) {
		return fmt.Errorf("%s merge job must hold exactly the documented narrow permissions", relative)
	}
	for name, access := range wantJob {
		if merge.Permissions[name] != access {
			return fmt.Errorf("%s merge job permission %s must be %s", relative, name, access)
		}
	}

	var lane strings.Builder
	for _, step := range merge.Steps {
		if step.Uses != "" {
			return fmt.Errorf("%s merge job must not run external actions", relative)
		}
		lane.WriteString(step.Run)
		lane.WriteByte('\n')
	}
	laneText := lane.String()
	mergeEndpoint := `repos/${REPOSITORY}/pulls/${pull_request}/merge`
	dispatchEndpoint := `repos/${REPOSITORY}/actions/workflows/ci.yml/dispatches`
	if strings.Count(laneText, mergeEndpoint) != 1 || strings.Count(laneText, dispatchEndpoint) != 1 {
		return fmt.Errorf("%s must contain exactly one SHA-bound merge and one exact-main CI dispatch", relative)
	}
	if strings.Count(laneText, "gh api --method PUT") != 1 || strings.Count(laneText, "gh api --method POST") != 3 {
		return fmt.Errorf("%s must contain only the one merge mutation and three documented Actions mutations", relative)
	}
	for _, forbidden := range []string{
		"gh api -X", "gh api --method=", "gh pr merge", "gh workflow run", "curl ", "wget ",
	} {
		if strings.Contains(laneText, forbidden) {
			return fmt.Errorf("%s must not use an alternate network mutation form %q", relative, forbidden)
		}
	}
	for _, line := range strings.Split(laneText, "\n") {
		if strings.Contains(line, "/pulls/") && strings.Contains(line, "/merge") &&
			!strings.Contains(line, mergeEndpoint) {
			return fmt.Errorf("%s contains an unrecognized pull-request merge endpoint", relative)
		}
		if strings.Contains(line, "/actions/workflows/") && strings.Contains(line, "/dispatches") &&
			!strings.Contains(line, dispatchEndpoint) {
			return fmt.Errorf("%s contains an unrecognized workflow dispatch endpoint", relative)
		}
	}
	boundMerge := regexp.MustCompile(`(?m)if merge_response=\$\(gh api --method PUT "repos/\$\{REPOSITORY\}/pulls/\$\{pull_request\}/merge" \\\n\s+-f "sha=\$\{head_sha\}" \\\n\s+-f merge_method=squash\) &&`)
	if !boundMerge.MatchString(laneText) {
		return fmt.Errorf("%s merge call must bind the qualified SHA and squash method in one command", relative)
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
		`.base.repo.full_name == $repository`,
		`.base.ref == "main"`,
		`.head.ref == $branch`,
		`.head.sha == $sha`,
		`.pull_requests[0].number`,
		`-f state=all`,
		`.merge_commit_sha`,
		`gh run list --repo`,
		`.status == "queued"`,
		`.status == "in_progress"`,
		`.conclusion == "success"`,
		`repos/${REPOSITORY}/pulls/${pull_request}/merge`,
		// Bind the merge to the qualified revision, and keep linear history.
		`-f "sha=${head_sha}"`,
		"-f merge_method=squash",
		`.merged == true`,
		`repos/${REPOSITORY}/git/ref/heads/main`,
		`"${main_sha}" == "${landed_sha}"`,
		`'{ref:"main", inputs:{expected_sha:$expected_sha}}'`,
		`X-GitHub-Api-Version: 2026-03-10`,
		`repos/${REPOSITORY}/actions/workflows/ci.yml/dispatches`,
		`.workflow_run_id`,
		`repos/${REPOSITORY}/actions/runs/${dispatch_run_id}`,
		`"${dispatch_event}" != 'workflow_dispatch'`,
		`"${dispatch_branch}" != 'main'`,
		`"${dispatch_sha}" != "${landed_sha}"`,
		`repos/${REPOSITORY}/actions/runs/${dispatch_run_id}/cancel`,
	} {
		if !strings.Contains(laneText, required) {
			return fmt.Errorf("%s merge lane must include %q", relative, required)
		}
	}
	return nil
}

// checkScorecard pins the measurement lane. It is the only workflow holding
// `security-events: write`, so that grant must stay confined to one job while the
// workflow default stays read-only, and it must not quietly grow an `id-token` for
// result publication. Scorecard is advisory by design and must never become the
// required check, which the single-required-context assertion above enforces.
//
// SHA pinning is not re-checked here; checkPinnedWorkflowActions covers every
// workflow, including this one.
func checkScorecard(root string) error {
	relative := ".github/workflows/scorecard.yml"
	data, err := os.ReadFile(filepath.Join(root, relative))
	if err != nil {
		return err
	}
	text := string(data)
	if strings.Contains(text, "secrets.") || strings.Contains(text, "pull_request_target") {
		return fmt.Errorf("%s must not access secrets or use pull_request_target", relative)
	}
	// Publication is what would require an OIDC token; the permission assertions
	// below are what actually deny one, at both the workflow and job level.
	if !strings.Contains(text, "publish_results: false") {
		return fmt.Errorf("%s must explicitly disable result publication", relative)
	}

	var wf workflow
	if err := yaml.Unmarshal(data, &wf); err != nil {
		return fmt.Errorf("decode %s: %w", relative, err)
	}
	gotTriggers := make([]string, 0, len(wf.On))
	for trigger := range wf.On {
		gotTriggers = append(gotTriggers, trigger)
	}
	sort.Strings(gotTriggers)
	// Never pull_request: Scorecard evaluates the default branch, and a
	// contributor-triggered run must not reach a lane holding a write grant.
	if want := []string{"push", "schedule", "workflow_dispatch"}; strings.Join(gotTriggers, ",") != strings.Join(want, ",") {
		return fmt.Errorf("%s triggers = %v, want %v", relative, gotTriggers, want)
	}
	if len(wf.Permissions) != 1 || wf.Permissions["contents"] != "read" {
		return fmt.Errorf("%s must default to exactly contents: read", relative)
	}

	analyze, ok := wf.Jobs["analyze"]
	if len(wf.Jobs) != 1 || !ok {
		return fmt.Errorf("%s must contain only the analysis job", relative)
	}
	wantJobPermissions := map[string]string{"security-events": "write", "actions": "read", "contents": "read"}
	if len(analyze.Permissions) != len(wantJobPermissions) {
		return fmt.Errorf("%s analysis job must hold exactly the documented narrow permissions", relative)
	}
	for name, access := range wantJobPermissions {
		if analyze.Permissions[name] != access {
			return fmt.Errorf("%s analysis job permission %s must be %s", relative, name, access)
		}
	}
	return nil
}

// checkPinnedWorkflowActions requires every `uses:` reference in every workflow to
// name a full commit SHA. GitHub's repository-level action allowlist enforces the
// same rule remotely and rejects the workflow at startup otherwise, which is a
// slow way to learn about a mutable reference: the run reports
// `startup_failure` with no jobs and no logs. Asserting it here fails the required
// check on the pull request instead, next to the reason.
//
// The allowlist also restricts *which* actions may run — currently GitHub-authored
// ones plus an explicitly vetted `gitleaks/gitleaks-action@*`. That list lives in
// repository settings rather than in this repository, so it cannot be asserted
// here; adding a workflow that uses anything else requires widening it first.
func checkPinnedWorkflowActions(root string) error {
	pattern := filepath.Join(root, ".github/workflows/*.yml")
	paths, err := filepath.Glob(pattern)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return fmt.Errorf("no workflows found at %s", pattern)
	}
	sort.Strings(paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var wf workflow
		if err := yaml.Unmarshal(data, &wf); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			relative = path
		}
		for id, job := range wf.Jobs {
			for _, step := range job.Steps {
				if step.Uses == "" {
					continue
				}
				if _, pinned := pinnedActionSHA(step.Uses); !pinned {
					return fmt.Errorf("%s job %s uses unpinned action %q", relative, id, step.Uses)
				}
			}
		}
	}
	return nil
}

// pinnedActionSHA reports whether a `uses:` reference names a full 40-character
// commit SHA. A tag or branch reference is mutable and therefore not a pin.
func pinnedActionSHA(uses string) (string, bool) {
	_, ref, found := strings.Cut(uses, "@")
	if !found || len(ref) != 40 {
		return "", false
	}
	for _, r := range ref {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return "", false
		}
	}
	return ref, true
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
