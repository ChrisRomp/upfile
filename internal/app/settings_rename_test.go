package app

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestSettingsMutationFailureRollsBack(t *testing.T) {
	for _, failure := range []string{"audit", "cancellation", "link snapshot"} {
		t.Run(failure, func(t *testing.T) {
			h := setup(t)
			_, l, cookie := h.link(nil)
			received := h.admit(l, cookie, "settings-received-rollback", 3, 201)
			h.patch(l, cookie, received, 0, "abc", 204)
			pending := h.admit(l, cookie, "settings-pending-rollback", 6, 201)
			h.patch(l, cookie, pending, 0, "ab", 204)
			stopped := false
			h.a.mu.Lock()
			beforeSettings, e1 := h.a.settings()
			beforeFile, e2 := h.a.file(received.ID)
			beforeAttempt, e3 := h.a.attempt(pending.ID)
			h.a.transfers[pending.ID] = &transfer{busy: true, stop: func() { stopped = true }}
			h.a.mu.Unlock()
			t.Cleanup(func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				delete(h.a.transfers, pending.ID)
			})
			if err := errors.Join(e1, e2, e3); err != nil {
				t.Fatal(err)
			}
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit")
			switch failure {
			case "audit":
				mutationExec(t, h, `CREATE TRIGGER fail_settings_audit BEFORE INSERT ON audit
					WHEN NEW.action='settings.update'
					BEGIN SELECT RAISE(ABORT,'simulated audit failure'); END`)
			case "cancellation":
				mutationExec(t, h, `CREATE TRIGGER fail_settings_cancel BEFORE UPDATE ON attempts
					WHEN NEW.status='canceled'
					BEGIN SELECT RAISE(ABORT,'simulated cancellation failure'); END`)
			case "link snapshot":
				installMutationReadFault(t, h, &mutationReadFault{failModelInTx: true})
			}
			h.call(true, "PUT", "/api/settings", map[string]any{
				"max_file_bytes": 5, "storage_budget_bytes": 7, "default_link_hours": 24,
			}, nil, 500)
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				afterSettings, e1 := h.a.settings()
				afterFile, e2 := h.a.file(received.ID)
				afterAttempt, e3 := h.a.attempt(pending.ID)
				if err := errors.Join(e1, e2, e3); err != nil {
					t.Fatal(err)
				}
				if beforeSettings != afterSettings || beforeFile != afterFile || beforeAttempt != afterAttempt || stopped {
					t.Fatal("failed settings update changed metadata, quota, or an active writer")
				}
			}()
			if mutationCount(t, h, "SELECT count(*) FROM audit") != beforeAudit ||
				mutationCount(t, h, "SELECT count(*) FROM sessions WHERE hash=? AND link_id=?", digest(cookie.Value), l.ID) != 1 {
				t.Fatal("failed settings update changed audit events or credentials")
			}
			for path, want := range map[string]string{h.a.partial(pending.ID): "ab", h.a.completed(received.ID): "abc"} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Fatalf("failed settings update changed bytes: %q, %v", data, err)
				}
			}
			h.a.mu.Lock()
			delete(h.a.transfers, pending.ID)
			h.a.mu.Unlock()
			h.patch(l, cookie, pending, 2, "cdef", 204)
		})
	}
}

