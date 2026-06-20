package stats

import "testing"

func TestParseUsageSumsContainers(t *testing.T) {
	raw := []byte(`{
		"containers": [
			{"name": "server", "usage": {"cpu": "150m", "memory": "256Mi"}},
			{"name": "sidecar", "usage": {"cpu": "50m", "memory": "10Mi"}}
		]
	}`)
	u, err := parseUsage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.CPUMillicores != 200 {
		t.Errorf("cpu = %dm, want 200m", u.CPUMillicores)
	}
	if want := int64(266 * 1024 * 1024); u.MemoryBytes != want {
		t.Errorf("memory = %d, want %d", u.MemoryBytes, want)
	}
}

func TestParseUsageHandlesNanoCPU(t *testing.T) {
	// metrics-server commonly reports CPU in nanocores (e.g. "12000000n").
	raw := []byte(`{"containers":[{"usage":{"cpu":"12000000n","memory":"1073741824"}}]}`)
	u, err := parseUsage(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if u.CPUMillicores != 12 {
		t.Errorf("cpu = %dm, want 12m", u.CPUMillicores)
	}
	if u.MemoryBytes != 1073741824 {
		t.Errorf("memory = %d, want 1GiB", u.MemoryBytes)
	}
}
