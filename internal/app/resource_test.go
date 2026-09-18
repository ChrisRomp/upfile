package app

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"testing"
	"time"
)

type deadlineRecorder struct{ *httptest.ResponseRecorder }

func (deadlineRecorder) SetReadDeadline(time.Time) error  { return nil }
func (deadlineRecorder) SetWriteDeadline(time.Time) error { return nil }
func (deadlineRecorder) EnableFullDuplex() error          { return nil }

func TestGlobalQuotaAcrossLinksAndZeroByteFile(t *testing.T) {
	h := setup(t)
	h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": 100, "storage_budget_bytes": 150, "default_link_hours": 168}, nil, 200)
	_, l, c := h.link(nil)
	_, l2, c2 := h.link(nil)
	v := h.admit(l, c, "quota-first-file01", 100, 201)
	h.admit(l2, c2, "quota-second-file1", 100, 507)
	h.call(false, "POST", "/api/links/"+l.ID+"/attempts/"+v.ID+"/cancel", map[string]any{}, c, 200)
	empty := h.admit(l2, c2, "empty-file-key001", 0, 201)
	if empty.Status != "completed" {
		t.Fatal("zero-byte file not finalized")
	}
	h.admit(l2, c2, "quota-second-file1", 100, 201)
}

func TestCleanupFailureRetainsReservation(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "cleanup-key-00001", 6, 201)
	// Replacing an expected regular file by a directory simulates an unsafe cleanup artifact.
	if err := os.Remove(h.a.partial(v.ID) + ".info"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(h.a.partial(v.ID)+".info", 0700); err != nil {
		t.Fatal(err)
	}
	h.call(false, "POST", "/api/links/"+l.ID+"/attempts/"+v.ID+"/cancel", map[string]any{}, c, 500)
	s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.ReservedBytes != 6 {
		t.Fatal("quota released before cleanup", s)
	}
	if err := os.Remove(h.a.partial(v.ID) + ".info"); err != nil {
		t.Fatal(err)
	}
	h.a.mu.Lock()
	err := h.a.sweepCleanup()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
}

func TestExpiredLinksAndSessions(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "expiry-test-key01", 4, 201)
	h.a.mu.Lock()
	_, err := h.a.db.Exec("UPDATE links SET expires=? WHERE id=?", time.Now().Add(-time.Second).Unix(), l.ID)
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.patch(l, c, v, 0, "abcd", 410)
	h.call(false, "GET", "/api/links/"+l.ID, nil, c, 410)
}

func TestChunkCeilingAndUnsupportedExtensions(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "chunk-headers-key", 4, 201)
	h.patch(l, c, v, 0, "abcde", 413)
	for _, header := range []string{"Upload-Concat", "Upload-Metadata", "Upload-Defer-Length", "X-HTTP-Method-Override"} {
		r := httptest.NewRequest("PATCH", v.UploadURL, bytes.NewReader([]byte("abcd")))
		r.AddCookie(c)
		r.Header.Set("Origin", "https://drop.test")
		r.Header.Set("X-Upfile-Request", "1")
		r.Header.Set("Tus-Resumable", "1.0.0")
		r.Header.Set("Content-Type", "application/offset+octet-stream")
		r.Header.Set("Upload-Offset", "0")
		r.Header.Set(header, "1")
		w := deadlineRecorder{httptest.NewRecorder()}
		h.public.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%s accepted: %d", header, w.Code)
		}
	}
	// Known length above the configured chunk ceiling is rejected before reading bytes.
	r := httptest.NewRequest("PATCH", v.UploadURL, bytes.NewReader(nil))
	r.ContentLength = h.a.cfg.ChunkBytes + 1
	r.AddCookie(c)
	r.Header.Set("Origin", "https://drop.test")
	r.Header.Set("X-Upfile-Request", "1")
	r.Header.Set("Tus-Resumable", "1.0.0")
	r.Header.Set("Content-Type", "application/offset+octet-stream")
	r.Header.Set("Upload-Offset", "0")
	w := httptest.NewRecorder()
	h.public.ServeHTTP(w, r)
	if w.Code != 413 {
		t.Fatalf("oversized chunk accepted: %d", w.Code)
	}
}

