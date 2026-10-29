package reconciler

import (
	"encoding/json"
	"fmt"
	"hash/maphash"
	"strconv"
	"sync"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// memoTTL is how long a write is taken as still holding. Past it, the next pass
// sends the object again, which undoes a change made behind Quetzal's back.
const memoTTL = 5 * time.Minute

// memoAbsent is what an object found deleted is remembered as; no content hash
// is ever equal to it.
const memoAbsent = "absent"

// Memo is what a control plane remembers about one cluster from one pass to the
// next. The Reconciler is made anew on every resync, and every pass applied
// each object of each server again, deleted the objects a server does not use
// and tried to create a RoleBinding that was already there: some two hundred
// writes a minute for five servers that had not changed, nine requests in ten
// of the API server's audit log. With a Memo, an object applied with the same
// content, or found absent, less than memoTTL ago is left alone.
//
// Until then, an object edited or deleted by hand stays so -- except the ones
// that fence a server in, its NetworkPolicies and ResourceQuota: those are read
// back on every pass, and sent again as soon as their resourceVersion is not
// the one Quetzal's write left. A namespace made again under the same name has
// a new UID, and starts with nothing remembered.
//
// A nil Memo remembers nothing: every pass writes everything, as before.
type Memo struct {
	mu         sync.Mutex
	now        func() time.Time
	seed       maphash.Seed
	entries    map[string]memoEntry
	namespaces map[string]types.UID
	warned     map[string]bool
	swept      time.Time
}

type memoEntry struct {
	namespace string
	sum       string
	// version is the resourceVersion the write left, for an object read back.
	version string
	at      time.Time
}

// NewMemo returns an empty Memo.
func NewMemo() *Memo {
	return &Memo{
		now:        time.Now,
		seed:       maphash.MakeSeed(),
		entries:    map[string]memoEntry{},
		namespaces: map[string]types.UID{},
		warned:     map[string]bool{},
	}
}

// memoKey names an object: its Go type, namespace and name.
func memoKey(obj client.Object) string {
	return fmt.Sprintf("%T|%s|%s", obj, obj.GetNamespace(), obj.GetName())
}

// sum is a hash of everything about obj Quetzal would send, or "" when there is
// nothing to remember it in. Secrets go through it, passwords included, so it
// is not a digest of them: the hash is keyed with a seed drawn when the Memo is
// made, never leaves the process, and means nothing to another one. It only
// has to tell one version of an object from the next.
func (m *Memo) sum(obj client.Object) (string, error) {
	if m == nil {
		return "", nil
	}
	b, err := json.Marshal(obj)
	if err != nil {
		return "", err
	}
	return strconv.FormatUint(maphash.Bytes(m.seed, b), 16), nil
}

// fresh reports whether key was recorded with sum less than memoTTL ago, and
// the resourceVersion that write left.
func (m *Memo) fresh(key, sum string) (bool, string) {
	if m == nil {
		return false, ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.entries[key]
	if !ok || e.sum != sum || m.now().Sub(e.at) >= memoTTL {
		return false, ""
	}
	return true, e.version
}

// record remembers that key now holds sum, at resourceVersion version when the
// object is read back ("" otherwise).
func (m *Memo) record(namespace, key, sum, version string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	m.entries[key] = memoEntry{namespace: namespace, sum: sum, version: version, at: now}
	// Entries past their TTL are worth nothing; dropping them keeps the
	// servers deleted since from being remembered for good.
	if now.Sub(m.swept) >= memoTTL {
		for k, e := range m.entries {
			if now.Sub(e.at) >= memoTTL {
				delete(m.entries, k)
			}
		}
		m.swept = now
	}
}

// sawNamespace forgets everything remembered in namespace name when it is not
// the namespace it was: deleted and made again, it holds none of it.
func (m *Memo) sawNamespace(name string, uid types.UID) {
	if m == nil || uid == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.namespaces[name] == uid {
		return
	}
	for k, e := range m.entries {
		if e.namespace == name {
			delete(m.entries, k)
		}
	}
	m.namespaces[name] = uid
}

// warnOnce reports whether key is seen for the first time.
func (m *Memo) warnOnce(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.warned[key] {
		return false
	}
	m.warned[key] = true
	return true
}
