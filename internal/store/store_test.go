package store

import "testing"

func TestApprovalPatternHashIsExactSetIdentity(t *testing.T) {
	t.Parallel()
	a := ApprovalPatternHash([]string{" shell command via -c ", "network access", "network access"})
	b := ApprovalPatternHash([]string{"network access", "shell command via -c"})
	if a == "" || a != b {
		t.Fatalf("same normalized set produced %q and %q", a, b)
	}
	if a == ApprovalPatternHash([]string{"shell command via -c"}) {
		t.Fatal("subset must not match full pattern set")
	}
	if a == ApprovalPatternHash([]string{"shell command via -c", "network access", "filesystem write"}) {
		t.Fatal("superset must not match full pattern set")
	}
	if got := ApprovalPatternHash(nil); got != "" {
		t.Fatalf("empty pattern set hash = %q", got)
	}
}

// A spoken job reference is literal text. Without escaping, a name containing
// LIKE metacharacters would turn the operator's own words into a wildcard: "50%"
// would match every active job, and "read_file" would match "read-file" too.
func TestEscapeLikePatternNeutralizesWildcards(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "plain audit", want: "plain audit"},
		{in: "50% headroom", want: `50\% headroom`},
		{in: "read_file audit", want: `read\_file audit`},
		{in: `back\slash`, want: `back\\slash`},
		{in: `%_\`, want: `\%\_\\`},
		{in: "", want: ""},
		// Multi-byte characters must survive intact.
		{in: "café 50%", want: `café 50\%`},
	}
	for _, tc := range tests {
		if got := escapeLikePattern(tc.in); got != tc.want {
			t.Errorf("escapeLikePattern(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
