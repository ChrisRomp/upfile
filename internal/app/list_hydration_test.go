package app

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"modernc.org/sqlite"
)

type listQueryLog struct {
	mu      sync.Mutex
	queries []string
}

func (l *listQueryLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	queries := l.queries
	l.queries = nil
	return queries
}

type listConnector struct{ log *listQueryLog }

func (c *listConnector) Driver() driver.Driver { return &sqlite.Driver{} }
func (c *listConnector) Connect(context.Context) (driver.Conn, error) {
	conn, err := c.Driver().Open(":memory:")
	if err != nil {
		return nil, err
	}
	return &listConn{Conn: conn, log: c.log}, nil
}

type listConn struct {
	driver.Conn
	log *listQueryLog
}

func (c *listConn) Prepare(query string) (driver.Stmt, error) {
	c.log.mu.Lock()
	c.log.queries = append(c.log.queries, query)
	c.log.mu.Unlock()
	return c.Conn.Prepare(query)
}

func listFixture(t *testing.T) (*App, *listQueryLog, string) {
	t.Helper()
	log := &listQueryLog{}
	db := sql.OpenDB(&listConnector{log: log})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := db.Exec(query, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(schema)
	exec("UPDATE settings SET max_file=1000,budget=100000 WHERE id=1")
	parent := fmt.Sprintf("%032x", 1)
	for i := 0; i < 30; i++ {
		var max any
		if i == 0 {
			max = 800
		} else if i%2 == 1 {
			max = 500
		}
		exec("INSERT INTO containers(id,name,instructions,max_file,created) VALUES(?,?,?,?,?)",
			fmt.Sprintf("%032x", i+1), fmt.Sprintf("Request %02d", i), "Instructions", max, 100+i)
		var linkMax any
		switch i % 3 {
		case 1:
			linkMax = 400
		case 2:
			linkMax = 1200
		}
		expiry := 3000
		if i == 0 {
			expiry = 999
		}
		exec("INSERT INTO links(id,container_id,sender,expires,max_file,created,hash,revoked) VALUES(?,?,?,?,?,?,?,?)",
			fmt.Sprintf("%032x", i+100), parent, fmt.Sprintf("Sender %02d", i), expiry, linkMax, 1000+i, "hashed-secret", i == 1)
	}
	linkID := fmt.Sprintf("%032x", 100)
	for i, state := range []string{"completed", "uploading", "completed"} {
		id := fmt.Sprintf("%032x", i+1000)
		exec(`INSERT INTO attempts(id,link_id,session_hash,key,name,comment,size,status,created,last,reserved)
			VALUES(?,?,'session',?,'file.txt','',7,?,2000,2000,0)`, id, linkID, id, state)
		if state == "completed" {
			fileState := "ready"
			if i == 2 {
				fileState = "deleted"
			}
			exec(`INSERT INTO files(id,container_id,link_id,name,original_name,sender,comment,size,created,status)
				VALUES(?,?,?,'file.txt','file.txt','Sender','',7,2000,?)`, id, parent, linkID, fileState)
		}
	}
	a := &App{
		db: db, now: func() time.Time { return time.Unix(2000, 0) },
		downloads: map[string]*download{"held": {container: parent}},
	}
	log.take()
	return a, log, parent
}

func assertListQueries(t *testing.T, queries []string, wantContainers int) {
	t.Helper()
	settings, containers := 0, 0
	for _, query := range queries {
		if strings.Contains(query, "FROM settings") {
			settings++
			if query != "SELECT max_file FROM settings WHERE id=1" {
				t.Errorf("hydration fetched more than the scalar global maximum: %s", query)
			}
		}
		if strings.Contains(query, "sum(reserved)") || strings.Contains(query, "count(*) FROM sessions") {
			t.Errorf("list ran global usage aggregates: %s", query)
		}
		if strings.Contains(query, "FROM containers c WHERE") {
			containers++
		}
	}
	if settings != 1 || containers != wantContainers {
		t.Fatalf("settings reads=%d (want 1), container hydrations=%d (want %d)", settings, containers, wantContainers)
	}
}

func TestListsReuseLimitsWithoutGlobalUsageScans(t *testing.T) {
	for _, kind := range []string{"containers", "links"} {
		for _, tc := range []struct {
			query string
			total int
			items int
		}{
			{"limit=1", 30, 1},
			{"limit=25", 30, 25},
			{"limit=100", 30, 30},
			{"limit=25&page=2", 30, 5},
			{"limit=25&q=missing", 0, 0},
		} {
			t.Run(kind+"/"+tc.query, func(t *testing.T) {
				a, log, parent := listFixture(t)
				r := httptest.NewRequest(http.MethodGet, "/api/"+kind+"?"+tc.query, nil)
				w := httptest.NewRecorder()
				if kind == "containers" {
					a.endpoint(a.listContainers)(w, r)
				} else {
					r.SetPathValue("container", parent)
					a.endpoint(a.listLinks)(w, r)
				}
				if w.Code != http.StatusOK {
					t.Fatalf("list status %d: %s", w.Code, w.Body.String())
				}
				if kind == "containers" {
					assertListQueries(t, log.take(), tc.items)
					got := decode[struct {
						Items []Container `json:"items"`
						Total int         `json:"total"`
					}](t, w)
					if got.Total != tc.total || len(got.Items) != tc.items {
						t.Fatalf("pagination: total=%d items=%d", got.Total, len(got.Items))
					}
					for _, item := range got.Items {
						expected, err := a.container(item.ID)
						if err != nil || !reflect.DeepEqual(item, expected) {
							t.Fatalf("container list changed metadata: %+v vs %+v, %v", item, expected, err)
						}
						if item.ID == parent && (item.EffectiveMax != 800 || item.FileCount != 1 || item.StoredBytes != 7 ||
							item.ActiveUploads != 1 || item.ActiveDownloads != 1 || item.LinkCount != 30 || item.LastActivity != 2000) {
							t.Fatalf("unexpected parent metadata: %+v", item)
						}
					}
				} else {
					assertListQueries(t, log.take(), 1)
					got := decode[struct {
						Items []Link `json:"items"`
						Total int    `json:"total"`
					}](t, w)
					if got.Total != tc.total || len(got.Items) != tc.items {
						t.Fatalf("pagination: total=%d items=%d", got.Total, len(got.Items))
					}
					for _, item := range got.Items {
						expected, err := a.link(item.ID)
						expected.Hash, expected.Revoked = "", false
						if err != nil || !reflect.DeepEqual(item, expected) {
							t.Fatalf("link list changed metadata: %+v vs %+v, %v", item, expected, err)
						}
						wantMax := int64(800)
						if item.MaxFileBytes != nil && *item.MaxFileBytes < wantMax {
							wantMax = *item.MaxFileBytes
						}
						if item.EffectiveMax != wantMax {
							t.Fatalf("incorrect inherited limit: %+v", item)
						}
						wantStatus := "active"
						if item.ExpiresAt <= a.now().Unix() {
							wantStatus = "expired"
						}
						if item.ID == fmt.Sprintf("%032x", 101) {
							wantStatus = "revoked"
						}
						if item.Status != wantStatus || item.URL != "" {
							t.Fatalf("incorrect status or exposed URL: %+v", item)
						}
					}
				}
			})
		}
	}
}

func TestHydrationReadsCurrentTransactionLimitWithoutUsageScans(t *testing.T) {
	a, log, parent := listFixture(t)
	tx, err := a.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE settings SET max_file=250 WHERE id=1"); err != nil {
		t.Fatal(err)
	}
	log.take()
	container, err := a.containerFrom(tx, parent)
	if err != nil || container.EffectiveMax != 250 {
		t.Fatalf("container did not see transaction settings: %+v, %v", container, err)
	}
	assertListQueries(t, log.take(), 1)
	link, err := a.linkFrom(tx, fmt.Sprintf("%032x", 100))
	if err != nil || link.EffectiveMax != 250 {
		t.Fatalf("link did not see transaction settings: %+v, %v", link, err)
	}
	assertListQueries(t, log.take(), 1)
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	container, err = a.container(parent)
	if err != nil || container.EffectiveMax != 800 {
		t.Fatalf("transaction limit escaped its scope: %+v, %v", container, err)
	}
}
