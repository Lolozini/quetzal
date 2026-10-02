package reconciler

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/store"
)

// maxNodePortMoves bounds how many times one pass draws another node port for
// a Service the cluster keeps refusing: a range crowded with other Services
// is better reported than walked through.
const maxNodePortMoves = 5

// nodePortField is the field the apiserver names when it refuses a node port.
var nodePortField = regexp.MustCompile(`^spec\.ports\[(\d+)\]\.nodePort$`)

// takenNodePorts reads, from the apiserver's refusal of svc, the node ports it
// has already given to another Service ("spec.ports[0].nodePort: Invalid
// value: 30100: provided port is already allocated").
func takenNodePorts(err error, svc *corev1.Service) []int32 {
	if err == nil || !apierrors.IsInvalid(err) {
		return nil
	}
	var status apierrors.APIStatus
	if !errors.As(err, &status) || status.Status().Details == nil {
		return nil
	}
	var out []int32
	seen := map[int32]bool{}
	for _, c := range status.Status().Details.Causes {
		m := nodePortField.FindStringSubmatch(c.Field)
		if m == nil || !strings.Contains(c.Message, "already allocated") {
			continue
		}
		i, err := strconv.Atoi(m[1])
		if err != nil || i >= len(svc.Spec.Ports) {
			continue
		}
		if np := svc.Spec.Ports[i].NodePort; np != 0 && !seen[np] {
			seen[np] = true
			out = append(out, np)
		}
	}
	return out
}

// applyNodePortService applies a Service whose node ports come from the pool.
// The pool only knows Quetzal's own allocations, and the range it draws from
// is by default the cluster's whole range, which Traefik's LoadBalancer
// Service or any other NodePort Service draws from too: the cluster then
// refuses the Service, on every pass, for a port it already gave away. move
// sets each such port aside and gives the Service another, and build makes the
// Service again with it.
func (r *Reconciler) applyNodePortService(ctx context.Context, build func() *corev1.Service, move func(taken int32) error) error {
	for moves := 0; ; moves++ {
		svc := build()
		err := r.apply(ctx, svc)
		taken := takenNodePorts(err, svc)
		if len(taken) == 0 || moves == maxNodePortMoves {
			return err
		}
		for _, p := range taken {
			if mErr := move(p); mErr != nil {
				return fmt.Errorf("%w (and no other node port could be drawn: %v)", err, mErr)
			}
		}
	}
}

// moveGamePort gives the server's ports published on node port taken another
// node port, and says so: the address players were given, on the port the
// cluster refused, never reached this server.
func (r *Reconciler) moveGamePort(s *models.Server, taken int32) error {
	var number int32
	for _, p := range s.Ports {
		if p.NodePort == taken {
			number = p.Port
			break
		}
	}
	if number == 0 {
		return fmt.Errorf("no port of the server is on node port %d", taken)
	}
	np, err := r.Store.SetAsideNodePort(s.ID, store.NodePortKey(number), taken, r.NodePortMin, r.NodePortMax)
	if err != nil {
		return err
	}
	if err := r.Store.RepointServerNodePort(s.ID, taken, np); err != nil {
		return err
	}
	for i := range s.Ports {
		if s.Ports[i].NodePort == taken {
			s.Ports[i].NodePort = np
		}
	}
	log.Printf("server %s: node port %d belongs to a Service outside Quetzal; port %d moves to node port %d", s.Slug, taken, number, np)
	r.emitEvent(s, models.EventServerPortMoved, fmt.Sprintf(
		"port %d is published on node port %d: the cluster had already given node port %d to another Service", number, np, taken))
	return nil
}

// moveSFTPPort does the same for the SFTP Service, whose node port is read
// from the pool on every pass.
func (r *Reconciler) moveSFTPPort(s *models.Server, taken int32) (int32, error) {
	np, err := r.Store.SetAsideNodePort(s.ID, SFTPPortName, taken, r.NodePortMin, r.NodePortMax)
	if err != nil {
		return 0, err
	}
	log.Printf("server %s: node port %d belongs to a Service outside Quetzal; SFTP moves to node port %d", s.Slug, taken, np)
	r.emitEvent(s, models.EventServerPortMoved, fmt.Sprintf(
		"SFTP is published on node port %d: the cluster had already given node port %d to another Service", np, taken))
	return np, nil
}
