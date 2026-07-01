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

func TestParseNetDev(t *testing.T) {
	raw := []byte(`Inter-|   Receive                                                |  Transmit
 face |bytes    packets errs drop fifo frame compressed multicast|bytes    packets errs drop fifo colls carrier compressed
    lo:  100      2    0    0    0     0          0         0      100       2    0    0    0     0       0          0
  eth0: 1000     10    0    0    0     0          0         0     2000      20    0    0    0     0       0          0
  eth1:  500      5    0    0    0     0          0         0      250       3    0    0    0     0       0          0`)
	rx, tx := ParseNetDev(raw)
	if rx != 1500 { // eth0 + eth1, lo excluded
		t.Errorf("rx = %d, want 1500", rx)
	}
	if tx != 2250 {
		t.Errorf("tx = %d, want 2250", tx)
	}
}

func TestParseDuUsed(t *testing.T) {
	if got := ParseDuUsed([]byte("204800\t/home/container\n")); got != 204800*1024 {
		t.Errorf("used = %d, want %d", got, int64(204800)*1024)
	}
	// du without a trailing path (BusyBox) still parses the leading count.
	if got := ParseDuUsed([]byte("512\n")); got != 512*1024 {
		t.Errorf("used = %d, want %d", got, int64(512)*1024)
	}
	// Empty / non-numeric input returns -1 (not a panic, not a bogus 0).
	if got := ParseDuUsed([]byte("   ")); got != -1 {
		t.Errorf("empty = %d, want -1", got)
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
