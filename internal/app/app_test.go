package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"upfile/internal/auth"
)

type testVerifier struct{}

func (testVerifier) Verify(_ context.Context, s string) (auth.Principal, error) {
	if s != "admin" {
		return auth.Principal{}, errors.New("invalid")
	}
	return auth.Principal{Subject: "test-admin", Email: "admin@example.invalid"}, nil
}

type harness struct {
	a             *App
	t             *testing.T
	admin, public http.Handler
}

func setup(t *testing.T) *harness {
	t.Helper()
	a, err := New(Config{DataDir: t.TempDir(), Origin: "https://drop.test", HeadroomBytes: 1}, testVerifier{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Error(err)
		}
	})
	h := &harness{a: a, t: t, admin: a.Admin(), public: a.Public()}
	h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": 1024, "storage_budget_bytes": 4096, "default_link_hours": 168}, nil, 200)
	return h
}
func (h *harness) call(admin bool, method, path string, value any, cookie *http.Cookie, want int) *httptest.ResponseRecorder {
	h.t.Helper()
	var b bytes.Buffer
	if value != nil {
		if err := json.NewEncoder(&b).Encode(value); err != nil {
			h.t.Fatal(err)
		}
	}
	origin := "https://drop.test"
	handler := h.public
	if admin {
		handler = h.admin
		path = "/admin" + path
	}
	r := httptest.NewRequest(method, origin+path, &b)
	r.Header.Set("Origin", origin)
	r.Header.Set("X-Upfile-Request", "1")
	r.Header.Set("Content-Type", "application/json")
	if admin {
		r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != want {
		h.t.Fatalf("%s %s: got %d want %d: %s", method, path, w.Code, want, w.Body.String())
	}
	return w
}
func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	return v
}
func (h *harness) link(max *int64) (Container, Link, *http.Cookie) {
	h.t.Helper()
	c := decode[Container](h.t, h.call(true, "POST", "/api/containers", map[string]any{"name": "Request", "instructions": "Send files", "max_file_bytes": max}, nil, 200))
	l := decode[Link](h.t, h.call(true, "POST", "/api/containers/"+c.ID+"/links", map[string]any{"sender_label": "Alice", "expires_at": time.Now().Add(time.Hour).Unix(), "max_file_bytes": nil}, nil, 200))
	secret := strings.Split(l.URL, "#")[1]
	w := h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": secret}, nil, 200)
	cookies := w.Result().Cookies()
	if len(cookies) != 1 {
		h.t.Fatal("missing session cookie")
	}
	return c, l, cookies[0]
}
func (h *harness) admit(l Link, c *http.Cookie, key string, size int64, want int) Attempt {
	h.t.Helper()
	w := h.call(false, "POST", "/api/links/"+l.ID+"/attempts", map[string]any{"key": key, "name": "report.txt", "comment": "A note", "size": size}, c, want)
	if want != 200 && want != 201 {
		return Attempt{}
	}
	return decode[Attempt](h.t, w)
}
func (h *harness) patch(l Link, c *http.Cookie, v Attempt, offset int64, data string, want int) {
	h.t.Helper()
	r := httptest.NewRequest("PATCH", v.UploadURL, strings.NewReader(data))
	r.SetPathValue("link", l.ID)
	r.SetPathValue("attempt", v.ID)
	r.AddCookie(c)
	r.Header.Set("Origin", "https://drop.test")
	r.Header.Set("X-Upfile-Request", "1")
	r.Header.Set("Content-Type", "application/offset+octet-stream")
	r.Header.Set("Tus-Resumable", "1.0.0")
	r.Header.Set("Upload-Offset", fmt.Sprint(offset))
	w := deadlineRecorder{httptest.NewRecorder()}
	h.public.ServeHTTP(w, r)
	if w.Code != want {
		h.t.Fatalf("PATCH %d want %d: %s", w.Code, want, w.Body.String())
	}
}

