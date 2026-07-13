package main

import (
	"fmt"
	"os"

	"thornhill/internal/cipolicy"
)

func main() {
	if err := cipolicy.Check("."); err != nil {
		fmt.Fprintln(os.Stderr, "CI policy:", err)
		os.Exit(1)
	}
	fmt.Println("CI policy is branch-protection ready: Go, web, and image build")
}
