package app

import (
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestEditAuditFailureRollsBack(t *testing.T) {
	for _, kind := range []string{"container", "link"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			c, l, cookie := h.link(nil)
			pending := h.admit(l, cookie, "edit-audit-pending", 6, 201)
			h.patch(l, cookie, pending, 0, "ab", 204)
			h.a.mu.Lock()
			beforeC, e1 := h.a.container(c.ID)
			beforeL, e2 := h.a.link(l.ID)
			beforeAttempt, e3 := h.a.attempt(pending.ID)
			h.a.mu.Unlock()
			if e1 != nil || e2 != nil || e3 != nil {
				t.Fatalf("snapshot failed: %v %v %v", e1, e2, e3)
			}
			path := "/api/containers/" + c.ID
			payload := map[string]any{"name": "Edited", "instructions": "New instructions", "max_file_bytes": 5}
			if kind == "link" {
				path = "/api/links/" + l.ID
				payload = map[string]any{"sender_label": "Edited", "expires_at": l.ExpiresAt + 3600, "max_file_bytes": 5}
			}
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit")
			mutationExec(t, h, `CREATE TRIGGER fail_edit_audit BEFORE INSERT ON audit
				WHEN NEW.action='`+kind+`.save'
				BEGIN SELECT RAISE(ABORT,'simulated audit insert failure'); END`)
			h.call(true, "PUT", path, payload, nil, 500)
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				afterC, e1 := h.a.container(c.ID)
				afterL, e2 := h.a.link(l.ID)
				afterAttempt, e3 := h.a.attempt(pending.ID)
				if e1 != nil || e2 != nil || e3 != nil {
					t.Fatalf("snapshot failed: %v %v %v", e1, e2, e3)
				}
				if !reflect.DeepEqual(beforeC, afterC) || !reflect.DeepEqual(beforeL, afterL) || beforeAttempt != afterAttempt {
					t.Fatal("failed audit persisted edited metadata or canceled an upload")
				}
			}()
			if mutationCount(t, h, "SELECT count(*) FROM audit") != beforeAudit {
				t.Fatal("failed edit left a success event")
			}
			if data, err := os.ReadFile(h.a.partial(pending.ID)); err != nil || string(data) != "ab" {
				t.Fatalf("failed edit removed bytes: %q, %v", data, err)
			}
			h.patch(l, cookie, pending, 2, "cdef", 204)
		})
	}
}

func TestEditInvalidatesOnlyAffectedUploads(t *testing.T) {
	for _, kind := range []string{"container", "link"} {
		for _, cause := range []string{"size", "expired session", "missing session", "revoked link"} {
			t.Run(kind+"/"+cause, func(t *testing.T) {
				h := setup(t)
				c, l, cookie := h.link(nil)
				pending := h.admit(l, cookie, "edit-invalidated-upload", 6, 201)
				h.patch(l, cookie, pending, 0, "ab", 204)
				_, other, otherCookie := h.link(nil)
				untouched := h.admit(other, otherCookie, "edit-unrelated-upload", 6, 201)
				var max any
				switch cause {
				case "size":
					max = 5
				case "expired session":
					mutationExec(t, h, "UPDATE sessions SET expires=0 WHERE hash=?", digest(cookie.Value))
				case "missing session":
					mutationExec(t, h, "DELETE FROM sessions WHERE hash=?", digest(cookie.Value))
				case "revoked link":
					mutationExec(t, h, "UPDATE links SET revoked=1 WHERE id=?", l.ID)
				}
				path := "/api/containers/" + c.ID
				payload := map[string]any{"name": "Edited", "max_file_bytes": max}
				if kind == "link" {
					path = "/api/links/" + l.ID
					payload = map[string]any{"sender_label": "Edited", "expires_at": time.Now().Add(30 * time.Minute).Unix(), "max_file_bytes": max}
				}
				// Keep cleanup pending to distinguish committed cancellation from physical cleanup.
				if err := os.Mkdir(h.a.completed(pending.ID), 0700); err != nil {
					t.Fatal(err)
				}
				logs := mutationLogs(t)
				beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit WHERE action=?", kind+".save")
				w := h.call(true, "PUT", path, payload, nil, 200)
				if strings.Contains(w.Body.String(), `"url"`) || !strings.Contains(logs.String(), kind+".save") {
					t.Fatal("edit exposed a secret or failed to log pending cleanup")
				}
				if kind == "link" {
					got := decode[Link](t, w)
					if got.Active != 0 || got.ExpiresAt != payload["expires_at"] || got.SenderLabel != "Edited" {
						t.Fatalf("incorrect edited link metadata: %+v", got)
					}
				} else if got := decode[Container](t, w); got.ActiveUploads != 0 || got.Name != "Edited" {
					t.Fatalf("incorrect edited container metadata: %+v", got)
				}
				if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=1 AND reserved=6", pending.ID) != 1 ||
					mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='uploading' AND cleanup=0 AND reserved=6", untouched.ID) != 1 {
					t.Fatal("edit did not isolate cancellation to the affected uploads")
				}
				if mutationCount(t, h, "SELECT count(*) FROM audit WHERE action=?", kind+".save") != beforeAudit+1 {
					t.Fatal("committed edit was not audited exactly once")
				}
				if err := os.Remove(h.a.completed(pending.ID)); err != nil {
					t.Fatal(err)
				}
				h.a.mu.Lock()
				err := h.a.stopInvalidWriters()
				h.a.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=0 AND reserved=0", pending.ID) != 1 {
					t.Fatal("edit cleanup retry did not release quota")
				}
			})
		}
	}
}

