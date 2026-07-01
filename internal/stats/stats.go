// Package stats reads live resource usage for a server's pod from the
// Kubernetes metrics API (metrics.k8s.io, served by metrics-server). It avoids
// pulling in the typed metrics client by talking to the aggregated API
// directly, keeping the dependency set pinned.
package stats

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/client-go/kubernetes"
)

// ErrUnavailable indicates resource metrics could not be read: either the
// metrics API is absent (metrics-server not installed) or the pod has no
// sample yet (just started). Lets the caller return a clear hint, not a 500.
var ErrUnavailable = errors.New("resource metrics unavailable (metrics-server not installed, or the pod has no sample yet)")

// Usage is a pod's aggregated resource consumption.
type Usage struct {
	CPUMillicores int64 `json:"cpuMillicores"`
	MemoryBytes   int64 `json:"memoryBytes"`
}

// PodUsage returns the summed CPU/memory usage across a pod's containers.
func PodUsage(ctx context.Context, cs kubernetes.Interface, ns, pod string) (Usage, error) {
	raw, err := cs.CoreV1().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/namespaces", ns, "pods", pod).
		DoRaw(ctx)
	if err != nil {
		// A missing aggregated API surfaces as NotFound/ServiceUnavailable.
		if apierrors.IsNotFound(err) || apierrors.IsServiceUnavailable(err) {
			return Usage{}, ErrUnavailable
		}
		return Usage{}, err
	}
	return parseUsage(raw)
}

// parseUsage decodes a metrics.k8s.io PodMetrics document into a Usage.
func parseUsage(raw []byte) (Usage, error) {
	var pm struct {
		Containers []struct {
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"containers"`
	}
	if err := json.Unmarshal(raw, &pm); err != nil {
		return Usage{}, err
	}
	var u Usage
	for _, c := range pm.Containers {
		if c.Usage.CPU != "" {
			if q, err := resource.ParseQuantity(c.Usage.CPU); err == nil {
				u.CPUMillicores += q.MilliValue()
			}
		}
		if c.Usage.Memory != "" {
			if q, err := resource.ParseQuantity(c.Usage.Memory); err == nil {
				u.MemoryBytes += q.Value()
			}
		}
	}
	return u, nil
}

// ParseNetDev sums cumulative receive/transmit bytes across a pod's network
// interfaces (loopback excluded) from the contents of /proc/net/dev. The
// counters are cumulative since boot; callers derive a rate from successive
// samples. Pods share a network namespace, so this covers all containers.
func ParseNetDev(raw []byte) (rxBytes, txBytes int64) {
	for _, line := range strings.Split(string(raw), "\n") {
		colon := strings.IndexByte(line, ':')
		if colon < 0 {
			continue // header lines have no "iface:" prefix
		}
		iface := strings.TrimSpace(line[:colon])
		if iface == "" || iface == "lo" {
			continue
		}
		// Receive block is the first 8 columns; transmit bytes is column 9 (idx 8).
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		if v, err := strconv.ParseInt(fields[0], 10, 64); err == nil {
			rxBytes += v
		}
		if v, err := strconv.ParseInt(fields[8], 10, 64); err == nil {
			txBytes += v
		}
	}
	return rxBytes, txBytes
}

// ParseDuUsed reads used bytes from `du -sk <path>` output (a single summary
// record "<kib>\t<path>", KiB blocks). Returns -1 when it can't parse. Used
// instead of df for per-volume usage: on local-path (hostPath-backed) PVCs df
// reports the whole host filesystem, so only du of the data dir is meaningful.
func ParseDuUsed(raw []byte) int64 {
	f := strings.Fields(strings.TrimSpace(string(raw)))
	if len(f) < 1 {
		return -1
	}
	kib, err := strconv.ParseInt(f[0], 10, 64)
	if err != nil {
		return -1
	}
	return kib * 1024
}
