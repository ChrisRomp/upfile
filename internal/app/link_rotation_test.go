package app

import (
	"bytes"
	"log/slog"
	"net/url"
	"os"
	"reflect"
	"strings"
	"testing"
)

func assertLinkInvalidationAudit(t *testing.T, h *harness, id, action string, want int) {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	var count int
	var who, gotAction string
	err := h.a.db.QueryRow(`SELECT count(*),coalesce(min(actor),''),coalesce(min(action),'')
		FROM audit WHERE target=? AND action IN ('link.rotate','link.revoke')`, id).Scan(&count, &who, &gotAction)
	if err != nil {
		t.Fatal(err)
	}
	if count != want || (want > 0 && (who != "test-admin" || gotAction != action)) {
		t.Fatalf("invalidation audit: count=%d actor=%q action=%q", count, who, gotAction)
	}
}

func rotationSecret(t *testing.T, l Link) string {
	t.Helper()
	u, err := url.Parse(l.URL)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "drop.test" || u.Path != "/u/"+l.ID || len(u.Fragment) != 64 {
		t.Fatalf("invalid replacement URL: %q", l.URL)
	}
	return u.Fragment
}

func TestLinkInvalidationAuditFailureRollsBack(t *testing.T) {
	for _, action := range []string{"rotate", "revoke"} {
		for _, status := range []string{"allocating", "uploading", "finalizing", "abandoned"} {
			t.Run(action+"/"+status, func(t *testing.T) {
				h := setup(t)
				_, l, cookie := h.link(nil)
				v := h.admit(l, cookie, "rotation-rollback-key", 6, 201)
				h.patch(l, cookie, v, 0, "ab", 204)
				var beforeLink Link
				var beforeAttempt Attempt
				var beforeExpiry int64
				func() {
					h.a.mu.Lock()
					defer h.a.mu.Unlock()
					if _, err := h.a.db.Exec("UPDATE attempts SET status=? WHERE id=?", status, v.ID); err != nil {
						t.Fatal(err)
					}
					var err error
					if beforeLink, err = h.a.link(l.ID); err != nil {
						t.Fatal(err)
					}
					if beforeAttempt, err = h.a.attempt(v.ID); err != nil {
						t.Fatal(err)
					}
					if err = h.a.db.QueryRow("SELECT expires FROM sessions WHERE hash=?", digest(cookie.Value)).Scan(&beforeExpiry); err != nil {
						t.Fatal(err)
					}
					if _, err = h.a.db.Exec(`CREATE TRIGGER fail_invalidation_audit BEFORE INSERT ON audit
						WHEN NEW.action IN ('link.rotate','link.revoke')
						BEGIN SELECT RAISE(ABORT,'simulated audit insert failure'); END`); err != nil {
						t.Fatal(err)
					}
				}()

				w := h.call(true, "POST", "/api/links/"+l.ID+"/"+action, map[string]any{}, nil, 500)
				if strings.Contains(w.Body.String(), `"url"`) {
					t.Fatal("failed transaction returned a replacement URL")
				}
				func() {
					h.a.mu.Lock()
					defer h.a.mu.Unlock()
					afterLink, err := h.a.link(l.ID)
					if err != nil || !reflect.DeepEqual(beforeLink, afterLink) {
						t.Fatalf("audit failure changed link: %+v, %v", afterLink, err)
					}
					afterAttempt, err := h.a.attempt(v.ID)
					if err != nil || beforeAttempt != afterAttempt {
						t.Fatalf("audit failure changed attempt: %+v, %v", afterAttempt, err)
					}
					var afterExpiry int64
					err = h.a.db.QueryRow("SELECT expires FROM sessions WHERE hash=? AND link_id=?", digest(cookie.Value), l.ID).Scan(&afterExpiry)
					if err != nil || afterExpiry != beforeExpiry {
						t.Fatalf("audit failure changed session: expiry=%d, %v", afterExpiry, err)
					}
				}()
				assertLinkInvalidationAudit(t, h, l.ID, "", 0)
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 200)
				h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": rotationSecret(t, l)}, cookie, 200)
				if data, err := os.ReadFile(h.a.partial(v.ID)); err != nil || string(data) != "ab" {
					t.Fatalf("audit failure removed partial bytes: %q, %v", data, err)
				}
			})
		}
	}
}

