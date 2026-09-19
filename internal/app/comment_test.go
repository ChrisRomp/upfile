package app

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

func commentPath(l Link, v Attempt) string {
	return "/api/links/" + l.ID + "/attempts/" + v.ID + "/comment"
}

func editComment(t *testing.T, h *harness, l Link, cookie *http.Cookie, v Attempt, comment string) {
	t.Helper()
	got := decode[map[string]string](t, h.call(false, "PUT", commentPath(l, v), map[string]string{"comment": comment}, cookie, 200))
	if !reflect.DeepEqual(got, map[string]string{"comment": comment}) {
		t.Fatalf("unexpected comment response: %v", got)
	}
}

func commentAttempt(t *testing.T, h *harness, id string) Attempt {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	v, err := h.a.attempt(id)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func assertComment(t *testing.T, h *harness, id, comment, admission string) {
	t.Helper()
	v := commentAttempt(t, h, id)
	if v.Comment != comment || v.AdmissionComment != admission {
		t.Fatalf("incorrect attempt comments: current=%q admission=%q", v.Comment, v.AdmissionComment)
	}
	if v.Status == "completed" {
		h.a.mu.Lock()
		f, err := h.a.file(id)
		h.a.mu.Unlock()
		if err != nil || f.Comment != comment {
			t.Fatalf("incorrect completed comment: %+v, %v", f, err)
		}
	}
}

type commentEditingBody struct {
	io.Reader
	edit func()
}

func (b *commentEditingBody) Read(p []byte) (int, error) {
	if b.edit != nil {
		edit := b.edit
		b.edit = nil
		edit()
	}
	return b.Reader.Read(p)
}

func (*commentEditingBody) Close() error { return nil }

func TestAttemptCommentBeforeDuringAndAfterUpload(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	key := "comment-lifecycle-upload"
	v := h.admit(l, cookie, key, 6, 201)
	before := commentAttempt(t, h, v.ID)
	editComment(t, h, l, cookie, v, "Before upload")
	after := commentAttempt(t, h, v.ID)
	before.Comment = "Before upload"
	if before != after {
		t.Fatal("comment edit changed upload metadata or its inactivity lease")
	}
	assertComment(t, h, v.ID, "Before upload", "A note")
	if retry := h.admit(l, cookie, key, 6, 200); retry.ID != v.ID {
		t.Fatal("comment edit broke original admission retry")
	}
	h.call(false, "POST", "/api/links/"+l.ID+"/attempts", map[string]any{
		"key": key, "name": "report.txt", "comment": "Before upload", "size": 6,
	}, cookie, 409)
	h.patch(l, cookie, v, 0, "ab", 204)
	editComment(t, h, l, cookie, v, "")
	assertComment(t, h, v.ID, "", "A note")

	r := httptest.NewRequest("PATCH", v.UploadURL, nil)
	r.Body = &commentEditingBody{Reader: strings.NewReader("cdef"), edit: func() {
		h.a.mu.Lock()
		transfer := h.a.transfers[v.ID]
		active := transfer != nil && transfer.busy
		var last time.Time
		if active {
			last = transfer.last
		}
		h.a.mu.Unlock()
		if !active {
			t.Fatal("comment was not edited during an active request")
		}
		before := commentAttempt(t, h, v.ID)
		editComment(t, h, l, cookie, v, "During upload\n私のコメント")
		after := commentAttempt(t, h, v.ID)
		before.Comment = after.Comment
		h.a.mu.Lock()
		unchanged := h.a.transfers[v.ID] == transfer && transfer.busy && transfer.last.Equal(last)
		h.a.mu.Unlock()
		if before != after || !unchanged {
			t.Fatal("editing during transfer changed upload state or renewed its lease")
		}
	}}
	r.ContentLength = 4
	r.AddCookie(cookie)
	for k, value := range map[string]string{
		"Origin": "https://drop.test", "X-Upfile-Request": "1", "Tus-Resumable": "1.0.0",
		"Upload-Offset": "2", "Content-Type": "application/offset+octet-stream",
	} {
		r.Header.Set(k, value)
	}
	w := deadlineRecorder{httptest.NewRecorder()}
	h.public.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("upload failed during comment edit: %d %s", w.Code, w.Body.String())
	}
	assertComment(t, h, v.ID, "During upload\n私のコメント", "A note")
	if got := commentAttempt(t, h, v.ID); got.Status != "completed" {
		t.Fatalf("upload did not complete: %+v", got)
	}
	h.call(true, "PUT", "/api/files/"+v.ID, map[string]string{"name": "renamed.txt"}, nil, 200)
	for _, comment := range []string{"After completion", "", "Final comment"} {
		editComment(t, h, l, cookie, v, comment)
		assertComment(t, h, v.ID, comment, "A note")
		if retry := h.admit(l, cookie, key, 6, 200); retry.ID != v.ID || retry.Status != "completed" {
			t.Fatal("completed admission retry changed")
		}
	}
	if mutationCount(t, h, "SELECT count(*) FROM files WHERE id=? AND name='renamed.txt' AND original_name='report.txt' AND size=6", v.ID) != 1 {
		t.Fatal("comment edit overwrote file metadata")
	}
	if data, err := os.ReadFile(h.a.completed(v.ID)); err != nil || string(data) != "abcdef" {
		t.Fatalf("comment edit changed file bytes: %q, %v", data, err)
	}
	if mutationCount(t, h, "SELECT count(*) FROM audit WHERE actor='uploader' AND action='file.comment' AND target=?", v.ID) != 6 {
		t.Fatal("comment edits were not audited")
	}
	audit := h.call(true, "GET", "/api/audit", nil, nil, 200).Body.String()
	for _, secret := range []string{"Final comment", "A note", "Before upload", "During upload", "After completion"} {
		if strings.Contains(audit, secret) {
			t.Fatal("audit disclosed raw comment content")
		}
	}
}

