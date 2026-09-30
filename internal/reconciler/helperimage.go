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
	return r.apply(ctx, want)
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

// keepHelperImage stamps want with its pod spec hash and, when live runs the
// same template with another helper image and keeps running, gives want that
// image back. It reports whether it did.
func keepHelperImage(live, want *appsv1.Deployment, systemImage string) bool {
	hash := podSpecHash(want, systemImage)
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations[podSpecAnnotation] = hash
	if systemImage == "" || live == nil || live.Annotations[podSpecAnnotation] != hash {
		return false
	}
	return withHelpersOf(live, want, systemImage)
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
func (r *Reconciler) keepLegacyHelperImage(ctx context.Context, live, want *appsv1.Deployment, systemImage string) {
	if systemImage == "" {
		return
	}
	candidate := want.DeepCopy()
	if !withHelpersOf(live, candidate, systemImage) {
		return
	}
	dry := candidate.DeepCopy()
	err := r.Client.Patch(ctx, dry, client.Apply, client.FieldOwner(fieldOwner), client.ForceOwnership, client.DryRunAll)
	if err != nil || !equality.Semantic.DeepEqual(dry.Spec.Template, live.Spec.Template) {
		return
	}
	*want = *candidate
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