func TestAllocationRecoveryWithInterruptedMetadata(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "allocation-crash1", 3, 201)
	h.a.mu.Lock()
	_, err := h.a.db.Exec("UPDATE attempts SET status='allocating' WHERE id=?", v.ID)
	if err == nil {
		err = os.WriteFile(h.a.partial(v.ID)+".info", []byte("{"), 0600)
	}
	if err == nil {
		err = h.a.reconcile()
	}
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.patch(l, c, v, 0, "abc", 204)
}

func TestTransientFinalizeFailureCanRetryWithoutRestart(t *testing.T) {
	h := setup(t)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "retry-finalize-key", 3, 201)
	h.a.mu.Lock()
	h.a.rename = func(string, string) error { return errors.New("simulated transient rename failure") }
	h.a.mu.Unlock()
	h.patch(l, c, v, 0, "abc", 500)
	h.a.mu.Lock()
	h.a.rename = os.Rename
	h.a.mu.Unlock()
	receipt := decode[Attempt](t, h.call(false, "GET", "/api/links/"+l.ID+"/attempts/"+v.ID, nil, c, 200))
	if receipt.Status != "completed" {
		t.Fatal("finalizing attempt was stranded")
	}
}

func TestDeletingContainerCleansAbandonedUploads(t *testing.T) {
	h := setup(t)
	container, l, c := h.link(nil)
	v := h.admit(l, c, "abandoned-delete1", 3, 201)
	h.patch(l, c, v, 0, "a", 204)
	h.a.mu.Lock()
	future := time.Now().Add(6 * time.Minute)
	h.a.now = func() time.Time { return future }
	err := h.a.sweep()
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.call(true, "DELETE", "/api/containers/"+container.ID, nil, nil, 200)
	if _, err = os.Stat(h.a.partial(v.ID)); !os.IsNotExist(err) {
		t.Fatal("abandoned partial survived container deletion")
	}
}

func TestLargeStreamingUpload(t *testing.T) {
	if os.Getenv("UPFILE_LARGE_TEST") != "1" {
		t.Skip("set UPFILE_LARGE_TEST=1 for the 3 GiB streaming check")
	}
	h := setup(t)
	const size int64 = 3 << 30
	h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": size, "storage_budget_bytes": size + (1 << 20), "default_link_hours": 168}, nil, 200)
	_, l, c := h.link(nil)
	v := h.admit(l, c, "large-stream-key1", size, 201)
	chunk := make([]byte, h.a.cfg.ChunkBytes)
	for i := range chunk {
		chunk[i] = byte(i % 251)
	}
	expected := sha256.New()
	var peak uint64
	for offset := int64(0); offset < size; offset += int64(len(chunk)) {
		r := httptest.NewRequest("PATCH", v.UploadURL, bytes.NewReader(chunk))
		r.AddCookie(c)
		r.Header.Set("Origin", "https://drop.test")
		r.Header.Set("X-Upfile-Request", "1")
		r.Header.Set("Content-Type", "application/offset+octet-stream")
		r.Header.Set("Tus-Resumable", "1.0.0")
		r.Header.Set("Upload-Offset", fmt.Sprint(offset))
		w := deadlineRecorder{httptest.NewRecorder()}
		h.public.ServeHTTP(w, r)
		if w.Code != http.StatusNoContent {
			t.Fatalf("offset %d: %d %s", offset, w.Code, w.Body.String())
		}
		expected.Write(chunk)
		var m runtime.MemStats
		runtime.ReadMemStats(&m)
		if m.HeapAlloc > peak {
			peak = m.HeapAlloc
		}
	}
	if peak > 128<<20 {
		t.Fatalf("peak allocated heap %d exceeds 128 MiB bound", peak)
	}
	f, err := os.Open(h.a.completed(v.ID))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	actual := sha256.New()
	n, err := io.Copy(actual, f)
	if err != nil {
		t.Fatal(err)
	}
	if n != size || !bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
		t.Fatal("large upload integrity mismatch")
	}
	t.Logf("3 GiB streamed; peak Go heap %.1f MiB; SHA256 %s", float64(peak)/(1<<20), hex.EncodeToString(actual.Sum(nil)))
}
