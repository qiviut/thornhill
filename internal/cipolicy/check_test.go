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

func policyFixture(t *testing.T) string {
	t.Helper()
	source := repositoryRoot(t)
	target := t.TempDir()
	for _, relative := range []string{
		".github/branch-protection.json",
		".github/workflows/ci.yml",
		".github/workflows/fuzz.yml",
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
