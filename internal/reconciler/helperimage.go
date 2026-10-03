package reconciler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/lolozini/quetzal/internal/models"
)

// A server's pods run helpers out of the Quetzal image: the config renderer
// (render-copy), the SFTP server (sftp-copy) and the activator. That image is
// the panel's own, and its tag changes with every release, so each upgrade
// changed the pod template of every server that had a helper and Kubernetes
// restarted them all at once, players included.
//
// A running pod now keeps the helper image it has for as long as nothing else
// in its template changes. The new one arrives with the next change that
// restarts the pod anyway: a stop and start, hibernation, a new setting.

// podSpecAnnotation records on a server's Deployment the hash of the pod
// template it was last given, helper images left out. Two equal hashes mean the
// template differs by its helper image at most.
const podSpecAnnotation = "quetzal.dev/pod-spec"

// helperGeneration is part of that hash. Bump it when running pods must get the
// new helpers with the upgrade itself, for instance when the panel stops
// accepting what an older activator sends: every hash then changes, and every
// pod with a helper restarts.
const helperGeneration = "1"

// applyKeepingHelpers applies want, whose helper containers run systemImage,
// after giving it back the live Deployment's helper image if that is all that
// would change on a running pod.
func (r *Reconciler) applyKeepingHelpers(ctx context.Context, want *appsv1.Deployment, systemImage string) error {
	return r.applyRolling(ctx, want, systemImage, nil)
}

// applyRolling is applyKeepingHelpers, calling beforeRoll first when the apply
// is about to replace a running pod.
func (r *Reconciler) applyRolling(ctx context.Context, want *appsv1.Deployment, systemImage string, beforeRoll func()) error {
	live := &appsv1.Deployment{}
	if err := r.Client.Get(ctx, client.ObjectKeyFromObject(want), live); err != nil {
		if !apierrors.IsNotFound(err) {
			return err
		}
		live = nil
	}
	if !keepHelperImage(live, want, systemImage) && isLegacy(live) {
		r.keepLegacyHelperImage(ctx, live, want, systemImage)
	}
	if beforeRoll != nil && rolls(live, want) {
		beforeRoll()
	}
	return r.apply(ctx, want)
}

// rolls reports whether applying want replaces a pod live runs: both ask for
// one, and their pod templates differ. A Deployment made before the pod spec
// hash cannot tell, and counts as not rolling.
func rolls(live, want *appsv1.Deployment) bool {
	if live == nil || !scaledUp(live) || !scaledUp(want) || live.Status.ReadyReplicas == 0 {
		return false
	}
	h := live.Annotations[podSpecAnnotation]
	return h != "" && h != want.Annotations[podSpecAnnotation]
}

// applyNewHelpers applies want as it is, new helper image included, stamped
// with its hash as applyKeepingHelpers would: for a pod that takes a new image
// without anyone noticing.
func (r *Reconciler) applyNewHelpers(ctx context.Context, want *appsv1.Deployment, systemImage string) error {
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations[podSpecAnnotation] = podSpecHash(want, systemImage)
	return r.apply(ctx, want)
}

// activatorTakesNewHelpers reports whether a server's activator may take a new
// image at once, where the game's pods wait for their next restart: while the
// server sleeps, when nobody goes through it but a player waking it, who at
// worst tries again. A drop-mode activator only exists then; a proxy runs for
// as long as its server may sleep, carries every player while the game is up,
// and otherwise kept the image it started with for good, and with it the wake
// rules of that version.
func activatorTakesNewHelpers(s *models.Server) bool {
	return s.Hibernated
}

// renderingChanges undo, newest first, what a release changed in the pods it
// renders for a server nobody changed. A running pod rendered by an earlier
// release keeps that rendering, as it keeps its helper image, until something
// restarts it anyway: a stop and start, hibernation, a new setting. Otherwise
// each such change restarts every running server with the upgrade that brings
// it, players and all.
var renderingChanges = []func(*appsv1.Deployment){
	undoResources07,
}

// undoResources07 is a pod as 0.6 rendered it: no resources on the install
// container and the helpers, and the game's memory limit at the memory set,
// without Wings' headroom or a request.
func undoResources07(d *appsv1.Deployment) {
	spec := &d.Spec.Template.Spec
	for i := range spec.InitContainers {
		switch spec.InitContainers[i].Name {
		case InstallContainer, RenderCopyContainer, RenderConfigContainer, "sftp-copy":
			spec.InitContainers[i].Resources = corev1.ResourceRequirements{}
		}
	}
	if d.Name != workloadName {
		return // the data-manager and the activator kept their resources
	}
	for i := range spec.Containers {
		c := &spec.Containers[i]
		if c.Name != workloadName {
			continue
		}
		if mem, ok := c.Resources.Requests[corev1.ResourceMemory]; ok {
			c.Resources.Limits[corev1.ResourceMemory] = mem
		}
		c.Resources.Requests = nil
	}
}

