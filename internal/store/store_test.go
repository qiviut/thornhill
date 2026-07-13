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
