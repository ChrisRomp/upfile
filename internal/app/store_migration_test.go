package app

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttemptCommentsSurviveRestartAndV1Migration(t *testing.T) {
	for _, version := range []string{"v1", "v2"} {
		t.Run(version, func(t *testing.T) {
			config := Config{DataDir: t.TempDir(), Origin: "https://drop.test", HeadroomBytes: 1}
			a, err := New(config, testVerifier{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			h := &harness{a: a, t: t, admin: a.Admin(), public: a.Public()}
			t.Cleanup(func() {
				if h.a != nil {
					if err := h.a.Close(); err != nil {
						t.Error(err)
					}
				}
			})
			restart := func() {
				t.Helper()
				err := h.a.Close()
				h.a = nil
				if err != nil {
					t.Fatal(err)
				}
				a, err := New(config, testVerifier{}, nil)
				if err != nil {
					t.Fatal(err)
				}
				h.a, h.admin, h.public = a, a.Admin(), a.Public()
			}
			h.call(true, "PUT", "/api/settings", map[string]any{
				"max_file_bytes": 1024, "storage_budget_bytes": 4096, "default_link_hours": 168,
			}, nil, 200)
			_, l, cookie := h.link(nil)
			completed := h.admit(l, cookie, "persistent-completed-comment", 3, 201)
			h.patch(l, cookie, completed, 0, "abc", 204)
			input := map[string]any{"key": "persistent-empty-admission", "name": "other.txt", "comment": "", "size": 6}
			uploading := decode[Attempt](t, h.call(false, "POST", "/api/links/"+l.ID+"/attempts", input, cookie, 201))
			h.patch(l, cookie, uploading, 0, "abc", 204)
			completedComment, uploadingComment := "A note", ""
			if version == "v1" {
				mutationExec(t, h, "ALTER TABLE attempts DROP COLUMN admission_comment; PRAGMA user_version=1;")
			} else {
				completedComment, uploadingComment = "Edited completed comment", "Edited pending comment"
				editComment(t, h, l, cookie, completed, completedComment)
				editComment(t, h, l, cookie, uploading, uploadingComment)
			}
			restart()
			if mutationCount(t, h, "PRAGMA user_version") != 2 {
				t.Fatal("database was not upgraded to version 2")
			}
			assertComment(t, h, completed.ID, completedComment, "A note")
			assertComment(t, h, uploading.ID, uploadingComment, "")
			for _, comment := range []string{"After restart\n🙂", ""} {
				editComment(t, h, l, cookie, completed, comment)
				editComment(t, h, l, cookie, uploading, comment)
				if retry := h.admit(l, cookie, "persistent-completed-comment", 3, 200); retry.ID != completed.ID {
					t.Fatal("restart or migration broke completed admission retry")
				}
				retry := decode[Attempt](t, h.call(false, "POST", "/api/links/"+l.ID+"/attempts", input, cookie, 200))
				if retry.ID != uploading.ID || retry.Offset != 3 {
					t.Fatal("restart or migration broke active admission retry")
				}
			}
			h.patch(l, cookie, uploading, 3, "def", 204)
			editComment(t, h, l, cookie, completed, "Persisted final edit")
			restart()
			assertComment(t, h, completed.ID, "Persisted final edit", "A note")
			assertComment(t, h, uploading.ID, "", "")
			h.admit(l, cookie, "persistent-completed-comment", 3, 200)
			if mutationCount(t, h, "SELECT count(*) FROM files WHERE link_id=?", l.ID) != 2 {
				t.Fatal("restart duplicated completed files")
			}
		})
	}
}

func rawMigrationDB(t *testing.T, dir string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "metadata.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	return db
}

func schemaCount(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var count int
	if err := db.QueryRow(query).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func TestSchemaCreationAndReopeningV2(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	if schemaCount(t, db, "PRAGMA user_version") != 2 ||
		schemaCount(t, db, "SELECT count(*) FROM pragma_table_info('attempts') WHERE name='admission_comment' AND \"notnull\"=1") != 1 {
		t.Fatal("fresh database lacks version 2 schema")
	}
	// A v2 open should not replay the fresh-schema settings insert.
	if _, err = db.Exec(`CREATE TRIGGER reject_settings_insert BEFORE INSERT ON settings
		BEGIN SELECT RAISE(ABORT,'schema replayed'); END`); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if schemaCount(t, db, "PRAGMA user_version") != 2 {
		t.Fatal("reopen changed current schema version")
	}
}

func TestV1MigrationFailureRollsBackSchemaAndBackfill(t *testing.T) {
	dir := t.TempDir()
	db := rawMigrationDB(t, dir)
	_, err := db.Exec(`CREATE TABLE attempts(id TEXT PRIMARY KEY, comment TEXT NOT NULL);
		INSERT INTO attempts VALUES('first','Original comment'),('second','');
		PRAGMA user_version=1;
		CREATE TRIGGER fail_backfill BEFORE UPDATE ON attempts WHEN OLD.id='second'
		BEGIN SELECT RAISE(ABORT,'simulated backfill failure'); END;`)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := openDB(dir); err == nil {
		opened.Close()
		t.Fatal("migration succeeded despite failed backfill")
	}
	db = rawMigrationDB(t, dir)
	if schemaCount(t, db, "PRAGMA user_version") != 1 ||
		schemaCount(t, db, "SELECT count(*) FROM pragma_table_info('attempts') WHERE name='admission_comment'") != 0 ||
		schemaCount(t, db, "SELECT count(*) FROM attempts WHERE (id='first' AND comment='Original comment') OR (id='second' AND comment='')") != 2 {
		t.Fatal("failed migration committed a partial schema, version, or comment change")
	}
	if _, err = db.Exec("DROP TRIGGER fail_backfill"); err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = openDB(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if schemaCount(t, db, "PRAGMA user_version") != 2 ||
		schemaCount(t, db, "SELECT count(*) FROM attempts WHERE admission_comment=comment") != 2 {
		t.Fatal("migration retry failed to preserve original admission comments")
	}
}

func TestFreshSchemaFailureRollsBack(t *testing.T) {
	dir := t.TempDir()
	db := rawMigrationDB(t, dir)
	if _, err := db.Exec("CREATE TABLE attempts(id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := openDB(dir); err == nil {
		opened.Close()
		t.Fatal("incompatible unversioned database accepted")
	}
	db = rawMigrationDB(t, dir)
	defer db.Close()
	if schemaCount(t, db, "PRAGMA user_version") != 0 ||
		schemaCount(t, db, "SELECT count(*) FROM sqlite_master WHERE type='table'") != 1 {
		t.Fatal("failed schema creation left partially created tables or version")
	}
}

func TestFutureSchemaRejectedWithoutChanges(t *testing.T) {
	dir := t.TempDir()
	db := rawMigrationDB(t, dir)
	if _, err := db.Exec("PRAGMA user_version=3"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if opened, err := openDB(dir); err == nil {
		opened.Close()
		t.Fatal("future schema accepted")
	} else if !strings.Contains(err.Error(), "database schema 3 is newer") {
		t.Fatalf("incorrect future schema error: %v", err)
	}
	db = rawMigrationDB(t, dir)
	defer db.Close()
	if schemaCount(t, db, "PRAGMA user_version") != 3 ||
		schemaCount(t, db, "SELECT count(*) FROM sqlite_master") != 0 {
		t.Fatal("future database was modified")
	}
}
