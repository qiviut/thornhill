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
		}
	}
	return nil
}
