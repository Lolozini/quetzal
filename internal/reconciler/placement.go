package reconciler

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
)

// A server is tied to a node: the data-manager holds its volume there, and the
// game and the activator follow it. When that node stopped answering, the
// panel said nothing: a sleeping server stayed "Hibernated", one being started
// sat on "Starting" with no message while its pod could not be scheduled, and
// the file manager waited two minutes before blaming a restore.

// NodeReady reports whether a node's Ready condition is true.
func NodeReady(n *corev1.Node) bool {
	for _, c := range n.Status.Conditions {
		if c.Type == corev1.NodeReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// PodCondition returns a pod's condition of type t, or nil.
func PodCondition(p *corev1.Pod, t corev1.PodConditionType) *corev1.PodCondition {
	for i := range p.Status.Conditions {
		if p.Status.Conditions[i].Type == t {
			return &p.Status.Conditions[i]
		}
	}
	return nil
}

// Unschedulable returns why the scheduler cannot place a pod, or "" when it
// can (or has).
func Unschedulable(p *corev1.Pod) string {
	c := PodCondition(p, corev1.PodScheduled)
	if p.Status.Phase != corev1.PodPending || c == nil || c.Status != corev1.ConditionFalse || c.Reason != corev1.PodReasonUnschedulable {
		return ""
	}
	if c.Message == "" {
		return "no node can take it"
	}
	return c.Message
}

// placementProblem says why a server's pods cannot run where they must, when
// it is the cluster's doing: no node can take its game pod (unscheduled, from
// inspectPods), the node that holds its data is not answering, or no node can
// take the data-manager. "" when nothing of the sort is known.
func (r *Reconciler) placementProblem(ctx context.Context, s *models.Server, unscheduled string) string {
	if unscheduled != "" {
		return "no node can run it: " + unscheduled
	}
	var pods corev1.PodList
	if err := r.Client.List(ctx, &pods, client.InNamespace(s.Namespace), client.MatchingLabels{DataLabel: s.Slug}); err != nil {
		return ""
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if why := Unschedulable(p); why != "" {
			return "no node can hold its data: " + why
		}
		if p.Spec.NodeName == "" {
			continue
		}
		var node corev1.Node
		if err := r.Client.Get(ctx, client.ObjectKey{Name: p.Spec.NodeName}, &node); err != nil {
			return ""
		}
		if !NodeReady(&node) {
			return fmt.Sprintf("the node %s, which holds its data, is not responding", node.Name)
		}
	}
	return ""
}
