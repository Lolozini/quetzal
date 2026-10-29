package reconciler

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"testing"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/lolozini/quetzal/internal/models"
)

// writeLog counts the writes that reach the API server, by verb and type.
type writeLog struct {
	mu sync.Mutex
	n  map[string]int
}

func (w *writeLog) add(verb string, obj client.Object) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.n[fmt.Sprintf("%s %T", verb, obj)]++
}

// take returns what was counted since the last call, sorted, and starts over.
func (w *writeLog) take() []string {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []string
	for k, v := range w.n {
		out = append(out, fmt.Sprintf("%s ×%d", k, v))
	}
	sort.Strings(out)
	w.n = map[string]int{}
	return out
}

func (w *writeLog) funcs() interceptor.Funcs {
	return interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			w.add("create", obj)
			if r, ok := obj.(*authnv1.SelfSubjectReview); ok {
				r.Status.UserInfo = authnv1.UserInfo{Username: "system:serviceaccount:quetzal:quetzal"}
				return nil
			}
			return c.Create(ctx, obj, opts...)
		},
		Update: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.UpdateOption) error {
			w.add("update", obj)
			return c.Update(ctx, obj, opts...)
		},
		Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
			w.add("patch", obj)
			return c.Patch(ctx, obj, patch, opts...)
		},
		Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
			w.add("delete", obj)
			return c.Delete(ctx, obj, opts...)
		},
	}
}

// Every resync used to write every object of every server again: a patch per
// object, a delete for each object a mode the server does not use would need,
// a RoleBinding created for a 409. Five servers that had not changed made some
// two hundred writes a minute. A pass over an unchanged server now writes
// nothing; a change is still written on the next pass, and past memoTTL
// everything is sent again, which undoes what was changed by hand.
func TestUnchangedServerIsNotWrittenAgain(t *testing.T) {
	st := reconStore(t)
	srv := publishedServer(t, st)
	w := &writeLog{n: map[string]int{}}
	cl := fakeCluster(w.funcs())
	memo := NewMemo()
	clock := time.Now()
	memo.now = func() time.Time { return clock }
	pass := func() []string {
		t.Helper()
		r := newTestReconciler(cl, st)
		r.Memo = memo
		r.NamespacedRole = "quetzal-namespaced"
		r.accessWait = time.Millisecond
		if err := r.ReconcileServer(context.Background(), srv.ID); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		return w.take()
	}

	if first := pass(); len(first) == 0 {
		t.Fatal("the first pass wrote nothing; the counting is broken")
	}
	if again := pass(); len(again) != 0 {
		t.Errorf("a pass over an unchanged server wrote %v", again)
	}

	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if changed := pass(); !hasWrite(changed, "patch *v1.Deployment") {
		t.Errorf("stopping the server did not patch its Deployment: %v", changed)
	}

	clock = clock.Add(memoTTL)
	if expired := pass(); !hasWrite(expired, "patch *v1.NetworkPolicy") || !hasWrite(expired, "delete *v1.Service") {
		t.Errorf("past memoTTL the objects were not sent again: %v", expired)
	}
}

func hasWrite(lines []string, prefix string) bool {
	for _, l := range lines {
		if len(l) >= len(prefix) && l[:len(prefix)] == prefix {
			return true
		}
	}
	return false
}

// A namespace deleted and made again under the same name holds none of what
// was written to the one before it.
func TestMemoForgetsANamespaceMadeAgain(t *testing.T) {
	m := NewMemo()
	m.sawNamespace("quetzal-srv-a", "uid-1")
	m.record("quetzal-srv-a", "k", "sum")
	m.record("quetzal-srv-b", "other", "sum")
	m.sawNamespace("quetzal-srv-a", "uid-1")
	if !m.fresh("k", "sum") {
		t.Fatal("the same namespace seen again lost what was written to it")
	}
	m.sawNamespace("quetzal-srv-a", "uid-2")
	if m.fresh("k", "sum") {
		t.Error("a namespace made again still counts as holding what the old one held")
	}
	if !m.fresh("other", "sum") {
		t.Error("another namespace was forgotten with it")
	}
}

// A nil Memo, which tests and older callers have, remembers nothing.
func TestNilMemoRemembersNothing(t *testing.T) {
	var m *Memo
	m.record("ns", "k", "sum")
	m.sawNamespace("ns", "uid")
	if m.fresh("k", "sum") {
		t.Error("a nil Memo said an object was written")
	}
}