// renderings returns want as this release renders it, then as each earlier one
// did, newest first.
func renderings(want *appsv1.Deployment) []*appsv1.Deployment {
	out := []*appsv1.Deployment{want.DeepCopy()}
	earlier := want.DeepCopy()
	for _, undo := range renderingChanges {
		undo(earlier)
		out = append(out, earlier.DeepCopy())
	}
	return out
}

// keepHelperImage stamps want with its pod spec hash and, while live keeps
// running what an earlier release or another helper image rendered for a
// server nobody changed, gives want back the template live runs. It reports
// whether it did.
func keepHelperImage(live, want *appsv1.Deployment, systemImage string) bool {
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations[podSpecAnnotation] = podSpecHash(want, systemImage)
	if live == nil || live.Annotations[podSpecAnnotation] == "" {
		return false
	}
	for i, r := range renderings(want) {
		hash := podSpecHash(r, systemImage)
		if hash != live.Annotations[podSpecAnnotation] {
			continue
		}
		if i == 0 { // this release's rendering: the helper image differs at most
			return systemImage != "" && withHelpersOf(live, want, systemImage)
		}
		if !scaledUp(live) || !scaledUp(want) {
			return false // nothing runs, or nothing will: the new rendering restarts nothing
		}
		if systemImage != "" {
			withHelpersOf(live, r, systemImage)
		}
		r.Annotations[podSpecAnnotation] = hash
		*want = *r
		return true
	}
	return false
}

// isLegacy reports whether live predates podSpecAnnotation.
func isLegacy(live *appsv1.Deployment) bool {
	return live != nil && live.Annotations[podSpecAnnotation] == ""
}

// keepLegacyHelperImage does keepHelperImage's job for a Deployment made before
// the annotation, which has no hash to compare: it asks the API server what the
// template would become with the live helper image, and keeps that image if the
// answer is the live template. Without it, the upgrade that brings the
// annotation would restart every pod with a helper one last time. Any error
// leaves want as it is: the pod restarts, as it used to.
//
// The candidates are this release's rendering and each earlier one's, with the
// live helper image: a Deployment that old was rendered by an older release.
func (r *Reconciler) keepLegacyHelperImage(ctx context.Context, live, want *appsv1.Deployment, systemImage string) {
	if !scaledUp(live) || !scaledUp(want) {
		return
	}
	for _, candidate := range renderings(want) {
		// Hashed before the live helper image goes back in: the hash leaves out
		// the current helper image only, and the next pass compares against it.
		hash := podSpecHash(candidate, systemImage)
		if systemImage != "" {
			withHelpersOf(live, candidate, systemImage)
		}
		dry := candidate.DeepCopy()
		err := r.Client.Patch(ctx, dry, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership, client.DryRunAll)
		if err != nil {
			return
		}
		if equality.Semantic.DeepEqual(dry.Spec.Template, live.Spec.Template) {
			candidate.Annotations[podSpecAnnotation] = hash
			*want = *candidate
			return
		}
	}
}

// withHelpersOf gives want's helper containers, those that run systemImage, the
// image and pull policy they have in live, provided both keep running and live
// has every one of them. It reports whether it did.
func withHelpersOf(live, want *appsv1.Deployment, systemImage string) bool {
	if !scaledUp(live) || !scaledUp(want) {
		return false // nothing runs, or nothing will: the new image restarts nothing
	}
	liveContainers := map[string]corev1.Container{}
	for _, c := range append(append([]corev1.Container(nil), live.Spec.Template.Spec.InitContainers...), live.Spec.Template.Spec.Containers...) {
		liveContainers[c.Name] = c
	}
	spec := &want.Spec.Template.Spec
	changed := false
	for _, cs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range cs {
			if cs[i].Image != systemImage {
				continue
			}
			if _, ok := liveContainers[cs[i].Name]; !ok {
				return false // a helper the live pod lacks: the template changes anyway
			}
		}
	}
	for _, cs := range [][]corev1.Container{spec.InitContainers, spec.Containers} {
		for i := range cs {
			if cs[i].Image != systemImage {
				continue
			}
			old := liveContainers[cs[i].Name]
			if old.Image != cs[i].Image {
				changed = true
			}
			cs[i].Image, cs[i].ImagePullPolicy = old.Image, old.ImagePullPolicy
		}
	}
	return changed
}

// podSpecHash hashes a Deployment's pod template with the image and pull
// policy of its helpers, the containers that run systemImage, left out.
func podSpecHash(d *appsv1.Deployment, systemImage string) string {
	tmpl := d.Spec.Template.DeepCopy()
	for _, cs := range [][]corev1.Container{tmpl.Spec.InitContainers, tmpl.Spec.Containers} {
		for i := range cs {
			if systemImage != "" && cs[i].Image == systemImage {
				cs[i].Image, cs[i].ImagePullPolicy = "", ""
			}
		}
	}
	b, _ := json.Marshal(tmpl)
	sum := sha256.Sum256(append([]byte(helperGeneration+"\n"), b...))
	return hex.EncodeToString(sum[:12])
}

// scaledUp reports whether a Deployment asks for pods (unset means one).
func scaledUp(d *appsv1.Deployment) bool {
	return d.Spec.Replicas == nil || *d.Spec.Replicas > 0
}
