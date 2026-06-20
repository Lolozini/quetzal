// Package console proxies a server's live console over a WebSocket: stdout via
// the pod log stream, stdin via the Kubernetes attach subresource. No sidecar
// and no RCON server are required — this is the generic, game-agnostic console.
package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/gorilla/websocket"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/lolozini/quetzal/internal/reconciler"
)

// Message is a console frame exchanged with the browser.
type Message struct {
	// Type is "stdout" (server->client), "stdin" (client->server),
	// "status", or "error".
	Type string `json:"type"`
	Data string `json:"data"`
}

// FindRunningPod returns the name of a running pod for the given server slug,
// or an error if none is ready.
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

// Stream wires a WebSocket to a pod's console until either side closes. It runs
// log streaming (stdout) and an attach session (stdin) concurrently.
func Stream(ctx context.Context, ws *websocket.Conn, cs kubernetes.Interface, cfg *rest.Config, ns, pod string) error {
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

	// stdout: follow pod logs.
	go func() {
		if err := streamLogs(ctx, cs, ns, pod, out); err != nil && ctx.Err() == nil {
			send(ctx, out, Message{Type: "error", Data: "log stream ended: " + err.Error()})
		}
	}()

	// stdin: attach to the container, fed by a pipe written from the WS reader.
	pr, pw := io.Pipe()
	go func() {
		if err := attachStdin(ctx, cs, cfg, ns, pod, pr); err != nil && ctx.Err() == nil {
			send(ctx, out, Message{Type: "error", Data: "stdin attach ended: " + err.Error()})
		}
	}()

	send(ctx, out, Message{Type: "status", Data: "connected to " + pod})

	// Reader loop: WS -> pod stdin.
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			break
		}
		var m Message
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		if m.Type == "stdin" {
			if _, err := io.WriteString(pw, m.Data+"\n"); err != nil {
				break
			}
		}
	}
	cancel()
	_ = pw.Close()
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

func send(ctx context.Context, out chan<- Message, m Message) {
	select {
	case out <- m:
	case <-ctx.Done():
	}
}
