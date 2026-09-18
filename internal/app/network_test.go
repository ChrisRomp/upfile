package app

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

type advancingBody struct {
	a         *App
	remaining int
	now       time.Time
}

func (b *advancingBody) Read(p []byte) (int, error) {
	if b.remaining == 0 {
		return 0, io.EOF
	}
	b.a.mu.Lock()
	b.now = b.now.Add(2 * time.Minute)
	now := b.now
	b.a.now = func() time.Time { return now }
	b.a.mu.Unlock()
	p[0] = 'a'
	b.remaining--
	return 1, nil
}
func (*advancingBody) Close() error { return nil }

func TestProgressingStreamRenewsLeaseBeforeChunkCompletes(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "slow-stream-key01", 6, 201)
	r := httptest.NewRequest("PATCH", v.UploadURL, nil)
	r.Body = &advancingBody{a: h.a, remaining: 6, now: time.Now()}
	r.ContentLength = 6
	r.AddCookie(c)
	for k, v := range map[string]string{"Origin": "https://drop.test", "X-Upfile-Request": "1", "Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"} {
		r.Header.Set(k, v)
	}
	w := deadlineRecorder{httptest.NewRecorder()}
	h.public.ServeHTTP(w, r)
	if w.Code != 204 {
		t.Fatalf("slow progressing chunk: %d %s", w.Code, w.Body.String())
	}
	receipt := decode[Attempt](t, h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, c, 200))
	if receipt.Status != "completed" {
		t.Fatal("lease expired during a progressing chunk")
	}
}

type blockedResponse struct {
	*httptest.ResponseRecorder
	entered, release chan struct{}
	once             sync.Once
}

func (w *blockedResponse) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release
	return w.ResponseRecorder.Write(b)
}

func TestSlowJSONResponseDoesNotHoldApplicationLock(t *testing.T) {
	h := setup(t)
	w := &blockedResponse{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	defer close(w.release)
	r := httptest.NewRequest("GET", "https://admin.test/api/settings", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	go h.admin.ServeHTTP(w, r)
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("response did not start")
	}
	done := make(chan struct{})
	go func() { h.a.mu.Lock(); defer h.a.mu.Unlock(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("network write held application mutex")
	}
}

func TestUnexpectedGETBodyRejected(t *testing.T) {
	h := setup(t)
	r := httptest.NewRequest("GET", "https://admin.test/api/settings", strings.NewReader("unwanted"))
	r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatalf("GET body accepted: %d", w.Code)
	}
}

func TestRevocationStopsStalledNetworkWriter(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "network-stall-key", 100, 201)
	handlerDone := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(handlerDone)
		h.public.ServeHTTP(w, r)
	}))
	defer server.Close()
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, "PATCH", server.URL+"/api/links/"+l.ID+"/uploads/"+v.ID, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = 100
	request.AddCookie(c)
	for k, v := range map[string]string{"Origin": "https://drop.test", "X-Upfile-Request": "1", "Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"} {
		request.Header.Set(k, v)
	}
	result := make(chan error, 1)
	go func() {
		r, e := server.Client().Do(request)
		if e == nil {
			_, _ = io.Copy(io.Discard, r.Body)
			r.Body.Close()
		}
		result <- e
	}()
	if _, err = writer.Write([]byte("a")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		h.a.mu.Lock()
		active := h.a.transfers[v.ID] != nil
		h.a.mu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		r := httptest.NewRequest("POST", "https://admin.test/api/links/"+l.ID+"/revoke", strings.NewReader("{}"))
		r.Header.Set("Origin", "https://admin.test")
		r.Header.Set("X-Upfile-Request", "1")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
		w := httptest.NewRecorder()
		h.admin.ServeHTTP(w, r)
		finished <- w
	}()
	select {
	case w := <-finished:
		if w.Code != 200 {
			t.Fatalf("revoke: %d %s", w.Code, w.Body.String())
		}
	case <-time.After(3 * time.Second):
		writer.Close()
		t.Fatal("revocation blocked behind a network read")
	}
	writer.Close()
	select {
	case <-result:
	case <-time.After(3 * time.Second):
		t.Fatal("writer did not stop")
	}
	select {
	case <-handlerDone:
	case <-time.After(3 * time.Second):
		t.Fatal("server writer did not finish")
	}
	h.a.mu.Lock()
	err = h.a.sweepCleanup()
	s, e := h.a.settings()
	h.a.mu.Unlock()
	if err != nil || e != nil || s.ReservedBytes != 0 {
		t.Fatalf("cleanup: %v %v %+v", err, e, s)
	}
}

func TestHTTPRangeDownload(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "range-download-key", 6, 201)
	h.patch(l, c, v, 0, "abcdef", 204)
	r := httptest.NewRequest("GET", "https://admin.test/api/files/"+v.ID+"/download", nil)
	r.Header.Set("Cf-Access-Jwt-Assertion", "admin")
	r.Header.Set("Range", "bytes=2-4")
	w := httptest.NewRecorder()
	h.admin.ServeHTTP(w, r)
	if w.Code != 206 || w.Body.String() != "cde" {
		t.Fatalf("range: %d %q", w.Code, w.Body.String())
	}
	if w.Header().Get("Content-Type") != "application/octet-stream" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("unsafe download headers")
	}
}

func TestDeletingFileWaitsForDownloadHandle(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "delete-reader-key", 3, 201)
	h.patch(l, c, v, 0, "abc", 204)
	// A download that is still unwinding keeps its storage charge until its handle is released.
	h.a.mu.Lock()
	body, err := os.Open(h.a.completed(v.ID))
	if err != nil {
		h.a.mu.Unlock()
		t.Fatal(err)
	}
	h.a.downloads["held"] = &download{file: v.ID, container: l.ContainerID, body: body}
	h.a.mu.Unlock()
	w := h.call(true, "DELETE", "/api/files/"+v.ID, nil, nil, 200)
	var status map[string]string
	if err = json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if status["status"] != "deleting" {
		t.Fatal("premature deletion success")
	}
	h.a.mu.Lock()
	delete(h.a.downloads, "held")
	err = h.a.sweepCleanup()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}
