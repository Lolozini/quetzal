package reconciler

import (
	"context"
	"log"
	"maps"
	"strings"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// imageLookupTimeout bounds the lookups a start waits on: a registry that does
// not answer holds the game back that long at most, and it starts on what it
// ran last.
const imageLookupTimeout = 5 * time.Second

// refreshImagePins looks up, when the game's pod is about to be made -- a
// start, the start half of a restart, a wake, a first install -- what the tags
// of its images point at now, and pins the pod to that. Its images were pulled
// IfNotPresent: a node that had a tag kept it for good, so a fix published
// upstream, an image rebuilt for a CVE, never reached a server that had run the
// old one. Pterodactyl's Wings pulls on every start; this is the same, and a
// registry that does not answer leaves the digest the game ran last, as Wings
// falls back on the image it has.
//
// Never while the game runs: a pin is part of the pod, and changing it would
// restart the game.
func (r *Reconciler) refreshImagePins(ctx context.Context, srv *models.Server, t *models.Template) {
	if r.ResolveImage == nil || srv.Replicas() == 0 {
		return
	}
	if running, err := r.deploymentRunning(ctx, srv.Namespace); err != nil || running {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, imageLookupTimeout)
	defer cancel()
	pins := map[string]string{}
	for _, ref := range []string{srv.Image, installImage(t)} {
		if ref == "" || strings.Contains(ref, "@") {
			continue // nothing to run, or a digest already
		}
		if _, seen := pins[ref]; seen {
			continue
		}
		d, err := r.ResolveImage(ctx, ref)
		if err != nil {
			last := srv.ImagePins[ref]
			if last == "" {
				last = "whatever its node has"
			}
			log.Printf("server %s: looking up %s: %v; it starts on %s", srv.Slug, ref, err, last)
			if srv.ImagePins[ref] != "" {
				pins[ref] = srv.ImagePins[ref]
			}
			continue
		}
		pins[ref] = d
	}
	if maps.Equal(pins, srv.ImagePins) {
		return
	}
	if err := r.Store.SetServerImagePins(srv.ID, pins); err != nil {
		log.Printf("server %s: image pins: %v", srv.Slug, err)
		return
	}
	srv.ImagePins = pins
}
