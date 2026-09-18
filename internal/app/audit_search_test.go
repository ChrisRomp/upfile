package app

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func TestListAuditSearch(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err := db.Exec(schema); err != nil {
		t.Fatal(err)
	}
	for i, event := range []struct{ actor, action, target string }{
		{"alpha-admin", "container.save", "request-a"},
		{"bob", "link.rotate", "request-b"},
		{"carol", "alpha.action", "request-c"},
		{"d'angelo", "file.rename", "request-d"},
		{"uploader", "file.received", "alpha-target"},
		{"alpha-admin", "alpha.action", "alpha-target"},
	} {
		if _, err := db.Exec("INSERT INTO audit(id,actor,action,target,created) VALUES(?,?,?,?,?)",
			i+1, event.actor, event.action, event.target, 1234567890); err != nil {
			t.Fatal(err)
		}
	}
	a := &App{db: db}
	for _, tc := range []struct {
		name  string
		query string
		page  int
		limit int
		total int
		ids   []int64
	}{
		{"omitted query", "", 0, 0, 6, []int64{6, 5, 4, 3, 2, 1}},
		{"empty query", "", 1, 25, 6, []int64{6, 5, 4, 3, 2, 1}},
		{"actor substring case insensitive", "ADMIN", 1, 25, 2, []int64{6, 1}},
		{"action substring", ".action", 1, 25, 2, []int64{6, 3}},
		{"target substring", "target", 1, 25, 2, []int64{6, 5}},
		{"quoted actor", "d'angelo", 1, 25, 1, []int64{4}},
		{"no matches", "missing", 1, 25, 0, []int64{}},
		{"SQL input remains data", "' OR 1=1 --", 1, 25, 0, []int64{}},
		{"first filtered page", "alpha", 1, 3, 4, []int64{6, 5, 3}},
		{"last filtered page", "alpha", 2, 3, 4, []int64{1}},
		{"past filtered pages", "alpha", 3, 3, 4, []int64{}},
		{"empty query second page", "", 2, 3, 6, []int64{3, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := "/api/audit"
			if tc.page != 0 {
				path += "?" + url.Values{
					"q":     {tc.query},
					"page":  {fmt.Sprint(tc.page)},
					"limit": {fmt.Sprint(tc.limit)},
				}.Encode()
			}
			w := httptest.NewRecorder()
			a.endpoint(a.listAudit)(w, httptest.NewRequest(http.MethodGet, path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			got := decode[struct {
				Items []struct {
					ID int64 `json:"id"`
				} `json:"items"`
				Total int `json:"total"`
			}](t, w)
			if got.Total != tc.total {
				t.Errorf("total = %d, want %d", got.Total, tc.total)
			}
			if got.Items == nil {
				t.Fatal("items must be an array, not null")
			}
			ids := make([]int64, len(got.Items))
			for i, event := range got.Items {
				ids[i] = event.ID
			}
			if !slices.Equal(ids, tc.ids) {
				t.Errorf("IDs = %v, want %v", ids, tc.ids)
			}
		})
	}
}