func TestEditCancellationFailureRollsBack(t *testing.T) {
	for _, kind := range []string{"container", "link"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			c, l, cookie := h.link(nil)
			pending := h.admit(l, cookie, "edit-cancel-rollback", 6, 201)
			mutationExec(t, h, `CREATE TRIGGER fail_edit_cancel BEFORE UPDATE ON attempts
				WHEN NEW.status='canceled'
				BEGIN SELECT RAISE(ABORT,'simulated cancellation failure'); END`)
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit")
			path := "/api/containers/" + c.ID
			payload := map[string]any{"name": "Edited", "max_file_bytes": 5}
			if kind == "link" {
				path = "/api/links/" + l.ID
				payload = map[string]any{"sender_label": "Edited", "expires_at": l.ExpiresAt, "max_file_bytes": 5}
			}
			h.call(true, "PUT", path, payload, nil, 500)
			if mutationCount(t, h, "SELECT count(*) FROM audit") != beforeAudit ||
				mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='uploading' AND cleanup=0", pending.ID) != 1 {
				t.Fatal("failed cancellation left a partial edit or audit")
			}
			h.a.mu.Lock()
			current, err := h.a.link(l.ID)
			h.a.mu.Unlock()
			if err != nil || current.EffectiveMax != 1024 || current.SenderLabel != l.SenderLabel {
				t.Fatalf("failed cancellation changed effective metadata: %+v, %v", current, err)
			}
		})
	}
}

func TestLinkEditExpirationPreservesValidSiblingUploads(t *testing.T) {
	h := setup(t)
	c, l, cookie := h.link(nil)
	pending := h.admit(l, cookie, "edit-expiration-pending", 6, 201)
	sibling := decode[Link](t, h.call(true, "POST", "/api/containers/"+c.ID+"/links", map[string]any{"sender_label": "Sibling"}, nil, 200))
	exchange := h.call(false, "POST", "/api/links/"+sibling.ID+"/exchange", map[string]string{"secret": rotationSecret(t, sibling)}, nil, 200)
	other := h.admit(sibling, exchange.Result().Cookies()[0], "edit-expiration-sibling", 6, 201)
	now := time.Now().Truncate(time.Second)
	h.a.mu.Lock()
	h.a.now = func() time.Time { return now }
	h.a.mu.Unlock()
	expires := now.Add(time.Minute).Unix()
	edited := decode[Link](t, h.call(true, "PUT", "/api/links/"+l.ID, map[string]any{
		"sender_label": "Shortened", "expires_at": expires,
	}, nil, 200))
	if edited.ExpiresAt != expires || edited.Active != 1 {
		t.Fatalf("valid upload was canceled prematurely: %+v", edited)
	}
	h.a.mu.Lock()
	h.a.now = func() time.Time { return now.Add(61 * time.Second) }
	err := h.a.sweep()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND reserved=0", pending.ID) != 1 ||
		mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='uploading' AND reserved=6", other.ID) != 1 {
		t.Fatal("shortened expiry was not enforced without affecting the sibling link")
	}
}

func TestEditReturnsPreparedResponseAfterWriterFailure(t *testing.T) {
	for _, kind := range []string{"container", "link"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			c, l, cookie := h.link(nil)
			pending := h.admit(l, cookie, "edit-db-close-pending", 6, 201)
			stopped := false
			h.a.mu.Lock()
			h.a.transfers[pending.ID] = &transfer{busy: true, stop: func() {
				stopped = true
				if err := h.a.db.Close(); err != nil {
					t.Error(err)
				}
			}}
			h.a.mu.Unlock()
			path, id := "/api/containers/"+c.ID, c.ID
			payload := map[string]any{"name": "Edited", "max_file_bytes": 5}
			if kind == "link" {
				path, id = "/api/links/"+l.ID, l.ID
				payload = map[string]any{"sender_label": "Edited", "expires_at": l.ExpiresAt, "max_file_bytes": 5}
			}
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit WHERE target=? AND action=?", id, kind+".save")
			logs := mutationLogs(t)
			w := h.call(true, "PUT", path, payload, nil, 200)
			got := decode[struct {
				Active int   `json:"active_uploads"`
				Max    int64 `json:"effective_max_bytes"`
			}](t, w)
			if !stopped || got.Active != 0 || got.Max != 5 || !strings.Contains(logs.String(), kind+".save") {
				t.Fatalf("post-commit failure hid the edited response: %+v, %s", got, logs.String())
			}
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				delete(h.a.transfers, pending.ID)
				db, err := openDB(h.a.cfg.DataDir)
				if err != nil {
					t.Fatal(err)
				}
				h.a.db = db
				v, err := h.a.attempt(pending.ID)
				if err != nil || v.Status != "canceled" || !v.Cleanup || v.Reserved != 6 {
					t.Fatalf("post-commit failure lost pending invalidation: %+v, %v", v, err)
				}
				if err = h.a.stopInvalidWriters(); err != nil {
					t.Fatal(err)
				}
			}()
			if mutationCount(t, h, "SELECT count(*) FROM audit WHERE target=? AND action=?", id, kind+".save") != beforeAudit+1 {
				t.Fatal("post-commit failure lost the edit audit")
			}
		})
	}
}
