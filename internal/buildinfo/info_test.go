package buildinfo

import "testing"

func TestValid(t *testing.T) {
	old := Commit
	t.Cleanup(func() { Commit = old })
	for value, want := range map[string]bool{
		"unknown": false,
		"a58e47b": false,
		"a58e47b0149e6f10d9b4a56ff6710fee6fc799eb": true,
		"A58E47B0149E6F10D9B4A56FF6710FEE6FC799EB": false,
	} {
		Commit = value
		if got := Valid(); got != want {
			t.Fatalf("Valid() for %q = %v, want %v", value, got, want)
		}
	}
}
