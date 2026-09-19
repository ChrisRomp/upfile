package app

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func mutationExec(t *testing.T, h *harness, query string, args ...any) {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	if _, err := h.a.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func mutationCount(t *testing.T, h *harness, query string, args ...any) int {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	var count int
	if err := h.a.db.QueryRow(query, args...).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func mutationLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var logs bytes.Buffer
	logger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(logger) })
	return &logs
}

func TestCreationAuditFailureRollsBack(t *testing.T) {
	for _, kind := range []string{"container", "initial link", "link"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			path := "/api/containers"
			payload := map[string]any{"name": "Must roll back"}
			action := "container.save"
			if kind == "initial link" {
				action = "link.save"
			}
			if kind == "link" {
				c, _, _ := h.link(nil)
				path = "/api/containers/" + c.ID + "/links"
				payload = map[string]any{"sender_label": "Must roll back"}
				action = "link.save"
			}
			before := mutationCount(t, h, "SELECT (SELECT count(*) FROM containers)+(SELECT count(*) FROM links)+(SELECT count(*) FROM audit)")
			mutationExec(t, h, `CREATE TRIGGER fail_creation_audit BEFORE INSERT ON audit
				WHEN NEW.action='`+action+`'
				BEGIN SELECT RAISE(ABORT,'simulated audit insert failure'); END`)
			w := h.call(true, "POST", path, payload, nil, 500)
			if strings.Contains(w.Body.String(), `"url"`) {
				t.Fatal("failed creation returned a secret URL")
			}
			after := mutationCount(t, h, "SELECT (SELECT count(*) FROM containers)+(SELECT count(*) FROM links)+(SELECT count(*) FROM audit)")
			if before != after {
				t.Fatal("failed audit left creation records or success events")
			}
		})
	}
}

func TestDeletionAuditFailureRollsBack(t *testing.T) {
	for _, kind := range []string{"file", "container"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			c, l, cookie := h.link(nil)
			received := h.admit(l, cookie, "delete-audit-received", 3, 201)
			h.patch(l, cookie, received, 0, "abc", 204)
			pending := h.admit(l, cookie, "delete-audit-pending", 6, 201)
			h.patch(l, cookie, pending, 0, "ab", 204)
			body, err := os.Open(h.a.completed(received.ID))
			if err != nil {
				t.Fatal(err)
			}
			defer body.Close()
			stopped := false
			h.a.mu.Lock()
			h.a.downloads["held"] = &download{file: received.ID, container: c.ID, body: body}
			h.a.transfers[pending.ID] = &transfer{busy: true, stop: func() { stopped = true }}
			beforeC, e1 := h.a.container(c.ID)
			beforeL, e2 := h.a.link(l.ID)
			beforeAttempt, e3 := h.a.attempt(pending.ID)
			beforeFile, e4 := h.a.file(received.ID)
			h.a.mu.Unlock()
			if err := errors.Join(e1, e2, e3, e4); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				delete(h.a.downloads, "held")
				delete(h.a.transfers, pending.ID)
			})
			mutationExec(t, h, `CREATE TRIGGER fail_deletion_audit BEFORE INSERT ON audit
				WHEN NEW.action='`+kind+`.delete'
				BEGIN SELECT RAISE(ABORT,'simulated audit insert failure'); END`)
			path := "/api/files/" + received.ID
			if kind == "container" {
				path = "/api/containers/" + c.ID
			}
			h.call(true, "DELETE", path, nil, nil, 500)
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				afterC, e1 := h.a.container(c.ID)
				afterL, e2 := h.a.link(l.ID)
				afterAttempt, e3 := h.a.attempt(pending.ID)
				afterFile, e4 := h.a.file(received.ID)
				if err := errors.Join(e1, e2, e3, e4); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(beforeC, afterC) || !reflect.DeepEqual(beforeL, afterL) ||
					beforeAttempt != afterAttempt || beforeFile != afterFile || stopped {
					t.Fatal("failed audit changed destructive state or stopped an upload")
				}
			}()
			if got := mutationCount(t, h, "SELECT count(*) FROM sessions WHERE hash=? AND link_id=?", digest(cookie.Value), l.ID); got != 1 {
				t.Fatal("failed audit deleted the upload session")
			}
			if got := mutationCount(t, h, "SELECT count(*) FROM audit WHERE action=?", kind+".delete"); got != 0 {
				t.Fatal("failed deletion left a success audit event")
			}
			b := make([]byte, 3)
			if _, err = body.ReadAt(b, 0); err != nil || string(b) != "abc" {
				t.Fatalf("failed audit stopped a download: %q, %v", b, err)
			}
			if data, err := os.ReadFile(h.a.partial(pending.ID)); err != nil || string(data) != "ab" {
				t.Fatalf("failed audit removed upload bytes: %q, %v", data, err)
			}
			h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 200)
		})
	}
}