func TestSettingsLowerCapsCancelOnlyInvalidAttempts(t *testing.T) {
	for _, state := range []string{"allocating", "uploading", "finalizing"} {
		t.Run(state, func(t *testing.T) {
			h := setup(t)
			max := int64(10)
			_, l, cookie := h.link(&max)
			received := h.admit(l, cookie, "settings-existing-file", 6, 201)
			h.patch(l, cookie, received, 0, "abcdef", 204)
			pending := h.admit(l, cookie, "settings-oversize-file", 6, 201)
			h.patch(l, cookie, pending, 0, "ab", 204)
			mutationExec(t, h, "UPDATE attempts SET status=? WHERE id=?", state, pending.ID)
			max = 4
			_, other, otherCookie := h.link(&max)
			valid := h.admit(other, otherCookie, "settings-valid-file", 4, 201)
			stopped := false
			h.a.mu.Lock()
			beforeFile, err := h.a.file(received.ID)
			if state == "uploading" {
				writer := &transfer{busy: true}
				writer.stop = func() { stopped, writer.busy = true, false }
				h.a.transfers[pending.ID] = writer
			}
			h.a.mu.Unlock()
			t.Cleanup(func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				delete(h.a.transfers, pending.ID)
			})
			if err != nil {
				t.Fatal(err)
			}
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit WHERE action='settings.update'")
			s := decode[Settings](t, h.call(true, "PUT", "/api/settings", map[string]any{
				"max_file_bytes": 5, "storage_budget_bytes": 7, "default_link_hours": 24,
			}, nil, 200))
			if !s.Configured || s.MaxFileBytes != 5 || s.StorageBudget != 7 || s.DefaultLinkHours != 24 ||
				s.StoredBytes != 6 || s.ReservedBytes != 10 || s.CleanupErrors != 1 {
				t.Fatalf("incorrect pre-cleanup settings snapshot: %+v", s)
			}
			func() {
				h.a.mu.Lock()
				defer h.a.mu.Unlock()
				afterFile, err := h.a.file(received.ID)
				if err != nil || beforeFile != afterFile || (state == "uploading" && !stopped) {
					t.Fatalf("settings changed received metadata or left an invalid writer running: %+v, %v", afterFile, err)
				}
			}()
			if mutationCount(t, h, "SELECT count(*) FROM audit WHERE action='settings.update'") != beforeAudit+1 ||
				mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=0 AND reserved=0", pending.ID) != 1 ||
				mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='uploading' AND cleanup=0 AND reserved=4", valid.ID) != 1 {
				t.Fatal("settings update did not audit once and isolate cancellation to invalid uploads")
			}
			current := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			s.ReservedBytes, s.CleanupErrors = 4, 0
			if current != s {
				t.Fatalf("cleanup changed more than the quota snapshot: got %+v want %+v", current, s)
			}
			if _, err = os.Stat(h.a.partial(pending.ID)); !os.IsNotExist(err) {
				t.Fatalf("canceled upload bytes remain: %v", err)
			}
			if data, err := os.ReadFile(h.a.completed(received.ID)); err != nil || string(data) != "abcdef" {
				t.Fatalf("settings changed received bytes: %q, %v", data, err)
			}
			h.patch(l, cookie, pending, 2, "cdef", 410)
			h.admit(l, cookie, "settings-new-too-large", 6, 413)
			h.admit(l, cookie, "settings-new-over-budget", 1, 507)
			h.patch(other, otherCookie, valid, 0, "abcd", 204)
		})
	}
}

func TestSettingsCleanupFailureIsDeferred(t *testing.T) {
	for _, failure := range []string{"filesystem", "post-commit reads"} {
		t.Run(failure, func(t *testing.T) {
			h := setup(t)
			_, l, cookie := h.link(nil)
			pending := h.admit(l, cookie, "settings-cleanup-pending", 6, 201)
			h.patch(l, cookie, pending, 0, "ab", 204)
			var fault *mutationReadFault
			if failure == "filesystem" {
				if err := os.Mkdir(h.a.completed(pending.ID), 0700); err != nil {
					t.Fatal(err)
				}
			} else {
				fault = &mutationReadFault{failAfterCommit: true}
				installMutationReadFault(t, h, fault)
			}
			logs := mutationLogs(t)
			beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit WHERE action='settings.update'")
			s := decode[Settings](t, h.call(true, "PUT", "/api/settings", map[string]any{
				"max_file_bytes": 5, "storage_budget_bytes": 4096, "default_link_hours": 24,
			}, nil, 200))
			if s.MaxFileBytes != 5 || s.DefaultLinkHours != 24 || s.ReservedBytes != 6 || s.CleanupErrors != 1 ||
				!strings.Contains(logs.String(), "storage cleanup pending") || !strings.Contains(logs.String(), "settings.update") {
				t.Fatalf("post-commit failure hid settings or pending cleanup: %+v, %s", s, logs.String())
			}
			if fault != nil {
				h.a.mu.Lock()
				commits, rejected := fault.commits, fault.rejected
				fault.failAfterCommit = false
				h.a.mu.Unlock()
				if commits != 1 || rejected != 1 {
					t.Fatalf("expected only a deferred cleanup read failure: commits=%d reads=%d", commits, rejected)
				}
			}
			current := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
			if current != s ||
				mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=1 AND reserved=6", pending.ID) != 1 {
				t.Fatal("cleanup failure lost committed settings, cancellation, or quota charges")
			}
			h.patch(l, cookie, pending, 2, "cdef", 410)
			if failure == "filesystem" {
				if err := os.Remove(h.a.completed(pending.ID)); err != nil {
					t.Fatal(err)
				}
			}
			h.a.mu.Lock()
			err := h.a.sweep()
			h.a.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if mutationCount(t, h, "SELECT count(*) FROM attempts WHERE id=? AND status='canceled' AND cleanup=0 AND reserved=0", pending.ID) != 1 ||
				mutationCount(t, h, "SELECT count(*) FROM audit WHERE action='settings.update'") != beforeAudit+1 {
				t.Fatal("cleanup retry failed or emitted a duplicate settings audit event")
			}
			if _, err = os.Stat(h.a.partial(pending.ID)); !os.IsNotExist(err) {
				t.Fatalf("retried cleanup left upload bytes: %v", err)
			}
		})
	}
}

