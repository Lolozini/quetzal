package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/testdb"
)

// Writes racing on the chart's SQLite database (WAL, a 5 s busy timeout) failed
// at once with "database is locked (5) (SQLITE_BUSY)": 7 of 15 servers created
// in parallel through the API. A transaction that reads before it writes, as
// allocating a node port does, cannot wait for the lock when another write
// lands in between, so the busy timeout never applied. Transactions now take
// the write lock when they begin, where it does.
func TestConcurrentWritesWaitForTheLock(t *testing.T) {
	dsns := []string{
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", // the chart's
		"", // a bare path, as a hand-written install might have
	}
	if testdb.Postgres() {
		// The pragmas are SQLite's; the race is the same on PostgreSQL.
		dsns = []string{""}
	}
	for _, dsn := range dsns {
		t.Run(fmt.Sprintf("dsn%q", dsn), func(t *testing.T) {
			st, err := Open(Config{Driver: Driver(testdb.Driver()), DSN: testdb.DSN(t, "q.db") + dsn, Silent: true})
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

// A start and a restore asked for at the same moment: one of them wins, never
// both, or the restore would wait under a running server and roll its world
// back later (R-11 of the 0.10.0 test pass). On SQLite one write transaction
// runs at a time; on PostgreSQL the server's row lock is what keeps them apart,
// which only this test, run there, exercises.
func TestAStartAndARestoreNeverBothWin(t *testing.T) {
	st := newTestStore(t)
	for round := 0; round < 40; round++ {
		srv := &models.Server{Slug: fmt.Sprintf("r%d", round), Namespace: fmt.Sprintf("quetzal-srv-r%d", round),
			DesiredState: models.StateStopped}
		if err := st.CreateServer(srv); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		ready := make(chan struct{})
		var startErr, restoreErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-ready
			startErr = st.StartServer(srv.ID, time.Now())
		}()
		go func() {
			defer wg.Done()
			<-ready
			restoreErr = st.CreateRestore(&models.Backup{ServerID: srv.ID, Direction: models.DirRestore, Phase: models.BackupPending})
		}()
		close(ready)
		wg.Wait()
		if startErr == nil && restoreErr == nil {
			t.Fatalf("round %d: the start and the restore both went through", round)
		}
		for _, err := range []error{startErr, restoreErr} {
			if err != nil && !errors.Is(err, ErrRestoreActive) && !errors.Is(err, ErrServerRunning) {
				t.Fatalf("round %d: %v", round, err)
			}
		}
	}
}