func TestDeletionCleanupFailureIsDeferred(t *testing.T) {
	for _, kind := range []string{"file", "container"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			c, l, cookie := h.link(nil)
			received := h.admit(l, cookie, "delete-cleanup-received", 3, 201)
			h.patch(l, cookie, received, 0, "abc", 204)
			path, blockedPath := "/api/files/"+received.ID, h.a.completed(received.ID)
			var pending Attempt
			var wantReserved int64
			if kind == "container" {
				path = "/api/containers/" + c.ID
				pending = h.admit(l, cookie, "delete-cleanup-pending", 6, 201)
				blockedPath = h.a.completed(pending.ID)
				wantReserved = 6
			} else if err := os.Rename(blockedPath, blockedPath+".saved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(blockedPath, 0700); err != nil {
				t.Fatal(err)
			}
			logs := mutationLogs(t)
			for i := 0; i < 2; i++ {
				w := h.call(true, "DELETE", path, nil, nil, 200)
				if got := decode[map[string]string](t, w); !reflect.DeepEqual(got, map[string]string{"status": "deleting"}) {
					t.Fatalf("cleanup failure reported a completed deletion: %v", got)
				}
			}
			if !strings.Contains(logs.String(), "storage cleanup pending") || !strings.Contains(logs.String(), kind+".delete") {
				t.Fatalf("deferred cleanup was not logged: %s", logs.String())
			}
			s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			if s.StoredBytes != 3 || s.ReservedBytes != wantReserved || s.CleanupErrors == 0 {
				t.Fatalf("pending deletion lost its quota charge: %+v", s)
			}
			if mutationCount(t, h, "SELECT count(*) FROM audit WHERE action=?", kind+".delete") != 1 {
				t.Fatal("pending deletion retry emitted duplicate audit events")
			}
			if kind == "container" {
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 410)
				if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=1 AND reserved=6", pending.ID) != 1 ||
					mutationCount(t, h, "SELECT count(*) FROM sessions WHERE link_id=?", l.ID) != 0 ||
					mutationCount(t, h, "SELECT count(*) FROM containers WHERE id=? AND status='deleting'", c.ID) != 1 {
					t.Fatal("cleanup failure lost the durable deletion intent")
				}
			}
			if err := os.Remove(blockedPath); err != nil {
				t.Fatal(err)
			}
			if kind == "file" {
				if err := os.Rename(blockedPath+".saved", blockedPath); err != nil {
					t.Fatal(err)
				}
			}
			w := h.call(true, "DELETE", path, nil, nil, 200)
			if got := decode[map[string]string](t, w); !reflect.DeepEqual(got, map[string]string{"status": "deleted"}) {
				t.Fatalf("cleanup retry did not finish deletion: %v", got)
			}
			s = decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			if s.StoredBytes != 0 || s.ReservedBytes != 0 || s.CleanupErrors != 0 {
				t.Fatalf("successful cleanup did not release quota: %+v", s)
			}
			if _, err := os.Stat(h.a.completed(received.ID)); !os.IsNotExist(err) {
				t.Fatalf("deleted file still exists: %v", err)
			}
			if mutationCount(t, h, "SELECT count(*) FROM audit WHERE action=?", kind+".delete") != 1 {
				t.Fatal("cleanup completion emitted another audit event")
			}
		})
	}
}

