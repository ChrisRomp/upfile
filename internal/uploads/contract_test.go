package uploads

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tus/tusd/v2/pkg/filestore"
	tus "github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
)

// Admission owns creation; tus only resumes a resource with a stable ID.
func TestProgrammaticCreationAndResume(t *testing.T) {
	dir := t.TempDir()
	store := filestore.New(dir)
	composer := tus.NewStoreComposer()
	composer.UseCore(store)
	composer.UseLocker(memorylocker.New())
	finished := 0
	h, err := tus.NewUnroutedHandler(tus.Config{
		StoreComposer: composer, DisableDownload: true, DisableTermination: true,
		DisableConcatenation: true,
		PreFinishResponseCallback: func(tus.HookEvent) (tus.HTTPResponse, error) {
			finished++
			return tus.HTTPResponse{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.NewUpload(context.Background(), tus.FileInfo{ID: "fixed-id", Size: 6})
	if err != nil {
		t.Fatal(err)
	}
	router := h.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "HEAD":
			h.HeadFile(w, r)
		case "PATCH":
			h.PatchFile(w, r)
		default:
			http.Error(w, "not allowed", http.StatusMethodNotAllowed)
		}
	}))
	request := func(method, offset, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/fixed-id", strings.NewReader(body))
		r.Header.Set("Tus-Resumable", "1.0.0")
		r.Header.Set("Upload-Offset", offset)
		r.Header.Set("Content-Type", "application/offset+octet-stream")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	if w := request("HEAD", "", ""); w.Code != 200 || w.Header().Get("Upload-Offset") != "0" {
		t.Fatalf("initial HEAD: %d %s", w.Code, w.Body.String())
	}
	if w := request("PATCH", "0", "abc"); w.Code != 204 {
		t.Fatalf("first PATCH: %d %s", w.Code, w.Body.String())
	}
	if w := request("HEAD", "", ""); w.Header().Get("Upload-Offset") != "3" {
		t.Fatal("offset did not persist")
	}
	if w := request("PATCH", "0", "abc"); w.Code != 409 {
		t.Fatal("stale offset accepted")
	}
	if w := request("PATCH", "3", "def"); w.Code != 204 || finished != 1 {
		t.Fatalf("finish: %d %s notifications=%d", w.Code, w.Body.String(), finished)
	}
	if w := request("HEAD", "", ""); w.Header().Get("Upload-Offset") != "6" {
		t.Fatal("completed offset missing")
	}
	for _, method := range []string{"GET", "POST", "DELETE"} {
		if w := request(method, "", ""); w.Code != 405 {
			t.Fatalf("%s accepted", method)
		}
	}
	bytes, err := os.ReadFile(filepath.Join(dir, "fixed-id"))
	if err != nil || string(bytes) != "abcdef" {
		t.Fatalf("stored content %q: %v", bytes, err)
	}
}
