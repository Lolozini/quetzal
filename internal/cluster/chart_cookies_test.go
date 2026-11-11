package cluster

import (
	"os/exec"
	"strings"
	"testing"
)

// The panel's session cookies must be Secure as soon as the chart's own ingress
// serves TLS. They used to follow `secureCookies` alone, so an install that set
// `ingress.tls=true` and nothing else sent its session cookie over plain HTTP too.
func TestChartSecureCookiesFollowIngressTLS(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm is not installed")
	}
	render := func(set ...string) string {
		t.Helper()
		args := []string{"template", "q", "../../deploy/quetzal", "--show-only", "templates/deployment.yaml"}
		for _, s := range set {
			args = append(args, "--set", s)
		}
		out, err := exec.Command(helm, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("helm template %v: %v\n%s", set, err, out)
		}
		lines := strings.Split(string(out), "\n")
		for i, l := range lines {
			if strings.Contains(l, "name: QUETZAL_SECURE_COOKIES") && i+1 < len(lines) {
				return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(lines[i+1]), "value:"))
			}
		}
		t.Fatalf("QUETZAL_SECURE_COOKIES is not set by the chart (%v)", set)
		return ""
	}
	for _, tc := range []struct {
		name string
		set  []string
		want string
	}{
		{"default", nil, `"false"`},
		{"explicit", []string{"secureCookies=true"}, `"true"`},
		{"ingress without tls", []string{"ingress.enabled=true"}, `"false"`},
		{"tls without the ingress", []string{"ingress.tls=true"}, `"false"`},
		{"ingress with tls", []string{"ingress.enabled=true", "ingress.tls=true"}, `"true"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := render(tc.set...); got != tc.want {
				t.Fatalf("QUETZAL_SECURE_COOKIES = %s, want %s", got, tc.want)
			}
		})
	}
}
