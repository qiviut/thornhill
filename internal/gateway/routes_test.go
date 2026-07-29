package gateway

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"thornhill/internal/config"
)

const (
	routeSecurityBegin = "<!-- route-security:begin -->"
	routeSecurityEnd   = "<!-- route-security:end -->"
)

func TestEveryRouteHasSecurityClassification(t *testing.T) {
	g := &Gateway{
		Cfg:   &config.Config{PrebakeDir: t.TempDir(), StaticDir: t.TempDir()},
		Hooks: func(_ http.ResponseWriter, _ *http.Request) {},
	}
	seen := make(map[string]struct{})
	for _, route := range g.routeSpecs() {
		classification := route.Security
		for field, value := range map[string]string{
			"pattern": classification.Pattern, "caller": classification.Caller,
			"boundary": classification.Boundary, "data": classification.Data,
			"authority": classification.Authority,
		} {
			if strings.TrimSpace(value) == "" {
				t.Errorf("route %q has empty %s classification", classification.Pattern, field)
			}
		}
		if _, duplicate := seen[classification.Pattern]; duplicate {
			t.Errorf("route pattern %q is classified more than once", classification.Pattern)
		}
		seen[classification.Pattern] = struct{}{}
	}
}

func TestSecurityModelRouteInventoryMatchesCode(t *testing.T) {
	g := &Gateway{
		Cfg:   &config.Config{PrebakeDir: t.TempDir(), StaticDir: t.TempDir()},
		Hooks: func(_ http.ResponseWriter, _ *http.Request) {},
	}
	want := strings.TrimSpace(renderRouteSecurity(g.routeSpecs()))
	path := filepath.Join("..", "..", "docs", "security-model.md")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := markedSection(string(data), routeSecurityBegin, routeSecurityEnd)
	if !ok {
		t.Fatalf("%s must contain one %s ... %s block", path, routeSecurityBegin, routeSecurityEnd)
	}
	if strings.TrimSpace(got) != want {
		t.Fatalf("gateway route security inventory drifted; replace its marked block with:\n\n%s", want)
	}
}

func renderRouteSecurity(routes []routeSpec) string {
	var out strings.Builder
	fmt.Fprintln(&out, routeSecurityBegin)
	fmt.Fprintln(&out, "| Route | Expected caller | Enforced boundary | Data exposed or accepted | Authority |")
	fmt.Fprintln(&out, "| --- | --- | --- | --- | --- |")
	for _, route := range routes {
		classification := route.Security
		fmt.Fprintf(&out, "| `%s` | %s | %s | %s | %s |\n",
			classification.Pattern, classification.Caller, classification.Boundary,
			classification.Data, classification.Authority)
	}
	fmt.Fprintln(&out, routeSecurityEnd)
	return out.String()
}

func markedSection(document, begin, end string) (string, bool) {
	start := strings.Index(document, begin)
	if start < 0 {
		return "", false
	}
	finish := strings.Index(document[start:], end)
	if finish < 0 {
		return "", false
	}
	finish = start + finish + len(end)
	if strings.Contains(document[finish:], begin) {
		return "", false
	}
	return document[start:finish], true
}