func TestAttemptCommentZeroByteAndEmptyAdmission(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	input := map[string]any{"key": "empty-comment-zero-byte", "name": "empty.txt", "comment": "", "size": 0}
	v := decode[Attempt](t, h.call(false, "POST", "/api/links/"+l.ID+"/attempts", input, cookie, 201))
	if v.Status != "completed" {
		t.Fatal("zero-byte upload did not complete immediately")
	}
	for _, comment := range []string{"Added after zero-byte completion", "", ""} {
		editComment(t, h, l, cookie, v, comment)
		assertComment(t, h, v.ID, comment, "")
		retry := decode[Attempt](t, h.call(false, "POST", "/api/links/"+l.ID+"/attempts", input, cookie, 200))
		if retry.ID != v.ID {
			t.Fatal("zero-byte retry created another file")
		}
	}
	if mutationCount(t, h, "SELECT count(*) FROM files WHERE id=? AND size=0", v.ID) != 1 {
		t.Fatal("comment edits changed zero-byte file")
	}
}

func TestAttemptCommentWhileAllocatingOrFinalizing(t *testing.T) {
	for _, status := range []string{"allocating", "finalizing"} {
		t.Run(status, func(t *testing.T) {
			h := setup(t)
			_, l, cookie := h.link(nil)
			v := h.admit(l, cookie, "comment-recovery-"+status, 3, 201)
			if status == "allocating" {
				mutationExec(t, h, "UPDATE attempts SET status='allocating' WHERE id=?", v.ID)
			} else {
				h.a.mu.Lock()
				h.a.rename = func(string, string) error { return errors.New("simulated finalization failure") }
				h.a.mu.Unlock()
				h.patch(l, cookie, v, 0, "abc", 500)
			}
			if got := commentAttempt(t, h, v.ID); got.Status != status {
				t.Fatalf("unexpected upload state: %s", got.Status)
			}
			editComment(t, h, l, cookie, v, "Edited while "+status)
			assertComment(t, h, v.ID, "Edited while "+status, "A note")
			if status == "allocating" {
				h.admit(l, cookie, "comment-recovery-"+status, 3, 200)
				h.patch(l, cookie, v, 0, "abc", 204)
			} else {
				h.a.mu.Lock()
				h.a.rename = os.Rename
				h.a.mu.Unlock()
				h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, cookie, 200)
			}
			assertComment(t, h, v.ID, "Edited while "+status, "A note")
		})
	}
}

