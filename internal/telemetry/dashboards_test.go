package telemetry

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestDashboardsUseRealMetrics fails when the Grafana dashboard or the
// alerting rules name a metric the gateway does not export, such as
// after a rename.
func TestDashboardsUseRealMetrics(t *testing.T) {
	src, err := os.ReadFile("metrics.go")
	if err != nil {
		t.Fatal(err)
	}
	exported := map[string]bool{}
	for _, m := range regexp.MustCompile(`"((?:gog|gen_ai)_[a-z_]+)"`).FindAllStringSubmatch(string(src), -1) {
		exported[m[1]] = true
	}
	use := regexp.MustCompile(`\b((?:gog|gen_ai)_[a-z_]+)`)
	for _, f := range []string{"../../deploy/helm/genaiopengateway/files/dashboard.json", "../../deploy/helm/genaiopengateway/files/alerts.yaml"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range use.FindAllStringSubmatch(string(b), -1) {
			name := m[1]
			for _, suffix := range []string{"_bucket", "_sum", "_count"} {
				if base, ok := strings.CutSuffix(name, suffix); ok && exported[base] {
					name = base
				}
			}
			if strings.HasPrefix(name, "gen_ai_") && !strings.HasPrefix(name, "gen_ai_client_") {
				continue // a label, such as gen_ai_request_model
			}
			if !exported[name] {
				t.Errorf("%s uses %s, which the gateway does not export", f, m[1])
			}
		}
	}
}
