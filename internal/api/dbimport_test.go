package api_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/lolozini/quetzal/internal/models"
	"github.com/lolozini/quetzal/internal/reconciler"
	"github.com/lolozini/quetzal/internal/store"
)

// dbServer stores a stopped server owned by alice, holding one database on an
// external host, with mallory and dave as its subusers: mallory may handle its
// databases but not its files, dave both.
func dbServer(t *testing.T) (url string, admin, alice, mallory, dave *http.Client, st *store.Store, srv *models.Server, db *models.ServerDatabase) {
	t.Helper()
	ts, admin, st := newTestServerStore(t)
	setupAdmin(t, ts.URL, admin)
	for _, u := range []string{"alice", "mallory", "dave"} {
		createUser(t, admin, ts.URL, map[string]any{"username": u, "password": u + "pw12345"})
	}
	alice = loginAs(t, ts.URL, "alice", "alicepw12345")
	mallory = loginAs(t, ts.URL, "mallory", "mallorypw12345")
	dave = loginAs(t, ts.URL, "dave", "davepw12345")
	owner, err := st.GetUserByUsername("alice")
	if err != nil {
		t.Fatal(err)
	}
	gen, err := st.GetTemplateBySlug("generic-process")
	if err != nil {
		t.Fatal(err)
	}
	srv = &models.Server{Slug: "ts-a1b2", DisplayName: "ts", Namespace: reconciler.NamespaceFor("ts-a1b2"),
		OwnerID: owner.ID, TemplateID: gen.ID, Image: "alpine:3.20", DesiredState: models.StateStopped}
	if err := st.CreateServer(srv); err != nil {
		t.Fatal(err)
	}
	host := &models.DatabaseHost{Name: "lan", Kind: models.DBHostExternal, Host: "db.lan", Port: 3306, AdminUser: "root"}
	if err := st.CreateDatabaseHost(host, "rootpw"); err != nil {
		t.Fatal(err)
	}
	db = &models.ServerDatabase{ServerID: srv.ID, HostID: host.ID, DatabaseName: "s1_aaaa", Username: "u1_aaaa", Remote: "%"}
	if err := st.CreateServerDatabase(db, "pw"); err != nil {
		t.Fatal(err)
	}
	url = ts.URL + "/api/servers/" + itoa(srv.ID)
	for name, perms := range map[string][]string{"mallory": {"view", "databases", "backups"}, "dave": {"view", "databases", "files", "power"}} {
		if r := post(t, alice, url+"/access", map[string]any{"username": name, "permissions": perms}); r.StatusCode != http.StatusNoContent {
			t.Fatalf("grant %s = %d", name, r.StatusCode)
		}
	}
	return url, admin, alice, mallory, dave, st, srv, db
}

func errorOf(r *http.Response) string {
	defer r.Body.Close()
	var body struct{ Error string }
	_ = json.NewDecoder(r.Body).Decode(&body)
	return body.Error
}