func TestRenameAuditAtomicityAndMetadata(t *testing.T) {
	h := setup(t)
	c, l, cookie := h.link(nil)
	received := h.admit(l, cookie, "rename-received-file", 3, 201)
	h.patch(l, cookie, received, 0, "abc", 204)
	pending := h.admit(l, cookie, "rename-active-file", 6, 201)
	h.patch(l, cookie, pending, 0, "ab", 204)
	stopped := false
	h.a.mu.Lock()
	beforeFile, e1 := h.a.file(received.ID)
	beforeAttempt, e2 := h.a.attempt(pending.ID)
	h.a.transfers[pending.ID] = &transfer{busy: true, stop: func() { stopped = true }}
	h.a.mu.Unlock()
	t.Cleanup(func() {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		delete(h.a.transfers, pending.ID)
	})
	if err := errors.Join(e1, e2); err != nil {
		t.Fatal(err)
	}
	beforeAudit := mutationCount(t, h, "SELECT count(*) FROM audit")
	mutationExec(t, h, `CREATE TRIGGER fail_rename_audit BEFORE INSERT ON audit
		WHEN NEW.action='file.rename'
		BEGIN SELECT RAISE(ABORT,'simulated audit failure'); END`)
	h.call(true, "PUT", "/api/files/"+received.ID, map[string]string{"name": "renamed.txt"}, nil, 500)
	func() {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		afterFile, err := h.a.file(received.ID)
		if err != nil || beforeFile != afterFile {
			t.Fatalf("audit failure persisted the rename: %+v, %v", afterFile, err)
		}
	}()
	if mutationCount(t, h, "SELECT count(*) FROM audit") != beforeAudit {
		t.Fatal("failed rename emitted an audit event")
	}
	mutationExec(t, h, "DROP TRIGGER fail_rename_audit")
	fault := &mutationReadFault{failAfterCommit: true}
	installMutationReadFault(t, h, fault)
	renamed := decode[File](t, h.call(true, "PUT", "/api/files/"+received.ID, map[string]string{"name": "renamed.txt"}, nil, 200))
	h.a.mu.Lock()
	commits, rejected := fault.commits, fault.rejected
	fault.failAfterCommit = false
	h.a.mu.Unlock()
	if commits != 1 || rejected != 0 {
		t.Fatalf("rename used multiple commits or post-commit reads: commits=%d reads=%d", commits, rejected)
	}
	want := beforeFile
	want.Name = "renamed.txt"
	if renamed != want || renamed.OriginalName != "report.txt" {
		t.Fatalf("rename changed original_name or other metadata: got %+v want %+v", renamed, want)
	}
	stored := decode[struct {
		Items []File `json:"items"`
	}](t, h.call(true, "GET", "/api/containers/"+c.ID+"/files", nil, nil, 200))
	if !reflect.DeepEqual(stored.Items, []File{want}) ||
		mutationCount(t, h, "SELECT count(*) FROM audit WHERE action='file.rename' AND target=?", received.ID) != 1 {
		t.Fatal("successful rename was not durably stored and audited exactly once")
	}
	func() {
		h.a.mu.Lock()
		defer h.a.mu.Unlock()
		afterAttempt, err := h.a.attempt(pending.ID)
		if err != nil || beforeAttempt != afterAttempt || stopped {
			t.Fatalf("rename changed an active upload: %+v, %v", afterAttempt, err)
		}
	}()
	for path, want := range map[string]string{h.a.partial(pending.ID): "ab", h.a.completed(received.ID): "abc"} {
		if data, err := os.ReadFile(path); err != nil || string(data) != want {
			t.Fatalf("rename changed bytes: %q, %v", data, err)
		}
	}
}