func TestContainerDeletionStopsDownloadsAfterWriterFailure(t *testing.T) {
	h := setup(t)
	c, l, cookie := h.link(nil)
	received := h.admit(l, cookie, "delete-db-close-received", 3, 201)
	h.patch(l, cookie, received, 0, "abc", 204)
	pending := h.admit(l, cookie, "delete-db-close-pending", 6, 201)
	body, err := os.Open(h.a.completed(received.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	stopped := false
	h.a.mu.Lock()
	h.a.downloads["held"] = &download{file: received.ID, container: c.ID, body: body}
	h.a.transfers[pending.ID] = &transfer{busy: true, stop: func() {
		stopped = true
		if err := h.a.db.Close(); err != nil {
			t.Error(err)
		}
	}}
	h.a.mu.Unlock()
	logs := mutationLogs(t)
	w := h.call(true, "DELETE", "/api/containers/"+c.ID, nil, nil, 200)
	if got := decode[map[string]string](t, w); got["status"] != "deleting" || !stopped {
		t.Fatal("post-commit database failure hid the accepted deletion")
	}
	if _, err = body.ReadAt(make([]byte, 1), 0); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("download was not canceled despite writer/cleanup failure: %v", err)
	}
	if !strings.Contains(logs.String(), "confirm deletion") || !strings.Contains(logs.String(), "container.delete") {
		t.Fatalf("failed state confirmation was not logged: %s", logs.String())
	}
	func() {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		delete(h.a.downloads, "held")
		delete(h.a.transfers, pending.ID)
		db, err := openDB(h.a.cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		h.a.db = db
		s, err := h.a.settings()
		if err != nil || s.StoredBytes != 3 || s.ReservedBytes != 6 || s.CleanupErrors != 2 {
			t.Fatalf("post-commit failure lost quota/pending cleanup: %+v, %v", s, err)
		}
		if err = h.a.stopInvalidWriters(); err != nil {
			t.Fatal(err)
		}
	}()
	if mutationCount(t, h, "SELECT count(*) FROM containers WHERE id=? AND status='deleted'", c.ID) != 1 ||
		mutationCount(t, h, "SELECT count(*) FROM audit WHERE target=? AND action='container.delete'", c.ID) != 1 {
		t.Fatal("deletion was not durably audited and completed on retry")
	}
}

func TestCreationIgnoresUnrelatedInvalidUpload(t *testing.T) {
	h := setup(t)
	c, l, cookie := h.link(nil)
	pending := h.admit(l, cookie, "unrelated-invalid-cleanup", 6, 201)
	mutationExec(t, h, "UPDATE links SET expires=0 WHERE id=?", l.ID)
	if err := os.Remove(h.a.partial(pending.ID)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.a.partial(pending.ID), 0700); err != nil {
		t.Fatal(err)
	}
	newLink := decode[Link](t, h.call(true, "POST", "/api/containers/"+c.ID+"/links", map[string]any{"sender_label": "New"}, nil, 200))
	rotationSecret(t, newLink)
	newContainer := decode[CreatedContainer](t, h.call(true, "POST", "/api/containers", map[string]any{"name": "New"}, nil, 200))
	rotationSecret(t, newContainer.InitialLink)
	if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='uploading' AND cleanup=0 AND reserved=6", pending.ID) != 1 {
		t.Fatal("creation performed unrelated upload maintenance")
	}
	if err := os.Remove(h.a.partial(pending.ID)); err != nil {
		t.Fatal(err)
	}
}

// Wrap the real SQLite connection, rejecting model reads inside a transaction or
// all reads after commit without adding fault-injection hooks to production code.
type mutationReadFault struct {
	inTx, committed, failModelInTx, failAfterCommit bool
	rejected, commits                               int
}

type mutationConnector struct {
	dsn   string
	fault *mutationReadFault
}

func (c *mutationConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(c.dsn)
	if err != nil {
		return nil, err
	}
	return &mutationConn{Conn: conn, fault: c.fault}, nil
}

func (*mutationConnector) Driver() driver.Driver { return &sqlite.Driver{} }

type mutationConn struct {
	driver.Conn
	fault *mutationReadFault
}

func (c *mutationConn) Prepare(query string) (driver.Stmt, error) {
	f := c.fault
	if (f.failAfterCommit && f.committed && strings.HasPrefix(query, "SELECT")) ||
		(f.failModelInTx && f.inTx && strings.HasPrefix(query, "SELECT id,")) {
		f.rejected++
		return nil, errors.New("simulated response read failure")
	}
	return c.Conn.Prepare(query)
}

func (c *mutationConn) Begin() (driver.Tx, error) {
	tx, err := c.Conn.Begin()
	if err != nil {
		return nil, err
	}
	c.fault.inTx = true
	return &mutationTx{Tx: tx, fault: c.fault}, nil
}

type mutationTx struct {
	driver.Tx
	fault *mutationReadFault
}

func (tx *mutationTx) Commit() error {
	err := tx.Tx.Commit()
	tx.fault.inTx = false
	if err == nil {
		tx.fault.committed = true
		tx.fault.commits++
	}
	return err
}

func (tx *mutationTx) Rollback() error {
	tx.fault.inTx = false
	return tx.Tx.Rollback()
}

func installMutationReadFault(t *testing.T, h *harness, fault *mutationReadFault) {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	if err := h.a.db.Close(); err != nil {
		t.Fatal(err)
	}
	u := url.URL{Scheme: "file", Path: filepath.Join(h.a.cfg.DataDir, "metadata.db")}
	q := u.Query()
	for _, pragma := range []string{"foreign_keys(1)", "busy_timeout(5000)", "journal_mode(WAL)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	h.a.db = sql.OpenDB(&mutationConnector{dsn: u.String(), fault: fault})
	h.a.db.SetMaxOpenConns(1)
}

func TestCreationPreparesResponseBeforeCommit(t *testing.T) {
	for _, kind := range []string{"container", "link"} {
		for _, phase := range []string{"before commit", "after commit"} {
			t.Run(kind+"/"+phase, func(t *testing.T) {
				h := setup(t)
				now := time.Now().Truncate(time.Second)
				h.a.mu.Lock()
				h.a.now = func() time.Time { return now }
				h.a.mu.Unlock()
				path := "/api/containers"
				payload := map[string]any{"name": "Prepared response", "max_file_bytes": 512}
				if kind == "link" {
					max := int64(512)
					c, _, _ := h.link(&max)
					path = "/api/containers/" + c.ID + "/links"
					payload = map[string]any{"sender_label": "Prepared response", "max_file_bytes": 256}
				}
				before := mutationCount(t, h, "SELECT (SELECT count(*) FROM containers)+(SELECT count(*) FROM links)+(SELECT count(*) FROM audit)")
				fault := &mutationReadFault{failModelInTx: phase == "before commit", failAfterCommit: phase == "after commit"}
				installMutationReadFault(t, h, fault)
				want := 200
				if phase == "before commit" {
					want = 500
				}
				w := h.call(true, "POST", path, payload, nil, want)
				h.a.mu.Lock()
				commits, rejected := fault.commits, fault.rejected
				fault.failAfterCommit, fault.failModelInTx = false, false
				h.a.mu.Unlock()
				if phase == "before commit" {
					if commits != 0 || rejected != 1 || strings.Contains(w.Body.String(), `"url"`) ||
						mutationCount(t, h, "SELECT (SELECT count(*) FROM containers)+(SELECT count(*) FROM links)+(SELECT count(*) FROM audit)") != before {
						t.Fatal("response preparation failure committed a mutation or exposed a secret")
					}
					return
				}
				if commits != 1 || rejected != 0 {
					t.Fatalf("creation attempted fallible post-commit reads: commits=%d reads=%d", commits, rejected)
				}
				link := decode[Link](t, w)
				if kind == "container" {
					created := decode[CreatedContainer](t, w)
					link = created.InitialLink
					stored := decode[Container](t, h.call(true, "GET", "/api/containers/"+created.ID, nil, nil, 200))
					if !reflect.DeepEqual(created.Container, stored) || created.LinkCount != 1 || created.LastActivity != now.Unix() ||
						link.SenderLabel != created.Name || link.MaxFileBytes != nil || link.EffectiveMax != 512 {
						t.Fatalf("initial-link response metadata mismatch: %+v", created)
					}
				} else if link.EffectiveMax != 256 || link.MaxFileBytes == nil || *link.MaxFileBytes != 256 {
					t.Fatalf("link override not reflected: %+v", link)
				}
				secret := rotationSecret(t, link)
				if link.ExpiresAt != now.Add(168*time.Hour).Unix() || link.CreatedAt != now.Unix() ||
					link.Status != "active" || link.Active != 0 || link.FileCount != 0 || link.Hash != "" {
					t.Fatalf("incorrect committed link metadata: %+v", link)
				}
				func() {
					h.a.mu.Lock()
					defer h.a.mu.Unlock()
					stored, err := h.a.link(link.ID)
					if err != nil || stored.Hash != digest(secret) {
						t.Fatalf("returned secret does not match storage: %+v, %v", stored, err)
					}
					stored.Hash, stored.URL = "", link.URL
					if !reflect.DeepEqual(link, stored) {
						t.Fatalf("response differs from committed link: %+v, %+v", link, stored)
					}
				}()
				h.call(false, "POST", "/api/links/"+link.ID+"/exchange", map[string]string{"secret": secret}, nil, 200)
				for _, path := range []string{"/api/containers/" + link.ContainerID, "/api/containers/" + link.ContainerID + "/links", "/api/audit"} {
					body := h.call(true, "GET", path, nil, nil, 200).Body.String()
					if strings.Contains(body, secret) || strings.Contains(body, digest(secret)) || strings.Contains(body, `"url"`) {
						t.Fatal("one-time credentials leaked in a subsequent read")
					}
				}
			})
		}
	}
}

func TestFileDeletionConfirmationFailureIsDeferred(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "delete-confirmation-failure", 3, 201)
	h.patch(l, cookie, v, 0, "abc", 204)
	fault := &mutationReadFault{failAfterCommit: true}
	installMutationReadFault(t, h, fault)
	logs := mutationLogs(t)
	w := h.call(true, "DELETE", "/api/files/"+v.ID, nil, nil, 200)
	if got := decode[map[string]string](t, w); got["status"] != "deleting" {
		t.Fatalf("unconfirmed deletion reported success: %v", got)
	}
	h.a.mu.Lock()
	commits, rejected := fault.commits, fault.rejected
	fault.failAfterCommit = false
	h.a.mu.Unlock()
	if commits != 1 || rejected != 1 || !strings.Contains(logs.String(), "confirm deletion") || !strings.Contains(logs.String(), "file.delete") {
		t.Fatalf("confirmation failure was not deferred/logged: %s", logs.String())
	}
	if mutationCount(t, h, "SELECT count(*) FROM files WHERE id=? AND status='deleted'", v.ID) != 1 ||
		mutationCount(t, h, "SELECT count(*) FROM audit WHERE target=? AND action='file.delete'", v.ID) != 1 {
		t.Fatal("confirmation failure undid the audited physical deletion")
	}
	if _, err := os.Stat(h.a.completed(v.ID)); !os.IsNotExist(err) {
		t.Fatalf("physical deletion did not finish: %v", err)
	}
}
