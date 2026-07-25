package cipolicy

import (
	"os"
	"path/filepath"
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
		".github/workflows/ci.yml",
		".github/workflows/fuzz.yml",
		".github/workflows/scorecard.yml",
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
			name:    "merge is not delegated to Dependabot",
			old:     "@dependabot squash and merge",
			new:     "gh pr merge --auto --squash",
			contain: "approval lane must include",
		},
		{
			name:    "merge request is not bound to the tested revision",
			old:     `merge_marker="Requested squash merge for ${head_sha}."`,
			new:     `merge_marker="Requested squash merge."`,
			contain: "approval lane must include",
		},
		{
			name: "merge lane takes write access to the protected branch",
			old:  "  contents: read\n",
			new:  "  contents: write\n",
			// Delegating the merge to Dependabot is what keeps this lane free of
			// branch write access; escalating it must remain a policy failure.
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
func TestCheckRejectsUnsafeScorecardLane(t *testing.T) {
	tests := []struct {
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
			name:    "unpinned action reference",
			old:     "ossf/scorecard-action@2d1146689b8cda280b9bc96326124645441f03bc # v2.4.4",
			new:     "ossf/scorecard-action@v2.4.4",
			contain: "unpinned action",
		},
		{
			name:    "secret access",
			old:     "          results_file: scorecard.sarif\n",
			new:     "          token: ${{ secrets.SCORECARD_TOKEN }}\n",
			contain: "must not access secrets",
		},
	}
	for _, tc := range tests {
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