// An SQL file of the server's is loaded into one of its databases on request:
// by someone trusted with both its databases and its files -- the file's
// content ends up readable in the database -- on a stopped server, which then
// cannot start until the import is done.
func TestAnImportLoadsAFileOfTheServersIntoItsDatabase(t *testing.T) {
	url, _, alice, mallory, dave, st, srv, db := dbServer(t)
	importURL := url + "/databases/" + itoa(db.ID) + "/import"

	if r := post(t, mallory, importURL, map[string]any{"path": "/ts3.sql"}); r.StatusCode != http.StatusForbidden {
		t.Errorf("an import by a subuser without the files permission = %d, want 403", r.StatusCode)
	}
	for _, path := range []string{"", "../../etc/passwd", "/", "a/../../x"} {
		if r := post(t, dave, importURL, map[string]any{"path": path}); r.StatusCode != http.StatusBadRequest {
			t.Errorf("an import of %q = %d, want 400", path, r.StatusCode)
		}
	}
	if err := st.SetDesiredState(srv.ID, models.StateRunning); err != nil {
		t.Fatal(err)
	}
	if r := post(t, dave, importURL, map[string]any{"path": "/ts3.sql"}); r.StatusCode != http.StatusConflict {
		t.Errorf("an import of a running server = %d, want 409", r.StatusCode)
	}
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	r := post(t, dave, importURL, map[string]any{"path": "dumps/./ts3.sql"})
	var queued models.Backup
	json.NewDecoder(r.Body).Decode(&queued)
	if r.StatusCode != http.StatusAccepted || queued.Path != "/dumps/ts3.sql" || !queued.Wipe || queued.Phase != models.BackupPending {
		t.Fatalf("import = %d %+v, want 202 queued for /dumps/ts3.sql, emptying the database first", r.StatusCode, queued)
	}
	if r := post(t, dave, importURL, map[string]any{"path": "/other.sql"}); r.StatusCode != http.StatusConflict {
		t.Errorf("a second import = %d, want 409", r.StatusCode)
	}
	if r := post(t, dave, url+"/power", map[string]string{"action": "start"}); r.StatusCode != http.StatusConflict ||
		!strings.Contains(errorOf(r), "database import") {
		t.Errorf("a start under the import = %d, want 409 saying why", r.StatusCode)
	}
	if r := doMethod(t, alice, http.MethodDelete, url+"/databases/"+itoa(db.ID), nil); r.StatusCode != http.StatusConflict {
		t.Errorf("deleting the database under its import = %d, want 409", r.StatusCode)
	}

	// The databases list says how the last import went.
	var dbs []struct {
		ID         uint
		LastImport *struct {
			Phase string
			Path  string
			Wipe  bool
		} `json:"lastImport"`
	}
	getJSON(t, dave, url+"/databases", &dbs)
	if len(dbs) != 1 || dbs[0].LastImport == nil || dbs[0].LastImport.Phase != "Pending" || dbs[0].LastImport.Path != "/dumps/ts3.sql" {
		t.Errorf("databases = %+v", dbs)
	}
	// It is no backup of the server's.
	var backups []models.Backup
	getJSON(t, alice, url+"/backups", &backups)
	if len(backups) != 0 {
		t.Errorf("the backups list shows the import: %+v", backups)
	}

	// Called off before it starts, it lets the server start.
	if r := doMethod(t, dave, http.MethodDelete, importURL, nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel = %d", r.StatusCode)
	}
	if r := doMethod(t, dave, http.MethodDelete, importURL, nil); r.StatusCode != http.StatusNotFound {
		t.Errorf("cancelling again = %d, want 404", r.StatusCode)
	}
	if r := post(t, dave, url+"/power", map[string]string{"action": "start"}); r.StatusCode != http.StatusNoContent && r.StatusCode != http.StatusOK && r.StatusCode != http.StatusAccepted {
		t.Errorf("a start once the import is cancelled = %d", r.StatusCode)
	}

	// Without emptying the database, when asked.
	if err := st.SetDesiredState(srv.ID, models.StateStopped); err != nil {
		t.Fatal(err)
	}
	r = post(t, alice, importURL, map[string]any{"path": "/extra.sql.gz", "wipe": false})
	var keep models.Backup
	json.NewDecoder(r.Body).Decode(&keep)
	if r.StatusCode != http.StatusAccepted || keep.Wipe || keep.Path != "/extra.sql.gz" {
		t.Errorf("an import that keeps the database = %d %+v", r.StatusCode, keep)
	}
}

// A restore loads the backup's databases back when asked, and only for
// someone who may handle the server's databases: the backups permission alone
// restores the files.
func TestARestoreLoadsTheDatabasesWhenAsked(t *testing.T) {
	url, _, alice, mallory, _, st, srv, _ := dbServer(t)
	src := &models.Backup{ServerID: srv.ID, Direction: models.DirBackup, Phase: models.BackupPending}
	if err := st.CreateBackup(src); err != nil {
		t.Fatal(err)
	}
	if ok, err := st.ClaimBackup(src.ID, "quetzal-backup-x", "", []string{"s1_aaaa"}); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	src.Phase = models.BackupSucceeded
	if err := st.UpdateBackup(src); err != nil {
		t.Fatal(err)
	}
	restoreURL := url + "/backups/" + itoa(src.ID) + "/restore"

	// mallory holds the backups and databases permissions; strip databases.
	if r := post(t, alice, url+"/access", map[string]any{"username": "mallory", "permissions": []string{"view", "backups"}}); r.StatusCode != http.StatusNoContent {
		t.Fatalf("regrant = %d", r.StatusCode)
	}
	if r := post(t, mallory, restoreURL, map[string]any{"databases": true}); r.StatusCode != http.StatusForbidden {
		t.Errorf("restoring the databases without the databases permission = %d, want 403", r.StatusCode)
	}
	if r := post(t, mallory, restoreURL, []byte("{not json")); r.StatusCode != http.StatusBadRequest {
		t.Errorf("an unreadable body = %d, want 400", r.StatusCode)
	}
	r := post(t, alice, restoreURL, map[string]any{"databases": true})
	var queued models.Backup
	json.NewDecoder(r.Body).Decode(&queued)
	if r.StatusCode != http.StatusAccepted || !queued.WithDatabases {
		t.Fatalf("restore with the databases = %d %+v", r.StatusCode, queued)
	}
	// Called off; a bare restore is of the files alone, as it always was.
	if r := doMethod(t, alice, http.MethodDelete, url+"/backups/"+itoa(queued.ID), nil); r.StatusCode != http.StatusNoContent {
		t.Fatalf("cancel = %d", r.StatusCode)
	}
	r = post(t, mallory, restoreURL, nil)
	var bare models.Backup
	json.NewDecoder(r.Body).Decode(&bare)
	if r.StatusCode != http.StatusAccepted || bare.WithDatabases || bare.SourceID != src.ID {
		t.Errorf("a bare restore = %d %+v, want the files alone", r.StatusCode, bare)
	}
}