func TestLinkInvalidationSuccess(t *testing.T) {
	for _, action := range []string{"rotate", "revoke"} {
		t.Run(action, func(t *testing.T) {
			h := setup(t)
			max := int64(512)
			_, l, cookie := h.link(&max)
			received := h.admit(l, cookie, "rotation-received-key", 3, 201)
			h.patch(l, cookie, received, 0, "abc", 204)
			v := h.admit(l, cookie, "rotation-unfinished-key", 6, 201)
			h.patch(l, cookie, v, 0, "ab", 204)
			_, other, otherCookie := h.link(nil)
			otherAttempt := h.admit(other, otherCookie, "rotation-unrelated-key", 6, 201)

			w := h.call(true, "POST", "/api/links/"+l.ID+"/"+action, map[string]any{}, nil, 200)
			var current Link
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				var err error
				if current, err = h.a.link(l.ID); err != nil {
					t.Fatal(err)
				}
				var sessions int
				if err = h.a.db.QueryRow("SELECT count(*) FROM sessions WHERE link_id=?", l.ID).Scan(&sessions); err != nil || sessions != 0 {
					t.Fatalf("old sessions remain: %d, %v", sessions, err)
				}
				canceled, err := h.a.attempt(v.ID)
				if err != nil || canceled.Status != "canceled" || canceled.Cleanup || canceled.Reserved != 0 {
					t.Fatalf("unfinished upload was not canceled and cleaned: %+v, %v", canceled, err)
				}
				untouched, err := h.a.attempt(otherAttempt.ID)
				if err != nil || untouched.Status != "uploading" || untouched.Reserved != 6 || untouched.Cleanup {
					t.Fatalf("another link was affected: %+v, %v", untouched, err)
				}
				f, err := h.a.file(received.ID)
				if err != nil || f.Status != "ready" {
					t.Fatalf("received file was affected: %+v, %v", f, err)
				}
			}()
			assertLinkInvalidationAudit(t, h, l.ID, "link."+action, 1)
			h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": rotationSecret(t, l)}, nil, 410)
			h.call(false, "GET", "/api/links/"+other.ID, nil, otherCookie, 200)
			if data, err := os.ReadFile(h.a.completed(received.ID)); err != nil || string(data) != "abc" {
				t.Fatalf("received bytes were affected: %q, %v", data, err)
			}

			if action == "rotate" {
				rotated := decode[Link](t, w)
				secret := rotationSecret(t, rotated)
				if rotated.URL == l.URL || current.Hash != digest(secret) || current.Revoked {
					t.Fatal("replacement URL does not match the committed secret")
				}
				expected := current
				expected.Hash, expected.URL = "", rotated.URL
				if !reflect.DeepEqual(rotated, expected) || rotated.FileCount != 1 || rotated.Active != 0 || rotated.EffectiveMax != max {
					t.Fatalf("incorrect rotation response metadata: %+v", rotated)
				}
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 401)
				exchange := h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": secret}, nil, 200)
				cookies := exchange.Result().Cookies()
				if len(cookies) != 1 {
					t.Fatal("replacement secret did not establish a session")
				}
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookies[0], 200)
			} else {
				if got := decode[map[string]string](t, w); !reflect.DeepEqual(got, map[string]string{"status": "revoked"}) {
					t.Fatalf("revoke response changed: %v", got)
				}
				if !current.Revoked || current.Hash != digest(rotationSecret(t, l)) {
					t.Fatal("revocation did not preserve the hash and mark the link revoked")
				}
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 410)
			}
		})
	}
}

