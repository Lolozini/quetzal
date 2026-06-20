// Package stats reads live resource usage for a server's pod from the
// Kubernetes metrics API (metrics.k8s.io, served by metrics-server). It avoids
// pulling in the typed metrics client by talking to the aggregated API
// directly, keeping the dependency set pinned.
package stats

import (
	"context"
	"encoding/json"
	"errors"

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