func TestAttemptCommentValidation(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "comment-validation-key", 3, 201)
	for _, comment := range []string{"", " \t\r\n ", strings.Repeat("a", 2048), strings.Repeat("🙂", 512)} {
		editComment(t, h, l, cookie, v, comment)
		assertComment(t, h, v.ID, comment, "A note")
	}
	for _, input := range []any{
		map[string]any{}, map[string]any{"comment": nil}, map[string]any{"comment": 42},
		map[string]any{"comment": false}, map[string]any{"comment": []string{"bad"}},
		map[string]any{"comment": "fine", "extra": true},
		map[string]any{"comment": strings.Repeat("a", 2049)},
		map[string]any{"comment": strings.Repeat("🙂", 513)},
		map[string]any{"comment": "forbidden\x00control"}, map[string]any{"comment": "\b"},
		[]any{}, nil,
	} {
		before := commentAttempt(t, h, v.ID)
		audits := mutationCount(t, h, "SELECT count(*) FROM audit")
		h.call(false, "PUT", commentPath(l, v), input, cookie, 400)
		if commentAttempt(t, h, v.ID) != before || mutationCount(t, h, "SELECT count(*) FROM audit") != audits {
			t.Fatalf("invalid input changed metadata or audit: %v", input)
		}
	}
}

func TestAttemptCommentOwnershipAndValidity(t *testing.T) {
	for _, kind := range []string{
		"no cookie", "wrong cookie", "same link different session", "cross link", "invalid attempt", "missing attempt",
		"expired session", "deleted session", "expired link", "revoked link", "deleted container",
	} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			container, l, cookie := h.link(nil)
			v := h.admit(l, cookie, "comment-authentication", 0, 201)
			path, want := commentPath(l, v), 401
			switch kind {
			case "no cookie":
				cookie = nil
			case "wrong cookie":
				cookie = &http.Cookie{Name: cookie.Name, Value: opaque(32)}
			case "same link different session":
				w := h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": strings.Split(l.URL, "#")[1]}, nil, 200)
				cookie, want = w.Result().Cookies()[0], 404
			case "cross link":
				_, other, otherCookie := h.link(nil)
				path, cookie, want = commentPath(other, v), otherCookie, 404
			case "invalid attempt":
				path, want = commentPath(l, Attempt{ID: "invalid"}), 404
			case "missing attempt":
				path, want = commentPath(l, Attempt{ID: opaque(16)}), 404
			case "expired session":
				mutationExec(t, h, "UPDATE sessions SET expires=0 WHERE hash=?", digest(cookie.Value))
			case "deleted session":
				mutationExec(t, h, "DELETE FROM sessions WHERE hash=?", digest(cookie.Value))
			case "expired link":
				mutationExec(t, h, "UPDATE links SET expires=0 WHERE id=?", l.ID)
				want = 410
			case "revoked link":
				mutationExec(t, h, "UPDATE links SET revoked=1 WHERE id=?", l.ID)
				want = 410
			case "deleted container":
				mutationExec(t, h, "UPDATE containers SET status='deleted' WHERE id=?", container.ID)
				want = 410
			}
			before := commentAttempt(t, h, v.ID)
			audits := mutationCount(t, h, "SELECT count(*) FROM audit")
			h.call(false, "PUT", path, map[string]string{"comment": "Unauthorized edit"}, cookie, want)
			if commentAttempt(t, h, v.ID) != before || mutationCount(t, h, "SELECT count(*) FROM audit") != audits {
				t.Fatal("unauthorized request changed upload metadata or audit")
			}
			assertComment(t, h, v.ID, "A note", "A note")
		})
	}
}

func TestAttemptCommentRequiresSameOrigin(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "comment-origin-check", 0, 201)
	r := httptest.NewRequest("PUT", "https://drop.test"+commentPath(l, v), strings.NewReader(`{"comment":"Cross-origin"}`))
	r.AddCookie(cookie)
	r.Header.Set("Origin", "https://other.test")
	r.Header.Set("X-Upfile-Request", "1")
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.public.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatalf("cross-origin comment accepted: %d %s", w.Code, w.Body.String())
	}
	assertComment(t, h, v.ID, "A note", "A note")
}

func TestAttemptCommentRejectsEndedAttemptsAndUnavailableFiles(t *testing.T) {
	for _, kind := range []string{"canceled", "abandoned", "deleting", "deleted", "missing file"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			_, l, cookie := h.link(nil)
			size := int64(0)
			if kind == "canceled" || kind == "abandoned" {
				size = 3
			}
			v := h.admit(l, cookie, "comment-unavailable-key", size, 201)
			switch kind {
			case "canceled":
				h.call(false, "POST", "/api/links/"+l.ID+"/attempts/"+v.ID+"/cancel", map[string]any{}, cookie, 200)
			case "abandoned":
				h.a.mu.Lock()
				err := h.a.cancel(v.ID, "abandoned")
				h.a.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
			case "deleting":
				mutationExec(t, h, "UPDATE files SET status='deleting' WHERE id=?", v.ID)
			case "deleted":
				h.call(true, "DELETE", "/api/files/"+v.ID, nil, nil, 200)
			case "missing file":
				mutationExec(t, h, "DELETE FROM files WHERE id=?", v.ID)
			}
			before := commentAttempt(t, h, v.ID)
			audits := mutationCount(t, h, "SELECT count(*) FROM audit")
			h.call(false, "PUT", commentPath(l, v), map[string]string{"comment": "Must not persist"}, cookie, 410)
			if commentAttempt(t, h, v.ID) != before || mutationCount(t, h, "SELECT count(*) FROM audit") != audits {
				t.Fatal("rejected comment changed metadata or audit")
			}
			if mutationCount(t, h, "SELECT count(*) FROM files WHERE id=? AND comment!='A note'", v.ID) != 0 {
				t.Fatal("rejected comment changed unavailable file")
			}
		})
	}
}