func TestLinkInvalidationCleanupFailureIsDeferred(t *testing.T) {
	for _, action := range []string{"rotate", "revoke"} {
		t.Run(action, func(t *testing.T) {
			h := setup(t)
			_, l, cookie := h.link(nil)
			v := h.admit(l, cookie, "rotation-cleanup-key", 6, 201)
			h.patch(l, cookie, v, 0, "ab", 204)
			if err := os.Mkdir(h.a.completed(v.ID), 0700); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			logger := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
			defer slog.SetDefault(logger)

			w := h.call(true, "POST", "/api/links/"+l.ID+"/"+action, map[string]any{}, nil, 200)
			if !strings.Contains(logs.String(), "storage cleanup pending") || !strings.Contains(logs.String(), "link."+action) {
				t.Fatalf("deferred cleanup was not reported: %s", logs.String())
			}
			assertLinkInvalidationAudit(t, h, l.ID, "link."+action, 1)
			s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			if s.ReservedBytes != 6 || s.CleanupErrors != 1 {
				t.Fatalf("pending cleanup stopped charging bytes or reporting errors: %+v", s)
			}
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				canceled, err := h.a.attempt(v.ID)
				if err != nil || canceled.Status != "canceled" || !canceled.Cleanup || canceled.Reserved != 6 {
					t.Fatalf("pending cleanup was not retained: %+v, %v", canceled, err)
				}
				current, err := h.a.link(l.ID)
				if err != nil {
					t.Fatal(err)
				}
				if action == "rotate" {
					rotated := decode[Link](t, w)
					if current.Hash != digest(rotationSecret(t, rotated)) || current.Revoked || rotated.Active != 0 {
						t.Fatal("cleanup failure hid or invalidated the committed replacement")
					}
				} else if !current.Revoked {
					t.Fatal("cleanup failure undid revocation")
				}
			}()
			if action == "rotate" {
				rotated := decode[Link](t, w)
				h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": rotationSecret(t, rotated)}, nil, 200)
				h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 401)
			}
			h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": rotationSecret(t, l)}, nil, 410)
			if err := os.Remove(h.a.completed(v.ID)); err != nil {
				t.Fatal(err)
			}
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				if err := h.a.stopInvalidWriters(); err != nil {
					t.Fatal(err)
				}
			}()
			s = decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			if s.ReservedBytes != 0 || s.CleanupErrors != 0 {
				t.Fatalf("cleanup retry did not release the reservation: %+v", s)
			}
			assertLinkInvalidationAudit(t, h, l.ID, "link."+action, 1)
		})
	}
}

func TestRotationReturnsURLWithoutPostCommitDatabaseReads(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "rotation-db-close-key", 6, 201)
	stopped := false
	h.a.mu.Lock()
	h.a.transfers[v.ID] = &transfer{busy: true, stop: func() {
		stopped = true
		if err := h.a.db.Close(); err != nil {
			t.Error(err)
		}
	}}
	h.a.mu.Unlock()

	// Writer cancellation happens after commit; no later database read can succeed.
	w := h.call(true, "POST", "/api/links/"+l.ID+"/rotate", map[string]any{}, nil, 200)
	rotated := decode[Link](t, w)
	secret := rotationSecret(t, rotated)
	if !stopped || rotated.ID != l.ID || rotated.Active != 0 || rotated.Status != "active" {
		t.Fatal("rotation did not return committed metadata after stopping its writer")
	}
	func() {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		delete(h.a.transfers, v.ID)
		db, err := openDB(h.a.cfg.DataDir)
		if err != nil {
			t.Fatal(err)
		}
		h.a.db = db
		current, err := h.a.link(l.ID)
		if err != nil || current.Hash != digest(secret) {
			t.Fatalf("replacement secret was not committed: %+v, %v", current, err)
		}
		canceled, err := h.a.attempt(v.ID)
		if err != nil || canceled.Status != "canceled" || !canceled.Cleanup || canceled.Reserved != 6 {
			t.Fatalf("failed post-commit reads lost pending cleanup: %+v, %v", canceled, err)
		}
		if err = h.a.stopInvalidWriters(); err != nil {
			t.Fatal(err)
		}
	}()
	assertLinkInvalidationAudit(t, h, l.ID, "link.rotate", 1)
	h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": secret}, nil, 200)
}

func TestRotationPrecomputedStatus(t *testing.T) {
	for _, tc := range []struct {
		name, update, want string
	}{
		{"revoked link", "UPDATE links SET revoked=1", "active"},
		{"expired link", "UPDATE links SET expires=0", "expired"},
		{"inactive container", "UPDATE containers SET status='deleting'", "revoked"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := setup(t)
			_, l, _ := h.link(nil)
			h.a.mu.Lock()
			_, err := h.a.db.Exec(tc.update)
			h.a.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			rotated := decode[Link](t, h.call(true, "POST", "/api/links/"+l.ID+"/rotate", map[string]any{}, nil, 200))
			rotationSecret(t, rotated)
			if rotated.Status != tc.want || rotated.Active != 0 {
				t.Fatalf("incorrect post-rotation status: %+v", rotated)
			}
			assertLinkInvalidationAudit(t, h, l.ID, "link.rotate", 1)
		})
	}
}
