// Package console proxies a server's live console over a WebSocket: stdout via
// the pod log stream, stdin via the Kubernetes attach subresource. No sidecar
// and no RCON server are required — this is the generic, game-agnostic console.
//
// The stream is resilient: it waits for the container to actually be running
// before attaching (a pod that is still Pending has no host and no container to
// attach to) and re-attaches across restarts, so a freshly-created or
// crash-looping server shows logs instead of a one-shot error.
package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/lolozini/quetzal/internal/reconciler"
)

const (
	// pongWait is how long we wait for a pong before treating the connection as
	// dead; pingPeriod must be shorter so we ping in time.
	pongWait   = 60 * time.Second
	pingPeriod = (pongWait * 9) / 10
	// pollInterval is how often we re-check for a running container while waiting.
	pollInterval = 1500 * time.Millisecond
)

// Message is a console frame exchanged with the browser.
type Message struct {
	// Type is "stdout" (server->client), "stdin" (client->server),
	// "status", or "error".
	Type string `json:"type"`
	Data string `json:"data"`
}

// FindRunningPod returns the name of a running pod for the given server slug,
// falling back to any non-terminating pod. Used by best-effort callers (graceful
// stop, stats); the interactive console uses runningContainerPod instead.
func FindRunningPod(ctx context.Context, cs kubernetes.Interface, ns, slug string) (string, error) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: reconciler.ServerLabel + "=" + slug,
	})
	if err != nil {
		return "", err
	}
	var fallback string
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase == corev1.PodRunning {
			return p.Name, nil
		}
		if fallback == "" {
			fallback = p.Name
		}
	}
	if fallback != "" {
		return fallback, nil
	}
	return "", fmt.Errorf("no pod found for server %q (is it running?)", slug)
}

// runningContainerPod returns a pod whose main container is actually in the
// Running state (so it can be attached to / have its logs streamed), or false.
func runningContainerPod(ctx context.Context, cs kubernetes.Interface, ns, slug string) (string, bool) {
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: reconciler.ServerLabel + "=" + slug,
	})
	if err != nil {
		return "", false
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.DeletionTimestamp != nil {
			continue
		}
		for _, cs := range p.Status.ContainerStatuses {
			if cs.Name == reconciler.WorkloadName && cs.State.Running != nil {
				return p.Name, true
			}
		}
	}
	return "", false
}

// waitForRunningPod blocks until the server's main container is running,
// returning its pod name, or ctx.Err() if the context is cancelled first.
func waitForRunningPod(ctx context.Context, cs kubernetes.Interface, ns, slug string) (string, error) {
	for {
		if pod, ok := runningContainerPod(ctx, cs, ns, slug); ok {
			return pod, nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(pollInterval):
		}
	}
}

// Stream wires a WebSocket to a server's console until the client disconnects.
// It streams logs (stdout) and an attach session (stdin) concurrently, each in
// a loop that waits for a running container and reconnects across restarts.
func Stream(ctx context.Context, ws *websocket.Conn, cs kubernetes.Interface, cfg *rest.Config, ns, slug string) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Single writer goroutine: gorilla forbids concurrent writes.
	out := make(chan Message, 128)
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		for {
			select {
			case <-ctx.Done():
				return
			case m, ok := <-out:
				if !ok {
					return
				}
				if err := ws.WriteJSON(m); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	send(ctx, out, Message{Type: "status", Data: "connecting to " + slug + "…"})

	// stdout: follow logs, reconnecting when the container (re)starts.
	go func() {
		var last string
		for ctx.Err() == nil {
			pod, err := waitForRunningPod(ctx, cs, ns, slug)
			if err != nil {
				return
			}
			if pod != last {
				send(ctx, out, Message{Type: "status", Data: "streaming logs from " + pod})
				last = pod
			}
			if err := streamLogs(ctx, cs, ns, pod, out); err != nil && ctx.Err() == nil {
				send(ctx, out, Message{Type: "status", Data: "log stream ended; waiting for the server…"})
			}
			sleep(ctx, time.Second)
		}
	}()

	// stdin: a long-lived pipe fed by the WS reader; a pump owns the write end so
	// the reader never blocks, and the attach loop re-reads it across restarts.
	stdin := make(chan string, 64)
	pr, pw := io.Pipe()
	go func() {
		for {
			select {
			case <-ctx.Done():
				_ = pw.Close()
				return
			case line := <-stdin:
				if _, err := io.WriteString(pw, line+"\n"); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		for ctx.Err() == nil {
			pod, err := waitForRunningPod(ctx, cs, ns, slug)
			if err != nil {
				return
			}
			// Errors here are usually transient (container restarting); the loop
			// waits for the next running container and re-attaches.
			_ = attachStdin(ctx, cs, cfg, ns, pod, pr)
			sleep(ctx, time.Second)
		}
	}()

	// Keepalive: drop dead/half-open connections instead of leaking goroutines.
	ws.SetReadDeadline(time.Now().Add(pongWait))
	ws.SetPongHandler(func(string) error {
		ws.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go func() {
		t := time.NewTicker(pingPeriod)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// WriteControl is safe to call concurrently with the writer goroutine.
				if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					cancel()
					return
				}
			}
		}
	}()

	// Reader loop: WS -> pod stdin (non-blocking; drop if the pump is backed up
	// because no container is currently attachable).
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		ws.SetReadDeadline(time.Now().Add(pongWait))
		var m Message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.Type == "stdin" {
			select {
			case stdin <- m.Data:
			default:
			}
		}
	}
	cancel()
	<-writerDone
	return nil
}

func streamLogs(ctx context.Context, cs kubernetes.Interface, ns, pod string, out chan<- Message) error {
	tail := int64(200)
	req := cs.CoreV1().Pods(ns).GetLogs(pod, &corev1.PodLogOptions{
		Container: reconciler.WorkloadName,
		Follow:    true,
		TailLines: &tail,
	})
	stream, err := req.Stream(ctx)
	if err != nil {
		return err
	}
	defer stream.Close()

	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			send(ctx, out, Message{Type: "stdout", Data: string(buf[:n])})
		}
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				return nil
			}
			return err
		}
	}
}

func attachStdin(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod string, stdin io.Reader) error {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").Name(pod).Namespace(ns).SubResource("attach").
		VersionedParams(&corev1.PodAttachOptions{
			Container: reconciler.WorkloadName,
			Stdin:     true,
			Stdout:    false,
			Stderr:    false,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(cfg, "POST", req.URL())
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{Stdin: stdin})
}

// SendStdin attaches to a pod and writes a single payload to its stdin, then
// returns. Used for graceful stop (sending a template's stop command). Pass a
// context with a timeout.
func SendStdin(ctx context.Context, cs kubernetes.Interface, cfg *rest.Config, ns, pod, data string) error {
	return attachStdin(ctx, cs, cfg, ns, pod, strings.NewReader(data))
}

func send(ctx context.Context, out chan<- Message, m Message) {
	select {
	case out <- m:
	case <-ctx.Done():
	}
}

// sleep waits for d or until ctx is cancelled.
func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
