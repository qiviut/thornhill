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
		"Dockerfile",
		"Dockerfile.postgres",
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