func TestAttemptCommentDoesNotRenewInactivityLease(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "comment-no-lease-renewal", 3, 201)
	before := commentAttempt(t, h, v.ID)
	h.a.mu.Lock()
	now := time.Unix(before.Last, 0).Add(h.a.cfg.Lease - time.Second)
	h.a.now = func() time.Time { return now }
	h.a.mu.Unlock()
	editComment(t, h, l, cookie, v, "Almost idle")
	h.a.mu.Lock()
	now = now.Add(2 * time.Second)
	err := h.a.sweep()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if got := commentAttempt(t, h, v.ID); got.Status != "abandoned" || got.Last != before.Last {
		t.Fatalf("comment kept idle upload alive: %+v", got)
	}
	h.call(false, "PUT", commentPath(l, v), map[string]string{"comment": "Too late"}, cookie, 410)
}

func TestAttemptCommentMutationFailuresRollBack(t *testing.T) {
	for _, completed := range []bool{false, true} {
		for _, failure := range []string{"attempt update", "file update", "audit insert", "audit prune"} {
			if !completed && failure == "file update" {
				continue
			}
			t.Run(map[bool]string{false: "uploading", true: "completed"}[completed]+"/"+failure, func(t *testing.T) {
				h := setup(t)
				_, l, cookie := h.link(nil)
				v := h.admit(l, cookie, "comment-rollback-upload", 3, 201)
				if completed {
					h.patch(l, cookie, v, 0, "abc", 204)
				}
				trigger := map[string]string{
					"attempt update": "BEFORE UPDATE OF comment ON attempts",
					"file update":    "BEFORE UPDATE OF comment ON files",
					"audit insert":   "BEFORE INSERT ON audit WHEN NEW.action='file.comment'",
					"audit prune":    "BEFORE DELETE ON audit",
				}[failure]
				if failure == "audit prune" {
					mutationExec(t, h, "INSERT INTO audit(id,actor,action,target,created) VALUES(20000,'test','test','test',0)")
				}
				mutationExec(t, h, "CREATE TRIGGER fail_comment_mutation "+trigger+" BEGIN SELECT RAISE(ABORT,'simulated comment mutation failure'); END")
				before := commentAttempt(t, h, v.ID)
				audits := mutationCount(t, h, "SELECT count(*) FROM audit")
				h.call(false, "PUT", commentPath(l, v), map[string]string{"comment": "Must roll back"}, cookie, 500)
				if commentAttempt(t, h, v.ID) != before || mutationCount(t, h, "SELECT count(*) FROM audit") != audits {
					t.Fatal("failed mutation changed metadata or audit")
				}
				assertComment(t, h, v.ID, "A note", "A note")
				mutationExec(t, h, "DROP TRIGGER fail_comment_mutation")
				editComment(t, h, l, cookie, v, "Retry succeeds")
				assertComment(t, h, v.ID, "Retry succeeds", "A note")
			})
		}
	}
}

func TestAttemptCommentDoesNotReadAfterCommit(t *testing.T) {
	h := setup(t)
	_, l, cookie := h.link(nil)
	v := h.admit(l, cookie, "comment-response-prepared", 0, 201)
	fault := &mutationReadFault{failAfterCommit: true}
	installMutationReadFault(t, h, fault)
	editComment(t, h, l, cookie, v, "Prepared response")
	h.a.mu.Lock()
	commits, rejected := fault.commits, fault.rejected
	fault.failAfterCommit = false
	h.a.mu.Unlock()
	if commits != 1 || rejected != 0 {
		t.Fatalf("comment response performed post-commit reads: commits=%d rejected=%d", commits, rejected)
	}
	assertComment(t, h, v.ID, "Prepared response", "A note")
}
