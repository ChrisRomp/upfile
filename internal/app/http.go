package app

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Warn("response could not be written", "error", err)
	}
}

func (a *App) fail(w http.ResponseWriter, err error) {
	status, code, msg := classify(err)
	if status == 500 {
		slog.Error("operation failed", "error", err)
	}
	if status == 429 {
		w.Header().Set("Retry-After", "60")
	}
	writeJSON(w, status, map[string]string{"code": code, "error": msg})
}

type operation func(http.ResponseWriter, *http.Request) error

func (a *App) endpoint(fn operation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		out := &bufferedResponse{header: make(http.Header), underlying: w}
		a.mu.Lock()
		if err := fn(out, r); err != nil {
			a.fail(out, err)
		}
		a.mu.Unlock()
		for key, values := range out.header {
			w.Header()[key] = values
		}
		_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
		if out.code == 0 {
			out.code = 200
		}
		w.WriteHeader(out.code)
		if out.body.Len() > 0 {
			if _, err := w.Write(out.body.Bytes()); err != nil {
				slog.Warn("API response interrupted", "error", err)
			}
		}
	}
}

func readJSON(w http.ResponseWriter, r *http.Request, v any) error {
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		return invalid("A JSON request is required.")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("Invalid or oversized JSON request.")
	}
	if err := d.Decode(new(any)); !errors.Is(err, io.EOF) {
		return invalid("Only one JSON object is allowed.")
	}
	return nil
}

func page(r *http.Request) (limit, offset int, err error) {
	limit = 25
	p := 1
	if s := r.URL.Query().Get("limit"); s != "" {
		limit, err = strconv.Atoi(s)
		if err != nil {
			return 0, 0, invalid("Invalid page size.")
		}
	}
	if s := r.URL.Query().Get("page"); s != "" {
		p, err = strconv.Atoi(s)
		if err != nil {
			return 0, 0, invalid("Invalid page number.")
		}
	}
	if limit < 1 || limit > 100 || p < 1 || p > 100000 {
		return 0, 0, invalid("Invalid pagination.")
	}
	return limit, (p - 1) * limit, nil
}
func collection(w http.ResponseWriter, items any, total int) {
	writeJSON(w, 200, map[string]any{"items": items, "total": total})
}

func (a *App) static(admin bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" && r.Method != "HEAD" {
			http.Error(w, "method not allowed", 405)
			return
		}
		if r.URL.Path == "/favicon.ico" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			http.NotFound(w, r)
			return
		}
		name := "index.html"
		if strings.HasPrefix(r.URL.Path, "/assets/") || r.URL.Path == "/bootstrap.js" || r.URL.Path == "/favicon.svg" {
			name = strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		} else if !admin && !strings.HasPrefix(r.URL.Path, "/u/") {
			http.NotFound(w, r)
			return
		}
		if a.assets == nil {
			http.Error(w, "Web assets have not been built.", 503)
			return
		}
		b, err := fs.ReadFile(a.assets, name)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", mime.TypeByExtension(path.Ext(name)))
		if r.Method != "HEAD" {
			if _, err = w.Write(b); err != nil {
				slog.Warn("asset response interrupted", "error", err)
			}
		}
	}
}