func TestMultipleFilesReusableLinkAndManagement(t *testing.T) {
	h := setup(t)
	c, l, cookie := h.link(nil)
	for i := 0; i < 2; i++ {
		v := h.admit(l, cookie, fmt.Sprintf("intentional-key-%03d", i), 6, 201)
		retry := h.admit(l, cookie, fmt.Sprintf("intentional-key-%03d", i), 6, 200)
		if retry.ID != v.ID {
			t.Fatal("duplicate admission")
		}
		h.patch(l, cookie, v, 0, "abc", 204)
		h.patch(l, cookie, v, 0, "abc", 409)
		h.patch(l, cookie, v, 3, "def", 204)
		receipt := decode[Attempt](t, h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, cookie, 200))
		if receipt.Status != "completed" {
			t.Fatal(receipt)
		}
		if b, err := os.ReadFile(h.a.completed(v.ID)); err != nil || string(b) != "abcdef" {
			t.Fatalf("content %q %v", b, err)
		}
		h.call(true, "PUT", "/api/files/"+v.ID, map[string]string{"name": "renamed.txt"}, nil, 200)
		w := h.call(true, "GET", "/api/files/"+v.ID+"/download", nil, nil, 200)
		if w.Body.String() != "abcdef" || !strings.Contains(w.Header().Get("Content-Disposition"), "attachment") {
			t.Fatal("unsafe download")
		}
		h.call(true, "DELETE", "/api/files/"+v.ID, nil, nil, 200)
	}
	info := decode[Container](t, h.call(true, "GET", "/api/containers/"+c.ID, nil, nil, 200))
	if info.FileCount != 0 || info.StoredBytes != 0 {
		t.Fatal(info)
	}
	s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.StoredBytes != 0 || s.ReservedBytes != 0 {
		t.Fatal(s)
	}
	h.call(false, "GET", "/api/links/"+l.ID, nil, cookie, 200)
}

func TestLimitsIdempotencyQuotaAndCancellation(t *testing.T) {
	h := setup(t)
	max := int64(6)
	_, l, c := h.link(&max)
	h.admit(l, c, "too-large-key-001", 7, 413)
	v := h.admit(l, c, "same-admission-key", 6, 201)
	h.admit(l, c, "same-admission-key", 5, 409)
	h.admit(l, c, "another-new-key-1", 6, 409)
	h.patch(l, c, v, 0, "ab", 204)
	h.call(false, "POST", "/api/links/"+l.ID+"/attempts/"+v.ID+"/cancel", map[string]any{}, c, 200)
	s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.ReservedBytes != 0 {
		t.Fatal(s)
	}
	h.admit(l, c, "same-admission-key", 6, 410)
	next := h.admit(l, c, "another-new-key-2", 6, 201)
	h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": 5, "storage_budget_bytes": 4096, "default_link_hours": 168}, nil, 200)
	h.patch(l, c, next, 0, "abcdef", 410)
}

func TestSessionsScopeExpiryAndRotation(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	_, l2, c2 := h.link(nil)
	if c.Name == c2.Name || !strings.HasPrefix(c.Name, "__Host-") || c.Path != "/" || !c.Secure || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode {
		t.Fatal("unsafe cookies")
	}
	h.call(false, "GET", "/api/links/"+l.ID, nil, c2, 401)
	h.call(false, "GET", "/api/links/"+l2.ID, nil, c, 401)
	w := h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": strings.Split(l.URL, "#")[1]}, c, 200)
	if len(w.Result().Cookies()) != 0 {
		t.Fatal("re-exchange replaced session")
	}
	v := h.admit(l, c, "my-first-upload-key", 3, 201)
	h.call(false, "GET", "/api/links/"+l2.ID+"/attempts/"+v.ID, nil, c2, 404)
	h.call(true, "POST", "/api/links/"+l.ID+"/rotate", map[string]any{}, nil, 200)
	h.call(false, "GET", "/api/links/"+l.ID, nil, c, 401)
	h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": strings.Split(l.URL, "#")[1]}, nil, 410)
}

func TestLeaseResetCannotTakeActiveAttempt(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "stale-file-key-001", 6, 201)
	h.patch(l, c, v, 0, "abc", 204)
	w := h.call(false, "POST", "/api/links/"+l.ID+"/exchange", map[string]string{"secret": strings.Split(l.URL, "#")[1]}, nil, 200)
	other := w.Result().Cookies()[0]
	h.call(false, "POST", "/api/links/"+l.ID+"/reset", map[string]any{}, other, 409)
	h.a.mu.Lock()
	future := time.Now().Add(6 * time.Minute)
	h.a.now = func() time.Time { return future }
	h.a.mu.Unlock()
	h.call(false, "POST", "/api/links/"+l.ID+"/reset", map[string]any{}, other, 200)
	if _, err := os.Stat(h.a.partial(v.ID)); !os.IsNotExist(err) {
		t.Fatal("stale bytes retained")
	}
	h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, other, 404)
	h.admit(l, other, "different-new-key1", 6, 201)
}

