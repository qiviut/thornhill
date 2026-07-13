package store

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"
)

func fuzzPatternKeys(data []byte) []string {
	if len(data) > 1024 {
		data = data[:1024]
	}
	parts := bytes.Split(data, []byte{0})
	if len(parts) > 32 {
		parts = parts[:32]
	}
	keys := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) > 256 {
			part = part[:256]
		}
		keys = append(keys, string(part))
	}
	return keys
}

func FuzzApprovalPatternHash(f *testing.F) {
	f.Add([]byte("shell command\x00network access"))
	f.Add([]byte(" duplicate \x00duplicate\x00"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		keys := fuzzPatternKeys(data)
		original := ApprovalPatternHash(keys)

		permuted := make([]string, 0, len(keys)*2)
		for i := len(keys) - 1; i >= 0; i-- {
			permuted = append(permuted, " \t"+keys[i]+"\n ", keys[i])
		}
		if got := ApprovalPatternHash(permuted); got != original {
			t.Fatalf("permutation/space/duplicate changed identity: %q != %q", got, original)
		}

		if original == "" {
			for _, key := range keys {
				if strings.TrimSpace(key) != "" {
					t.Fatalf("non-empty normalized set produced empty hash: %#v", keys)
				}
			}
			return
		}

		sum := sha256.Sum256(data)
		unique := fmt.Sprintf("fuzz-unique-%x", sum[:])
		for i := 0; ApprovalPatternHash(append(append([]string(nil), keys...), unique)) == original; i++ {
			if i >= 64 {
				t.Fatal("could not construct a distinct bounded pattern")
			}
			unique += "#"
		}
		if got := ApprovalPatternHash(append(append([]string(nil), keys...), unique)); got == original || got == "" {
			t.Fatalf("adding unique key did not change identity: %q", got)
		}
	})
}
