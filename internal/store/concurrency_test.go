package store

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"

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