func TestPublicIsolationAndCSRF(t *testing.T) {
	h := setup(t)
	for _, p := range []string{"/api/settings", "/api/containers", "/api/files/fake/download"} {
		h.call(false, "GET", p, nil, nil, 404)
	}
	redirect := h.call(false, "GET", "/admin", nil, nil, http.StatusPermanentRedirect)
	if redirect.Header().Get("Location") != "/admin/" {
		t.Fatal("admin redirect changed")
	}
	h.call(false, "GET", "/admin/api/settings", nil, nil, 404)
	unprefixed := httptest.NewRequest("GET", "https://drop.test/api/settings", nil)
	unprefixed.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, unprefixed)
	if w.Code != 404 {
		t.Fatal("admin listener accepted an unprefixed route")
	}
	for _, malformed := range []string{
		"https://drop.test/admin//api/settings",
		"https://drop.test/admin/./api/settings",
		"https://drop.test/admin/api/../api/settings",
	} {
		r := httptest.NewRequest("GET", malformed, nil)
		r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
		w := httptest.NewRecorder()
		h.admin.ServeHTTP(w, r)
		if w.Code != 404 || w.Header().Get("Location") != "" {
			t.Fatalf("noncanonical admin path escaped: %s: %d %q", malformed, w.Code, w.Header().Get("Location"))
		}
	}
	encoded := httptest.NewRequest("GET", "https://drop.test/admin/api/settings", nil)
	encoded.URL.RawPath = "/%61dmin/api/settings"
	encoded.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	w = httptest.NewRecorder()
	h.admin.ServeHTTP(w, encoded)
	if w.Code != 404 {
		t.Fatal("encoded admin path accepted")
	}
	r := httptest.NewRequest("GET", "https://drop.test/admin/api/settings", nil)
	w = httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("admin auth bypass")
	}
	r = httptest.NewRequest("PUT", "https://drop.test/admin/api/settings", strings.NewReader("{}"))
	r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	w = httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal("CSRF accepted")
	}
	_, l, c := h.link(nil)
	v := h.admit(l, c, "no-public-read-key", 1, 201)
	h.call(false, "GET", "/api/links/"+l.ID+"/uploads/"+v.ID, nil, c, 404)
	h.call(false, "POST", "/api/links/"+l.ID+"/uploads/"+v.ID, map[string]any{}, c, 405)
}

func TestConcurrentAdmissionSameKey(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	var wg sync.WaitGroup
	ids := make(chan string, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			body := `{"key":"concurrent-key-001","name":"x.txt","comment":"","size":100}`
			r := httptest.NewRequest("POST", "https://drop.test/api/links/"+l.ID+"/attempts", strings.NewReader(body))
			r.AddCookie(c)
			r.Header.Set("Origin", "https://drop.test")
			r.Header.Set("X-Upfile-Request", "1")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.public.ServeHTTP(w, r)
			if w.Code != 200 && w.Code != 201 {
				t.Errorf("admission %d %s", w.Code, w.Body.String())
				return
			}
			var a Attempt
			if err := json.Unmarshal(w.Body.Bytes(), &a); err != nil {
				t.Error(err)
				return
			}
			ids <- a.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first != "" && id != first {
			t.Fatal("duplicate upload")
		}
		first = id
	}
	s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.ReservedBytes != 100 {
		t.Fatal(s)
	}
}

func TestReconcileRenameCrash(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "recover-rename-key", 3, 201)
	h.a.mu.Lock()
	if err := os.WriteFile(h.a.partial(v.ID), []byte("abc"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := h.a.db.Exec("UPDATE attempts SET status='finalizing' WHERE id=?", v.ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(h.a.partial(v.ID), h.a.completed(v.ID)); err != nil {
		t.Fatal(err)
	}
	err := h.a.reconcile()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	receipt := decode[Attempt](t, h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, c, 200))
	if receipt.Status != "completed" {
		t.Fatal(receipt)
	}
}

func TestContainerDeletionAndNameValidation(t *testing.T) {
	h := setup(t)
	container, l, c := h.link(nil)
	v := h.admit(l, c, "delete-active-key", 3, 201)
	h.patch(l, c, v, 0, "a", 204)
	h.call(true, "DELETE", "/api/containers/"+container.ID, nil, nil, 200)
	h.call(false, "GET", "/api/links/"+l.ID, nil, c, 410)
	if _, err := os.Stat(filepath.Join(h.a.cfg.DataDir, "partial", v.ID)); !os.IsNotExist(err) {
		t.Fatal("partial not deleted")
	}
	for _, s := range []string{"../x", "a\\b", "x\r\nbad", ".", ""} {
		if validName(s) {
			t.Fatalf("accepted name %q", s)
		}
	}
}
