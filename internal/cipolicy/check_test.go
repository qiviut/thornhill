package cipolicy

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func repositoryRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func TestCheckRepositoryPolicy(t *testing.T) {
	if err := Check(repositoryRoot(t)); err != nil {
		t.Fatal(err)
	}
}

func TestCheckRejectsCIQualificationLaneBypass(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		contain string
	}{
		{
			name:    "source skips preflight",
			old:     "    needs: preflight\n    runs-on: ubuntu-latest\n    timeout-minutes: 20\n",
			new:     "    needs: []\n    runs-on: ubuntu-latest\n    timeout-minutes: 20\n",
			contain: "source qualification lane must depend on preflight",
		},
		{
			name:    "source omits frontend tests",
			old:     "          npm test\n",
			new:     "",
			contain: "source qualification lane must include",
		},
		{
			name:    "image omits security scan",
			old:     "        run: scripts/run-security-scans.sh thornhill:ci thornhill-postgres:ci",
			new:     "        run: true",
			contain: "image qualification lane must include",
		},
		{
			name:    "verify does not aggregate failures",
			old:     "    if: ${{ always() }}\n",
			new:     "    if: ${{ success() }}\n",
			contain: "verify job must fail closed",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/ci.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.new, 1)
			if changed == string(data) {
				t.Fatalf("fixture did not contain %q", tc.old)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

func policyFixture(t *testing.T) string {
	t.Helper()
	source := repositoryRoot(t)
	target := t.TempDir()
	for _, relative := range []string{
		".github/branch-protection.json",
		".github/dependabot.yml",
		".github/scanners/compose.yml",
		".github/workflows/dependabot-auto-approve.yml",
		".github/workflows/dependabot-auto-merge.yml",
		".github/workflows/scorecard.yml",
		".github/workflows/publish-images.yml",
		".github/workflows/canary.yml",
		".github/workflows/ci.yml",
		".github/workflows/fuzz.yml",
		"Dockerfile",
		"Dockerfile.postgres",
		"docs/rollback-compatibility.json",
		"internal/store/store.go",
		"scripts/deploy-passed-main.sh",
	} {
		data, err := os.ReadFile(filepath.Join(source, relative))
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(target, relative)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return target
}

func TestCheckRejectsUnsafeDependabotApproval(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		contain string
	}{
		{
			name:    "missing actor guard",
			old:     `                "${actor}" != 'dependabot[bot]' ||`,
			new:     `                "${actor}" != 'anyone' ||`,
			contain: "approval lane must include",
		},
		{
			name:    "checkout",
			old:     "    steps:\n",
			new:     "    steps:\n      - uses: actions/checkout@0000000000000000000000000000000000000000\n",
			contain: "must not access secrets",
		},
		{
			name:    "review creation is not commit bound",
			old:     "            -f \"commit_id=${head_sha}\" \\\n",
			new:     "",
			contain: "approval lane must include",
		},
		{
			name: "review lane takes write access to the protected branch",
			old:  "  contents: read\n",
			new:  "  contents: write\n",
			// Merging lives in its own lane precisely so the reviewing lane never
			// needs branch write access; escalating it must stay a policy failure.
			contain: "permission contents must be read",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/dependabot-auto-approve.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.new, 1)
			if changed == string(data) {
				t.Fatalf("fixture did not contain %q", tc.old)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

func TestCheckRejectsPrivilegedJobAndUnsafeTrigger(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(string) string
		contain string
	}{
		{
			name: "job permission override",
			mutate: func(in string) string {
				return strings.Replace(in, "    name: Go, web, and image build\n", "    name: Go, web, and image build\n    permissions:\n      contents: write\n", 1)
			},
			contain: "overrides permissions",
		},
		{
			name: "pull request target",
			mutate: func(in string) string {
				return strings.Replace(in, "  pull_request:\n", "  pull_request_target:\n", 1)
			},
			contain: "triggers",
		},
		{
			name: "pull request metadata write",
			mutate: func(in string) string {
				return strings.Replace(in, "  pull-requests: read\n", "  pull-requests: write\n", 1)
			},
			contain: "documented read-only permissions",
		},
		{
			name: "unexpected permission",
			mutate: func(in string) string {
				return strings.Replace(in, "  pull-requests: read\n", "  pull-requests: read\n  issues: read\n", 1)
			},
			contain: "documented read-only permissions",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/ci.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(tc.mutate(string(data))), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

func TestCheckRejectsUnboundCIDispatch(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		contain string
	}{
		{
			name:    "dispatch shares push concurrency",
			old:     `group: ${{ github.workflow }}-${{ github.ref }}-${{ github.event_name }}-${{ inputs.expected_sha || '' }}`,
			new:     `group: ${{ github.workflow }}-${{ github.ref }}`,
			contain: "CI concurrency must isolate pushes from exact-SHA dispatches",
		},
		{
			name:    "dispatch duplication is not cancelled",
			old:     "cancel-in-progress: true",
			new:     "cancel-in-progress: false",
			contain: "CI concurrency must isolate pushes from exact-SHA dispatches",
		},
		{
			name:    "verification is not first",
			old:     "    steps:\n      - name: Verify dispatched protected-main revision\n",
			new:     "    steps:\n      - name: Harmless pre-step\n        run: 'true'\n      - name: Verify dispatched protected-main revision\n",
			contain: "CI preflight must verify the dispatched protected-main revision as its first step",
		},
		{
			name:    "expected SHA is optional",
			old:     "        required: true\n",
			new:     "        required: false\n",
			contain: "expected_sha must be a required string",
		},
		{
			name:    "expected SHA has the wrong type",
			old:     "        type: string\n",
			new:     "        type: choice\n",
			contain: "expected_sha must be a required string",
		},
		{
			name:    "dispatch may target another branch",
			old:     `[[ "${GITHUB_REF}" == 'refs/heads/main' ]]`,
			new:     `[[ -n "${GITHUB_REF}" ]]`,
			contain: "dispatch verification step must include",
		},
		{
			name:    "dispatch does not compare the resolved SHA",
			old:     `[[ "${GITHUB_SHA}" == "${EXPECTED_SHA}" ]]`,
			new:     `[[ -n "${GITHUB_SHA}" ]]`,
			contain: "dispatch verification step must include",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/ci.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.new, 1)
			if changed == string(data) {
				t.Fatalf("fixture did not contain %q", tc.old)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

func TestCheckRejectsMissingScannerUpdateCoverage(t *testing.T) {
	root := policyFixture(t)
	path := filepath.Join(root, ".github/dependabot.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = []byte(strings.Replace(string(data), "    directory: /.github/scanners\n", "    directory: /.github/scanners-disabled\n", 1))
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	err = Check(root)
	if err == nil || !strings.Contains(err.Error(), "docker-compose|/.github/scanners") {
		t.Fatalf("Check() error = %v, want scanner Dependabot coverage error", err)
	}
}

func TestCheckRejectsSchemaWithoutUpdatedRollbackDeclaration(t *testing.T) {
	root := policyFixture(t)
	path := filepath.Join(root, "internal/store/store.go")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "CREATE TABLE IF NOT EXISTS jobs (", "-- compatibility-affecting schema edit\nCREATE TABLE IF NOT EXISTS jobs (", 1)
	if changed == string(data) {
		t.Fatal("schema fixture marker missing")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Check(root)
	if err == nil || !strings.Contains(err.Error(), "does not cover current schema") {
		t.Fatalf("Check() error = %v, want rollback compatibility hash error", err)
	}
}

// The measurement lane is the only workflow holding `security-events: write`, so
// its blast radius must stay pinned the same way the other lanes are: read-only
// by default, one job, no OIDC grant, no contributor-triggered entry point, and
// no mutable action reference.
// This is the only lane that can write to the protected branch, so every part of
// its containment must stay asserted: a read-only workflow default, one job, no
// third-party action executing with that token, its own independently derived
// guards, and a merge bound to the qualified SHA under squash.
func TestCheckRejectsUnsafeDependabotMerge(t *testing.T) {
	tests := []struct {
		name    string
		old     string
		new     string
		contain string
	}{
		{
			name:    "workflow default is not read-only",
			old:     "permissions:\n  actions: read\n  contents: read\n",
			new:     "permissions:\n  actions: read\n  contents: write\n",
			contain: "default permission contents must be read",
		},
		{
			name:    "merge is not bound to the tested revision",
			old:     "            -f \"sha=${head_sha}\" \\\n",
			new:     "",
			contain: "merge call must bind the qualified SHA",
		},
		{
			name: "extra unbound merge call",
			old:  "            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			new: "            gh api --method PUT \"repos/${REPOSITORY}/pulls/${pull_request}/merge\" \\\n" +
				"              --input - <<<'{\"merge_method\":\"squash\"}'\n" +
				"            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			contain: "exactly one SHA-bound merge",
		},
		{
			name: "indirect extra unbound merge call",
			old:  "            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			new: "            unbound_merge_endpoint=\"repos/${REPOSITORY}/pulls/${pull_request}/\"\"merge\"\n" +
				"            gh api --method PUT \"${unbound_merge_endpoint}\" -f merge_method=squash\n" +
				"            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			contain: "one merge mutation and three documented Actions mutations",
		},
		{
			name: "attached short-method extra merge call",
			old:  "            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			new: "            unbound_merge_endpoint=\"repos/${REPOSITORY}/pulls/${pull_request}/\"\"merge\"\n" +
				"            gh api -XPUT \"${unbound_merge_endpoint}\" -f merge_method=squash\n" +
				"            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			contain: "alternate network mutation form",
		},
		{
			name: "high-level extra merge call",
			old:  "            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			new: "            gh pr merge \"${pull_request}\" --squash\n" +
				"            # Bind the merge to the tested commit. If Dependabot force-pushes a\n",
			contain: "alternate network mutation form",
		},
		{
			name:    "merge method breaks linear history",
			old:     "-f merge_method=squash",
			new:     "-f merge_method=merge",
			contain: "merge call must bind the qualified SHA",
		},
		{
			name:    "cannot dispatch landed main CI",
			old:     "      actions: write\n",
			new:     "      actions: read\n",
			contain: "merge job permission actions must be write",
		},
		{
			name:    "omits post-merge CI dispatch",
			old:     `repos/${REPOSITORY}/actions/workflows/ci.yml/dispatches`,
			new:     `repos/${REPOSITORY}/actions/workflows/disabled.yml/dispatches`,
			contain: "exactly one SHA-bound merge and one exact-main CI dispatch",
		},
		{
			name: "extra unbound CI dispatch",
			old:  "          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			new: "          gh api --method POST \"repos/${REPOSITORY}/actions/workflows/ci.yml/dispatches\" -f ref=${head_branch}\n" +
				"          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			contain: "exactly one SHA-bound merge and one exact-main CI dispatch",
		},
		{
			name: "indirect extra unbound CI dispatch",
			old:  "          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			new: "          unbound_dispatch_endpoint=\"repos/${REPOSITORY}/actions/workflows/ci.yml/\"\"dispatches\"\n" +
				"          gh api --method POST \"${unbound_dispatch_endpoint}\" -f ref=${head_branch}\n" +
				"          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			contain: "one merge mutation and three documented Actions mutations",
		},
		{
			name: "high-level extra CI dispatch",
			old:  "          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			new: "          gh workflow run ci.yml --ref \"${head_branch}\"\n" +
				"          # GITHUB_TOKEN-created commits do not emit another push workflow run.\n",
			contain: "alternate network mutation form",
		},
		{
			name:    "dispatches an arbitrary branch",
			old:     `'{ref:"main", inputs:{expected_sha:$expected_sha}}'`,
			new:     `'{ref:$head_branch, inputs:{expected_sha:$expected_sha}}'`,
			contain: "merge lane must include",
		},
		{
			name:    "dispatch omits the exact landed revision",
			old:     `inputs:{expected_sha:$expected_sha}`,
			new:     `inputs:{}`,
			contain: "merge lane must include",
		},
		{
			name:    "dispatch response is not bound to a run",
			old:     `.workflow_run_id`,
			new:     `.run_id`,
			contain: "merge lane must include",
		},
		{
			name:    "dispatched revision is not rechecked",
			old:     `"${dispatch_sha}" != "${landed_sha}"`,
			new:     `"${dispatch_sha}" == "${landed_sha}"`,
			contain: "merge lane must include",
		},
		{
			name:    "actor guard removed",
			old:     `                "${actor}" != 'dependabot[bot]' ||`,
			new:     `                "${actor}" != 'anyone' ||`,
			contain: "merge lane must include",
		},
		{
			name:    "base repository guard removed",
			old:     `.base.repo.full_name == $repository`,
			new:     `.base.repo.full_name != ""`,
			contain: "merge lane must include",
		},
		{
			name:    "rerun cannot recover a merged pull request",
			old:     "              -f state=all \\\n",
			new:     "              -f state=open \\\n",
			contain: "merge lane must include",
		},
		{
			name:    "rerun ignores source pull request identity",
			old:     `.pull_requests[0].number`,
			new:     `.pull_requests[0].id`,
			contain: "merge lane must include",
		},
		{
			name:    "rerun duplicates an active qualification",
			old:     `gh run list --repo`,
			new:     `gh run view --repo`,
			contain: "merge lane must include",
		},
		{
			name:    "runs a third-party action with the write token",
			old:     "    steps:\n",
			new:     "    steps:\n      - uses: some/action@0000000000000000000000000000000000000000\n",
			contain: "must not run external actions",
		},
		{
			name:    "checks out pull-request code",
			old:     "      - name: Merge the exact Dependabot revision CI tested\n",
			new:     "      - uses: actions/checkout@0000000000000000000000000000000000000000\n",
			contain: "must not access secrets, check out code",
		},
		{
			name:    "triggered directly by a pull request",
			old:     "  workflow_run:\n    workflows: [CI]\n    types: [completed]\n",
			new:     "  pull_request_target:\n",
			contain: "must not access secrets, check out code, or use pull_request_target",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/dependabot-auto-merge.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.new, 1)
			if changed == string(data) {
				t.Fatalf("fixture did not contain %q", tc.old)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

// The reviewing lane must never regain the ability to merge. If merging returns
// to that file, the write grant and the review authority live together again.
func TestApprovalLaneCannotMerge(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(repositoryRoot(t), ".github/workflows/dependabot-auto-approve.yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"contents: write", "/merge", "gh pr merge", "@dependabot"} {
		if strings.Contains(string(data), forbidden) {
			t.Errorf("the review lane must not contain %q", forbidden)
		}
	}
}

// The measurement lane is the only one holding security-events: write, so its
// containment must stay asserted even though it is advisory.
func TestCheckRejectsUnsafeScorecardLane(t *testing.T) {
	for _, tc := range []struct {
		name    string
		old     string
		new     string
		contain string
	}{
		{
			name:    "workflow default is not read-only",
			old:     "permissions:\n  contents: read\n",
			new:     "permissions:\n  contents: write\n",
			contain: "must default to exactly contents: read",
		},
		{
			name:    "escalates to an OIDC token",
			old:     "      security-events: write\n",
			new:     "      security-events: write\n      id-token: write\n",
			contain: "narrow permissions",
		},
		{
			name:    "publishes results",
			old:     "publish_results: false",
			new:     "publish_results: true",
			contain: "must explicitly disable result publication",
		},
		{
			name:    "contributor-triggered entry point",
			old:     "  workflow_dispatch:\n",
			new:     "  pull_request:\n",
			contain: "triggers",
		},
		{
			name:    "secret access",
			old:     "          results_file: scorecard.sarif\n",
			new:     "          repo_token: ${{ secrets.SCORECARD_TOKEN }}\n",
			contain: "must not access secrets",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/scorecard.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			changed := strings.Replace(string(data), tc.old, tc.new, 1)
			if changed == string(data) {
				t.Fatalf("fixture did not contain %q", tc.old)
			}
			if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), tc.contain) {
				t.Fatalf("Check() error = %v, want %q", err, tc.contain)
			}
		})
	}
}

func TestCheckRejectsDispatchControlledCanaryProviderURL(t *testing.T) {
	root := policyFixture(t)
	path := filepath.Join(root, ".github/workflows/canary.yml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := strings.Replace(string(data), "vars.THORNHILL_CANARY_PROVIDER_URL", "inputs.provider_url", 1)
	if changed == string(data) {
		t.Fatal("fixture did not contain the protected provider URL variable")
	}
	if err := os.WriteFile(path, []byte(changed), 0o600); err != nil {
		t.Fatal(err)
	}
	err = Check(root)
	if err == nil || !strings.Contains(err.Error(), "dispatch-controlled URLs") {
		t.Fatalf("Check() error = %v, want dispatch-controlled URL error", err)
	}
}

// A mutable action reference is rejected remotely by the repository's action
// allowlist, but only at workflow startup: the run reports startup_failure with no
// jobs and no logs, which is an expensive way to find out. This must fail the
// required check on the pull request instead, next to the reason.
func TestCheckRejectsUnpinnedWorkflowActions(t *testing.T) {
	// Derive the pin from the fixture rather than hardcoding a SHA: these are
	// exactly the references Dependabot rewrites, so a literal would rot on the
	// next bump and fail for the wrong reason.
	pinned := regexp.MustCompile(`(uses: \S+)@[0-9a-f]{40}`)
	for _, tc := range []struct {
		name        string
		replacement string
	}{
		{name: "tag reference", replacement: "${1}@v7"},
		{name: "branch reference", replacement: "${1}@main"},
		{name: "abbreviated sha", replacement: "${1}@9c091bb"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := policyFixture(t)
			path := filepath.Join(root, ".github/workflows/ci.yml")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !pinned.Match(data) {
				t.Fatal("fixture contains no SHA-pinned action to unpin")
			}
			changed := pinned.ReplaceAll(data, []byte(tc.replacement))
			if err := os.WriteFile(path, changed, 0o600); err != nil {
				t.Fatal(err)
			}
			err = Check(root)
			if err == nil || !strings.Contains(err.Error(), "unpinned action") {
				t.Fatalf("Check() error = %v, want an unpinned-action error", err)
			}
		})
	}
}

func TestPinnedActionSHARejectsMutableReferences(t *testing.T) {
	pinned := "actions/checkout@9c091bb21b7c1c1d1991bb908d89e4e9dddfe3e0"
	if _, ok := pinnedActionSHA(pinned); !ok {
		t.Errorf("pinnedActionSHA(%q) rejected a full commit SHA", pinned)
	}
	for _, mutable := range []string{
		"actions/checkout@v7",
		"actions/checkout@main",
		"actions/checkout",
		// Abbreviated SHAs are still ambiguous and must not count as a pin.
		"actions/checkout@9c091bb",
		// Right length, not hexadecimal.
		"actions/checkout@zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz",
	} {
		if _, ok := pinnedActionSHA(mutable); ok {
			t.Errorf("pinnedActionSHA(%q) accepted a mutable reference", mutable)
		}
	}
}
