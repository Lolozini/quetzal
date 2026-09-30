package store

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
)

// Writes racing on the chart's SQLite database (WAL, a 5 s busy timeout) failed
// at once with "database is locked (5) (SQLITE_BUSY)": 7 of 15 servers created
// in parallel through the API. A transaction that reads before it writes, as
// allocating a node port does, cannot wait for the lock when another write
// lands in between, so the busy timeout never applied. Transactions now take
// the write lock when they begin, where it does.
func TestConcurrentWritesWaitForTheLock(t *testing.T) {
	for _, dsn := range []string{
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", // the chart's
		"", // a bare path, as a hand-written install might have
	} {
		t.Run(fmt.Sprintf("dsn%q", dsn), func(t *testing.T) {
			st, err := Open(Config{Driver: DriverSQLite, DSN: filepath.Join(t.TempDir(), "q.db") + dsn, Silent: true})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			if err := st.Migrate(); err != nil {
				t.Fatalf("migrate: %v", err)
			}
			const n = 30
			var wg sync.WaitGroup
			errs := make(chan error, n)
			ports := make(chan int32, n)
			for i := 0; i < n; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					srv := &models.Server{Slug: fmt.Sprintf("s%d", i), Namespace: fmt.Sprintf("quetzal-srv-s%d", i)}
					if err := st.CreateServer(srv); err != nil {
						errs <- err
						return
					}
					p, err := st.AllocateNodePort(srv.ID, "p25565", 30000, 32767)
					if err != nil {
						errs <- err
						return
					}
					ports <- p
				}(i)
			}
			wg.Wait()
			close(errs)
			close(ports)
			for err := range errs {
				t.Errorf("concurrent write: %v", err)
			}
			seen := map[int32]bool{}
			for p := range ports {
				if seen[p] {
					t.Errorf("node port %d handed out twice", p)
				}
				seen[p] = true
			}
		})
	}
}

// A quota was checked, then the server inserted, and a request arriving in
// between saw the same room: five creations sent at once by an account allowed
// one server made two. The check here takes its time, as a busy panel's could,
// and exactly one creation may still get through.
func TestQuotaCheckAndInsertAreOne(t *testing.T) {
	st := newTestStore(t)
	owner := &models.User{Username: "quinn", MaxServers: 1}
	if err := st.CreateUser(owner); err != nil {
		t.Fatalf("user: %v", err)
	}
	full := errors.New("quota exceeded")
	const n = 5
	var wg sync.WaitGroup
	results := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			srv := &models.Server{Slug: fmt.Sprintf("q%d", i), Namespace: fmt.Sprintf("quetzal-srv-q%d", i), OwnerID: owner.ID}
			results <- st.CreateServerChecked(srv, func(owned []models.Server) error {
				time.Sleep(50 * time.Millisecond)
				if len(owned) >= owner.MaxServers {
					return full
				}
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(results)
	created, refused := 0, 0
	for err := range results {
		switch {
		case err == nil:
			created++
		case errors.Is(err, full):
			refused++
		default:
			t.Errorf("create: %v", err)
		}
	}
	if created != 1 || refused != n-1 {
		t.Errorf("created %d, refused %d; want 1 and %d", created, refused, n-1)
	}
}
