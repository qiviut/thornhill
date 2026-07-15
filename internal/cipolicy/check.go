// Package cipolicy validates the secretless workflows and their checked-in
// branch-protection contract without relying on grep-shaped YAML parsing.
package cipolicy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type workflow struct {
	On          map[string]any         `yaml:"on"`
	Permissions map[string]string      `yaml:"permissions"`
	Jobs        map[string]workflowJob `yaml:"jobs"`
}

type workflowJob struct {
	Name        string            `yaml:"name"`
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
			verify, ok := wf.Jobs["verify"]
			if !ok || verify.Name != requiredCheck {
				return fmt.Errorf("CI verify job must retain required check name %q", requiredCheck)
			}
			var lane strings.Builder
			for _, step := range verify.Steps {
				lane.WriteString(step.Uses)
				lane.WriteByte('\n')
				lane.WriteString(step.Run)
				lane.WriteByte('\n')
			}
			for _, required := range []string{
				"go tool actionlint",
				"go tool staticcheck",
				"go tool govulncheck",
				"npm run lint",
				"docker buildx build --check",
				"scripts/test-container-hardening.sh",
				"scripts/run-security-scans.sh",
				"actions/upload-artifact@",
			} {
				if !strings.Contains(lane.String(), required) {
					return fmt.Errorf("CI qualification lane must include %q", required)
				}
			}
		}
	}
	if err := checkDependabot(root); err != nil {
		return err
	}
	if err := checkPinnedImages(root); err != nil {
		return err
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
