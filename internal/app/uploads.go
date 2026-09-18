package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	tus "github.com/tus/tusd/v2/pkg/handler"
)

func (a *App) allocate(v Attempt) error {
	if !validID(v.ID) {
		return errors.New("invalid stored attempt ID")
	}
	_, err := regularStat(a.partial(v.ID) + ".info")
	if err == nil {
		u, e := a.store.GetUpload(context.Background(), v.ID)
		if e != nil {
			// An interrupted metadata write is recoverable only before the URL was issued.
			st, statErr := regularStat(a.partial(v.ID))
			if statErr != nil || st.Size() != 0 {
				return errors.Join(e, statErr)
			}
			if e = removeFile(a.partial(v.ID) + ".info"); e != nil {
				return e
			}
			if e = removeFile(a.partial(v.ID)); e != nil {
				return e
			}
			if _, e = a.store.NewUpload(context.Background(), tus.FileInfo{ID: v.ID, Size: v.Size}); e != nil {
				return e
			}
		} else {
			info, e := u.GetInfo(context.Background())
			if e != nil {
				return e
			}
			if info.ID != v.ID || info.Size != v.Size || info.Offset != 0 {
				return errors.New("inconsistent allocating upload")
			}
		}
	} else {
		if !isMissing(err) {
			return err
		}
		// No client received the URL while allocating; an orphan empty binary is safe to remove.
		if s, e := regularStat(a.partial(v.ID)); e == nil {
			if s.Size() != 0 {
				return errors.New("unexpected bytes in allocating upload")
			}
			if e = removeFile(a.partial(v.ID)); e != nil {
				return e
			}
		} else if !isMissing(e) {
			return e
		}
		if _, err = a.store.NewUpload(context.Background(), tus.FileInfo{ID: v.ID, Size: v.Size}); err != nil {
			return err
		}
	}
	if err = syncFile(a.partial(v.ID)); err != nil {
		return err
	}
	if err = syncFile(a.partial(v.ID) + ".info"); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(a.partial(v.ID))); err != nil {
		return err
	}
	_, err = a.db.Exec("UPDATE attempts SET status='uploading' WHERE id=? AND status='allocating'", v.ID)
	return err
}

func (a *App) finalize(id string) error {
	v, err := a.attempt(id)
	if err != nil {
		return err
	}
	if v.Status == "completed" {
		return nil
	}
	if v.Status != "uploading" && v.Status != "finalizing" {
		return problem(410, "attempt_unavailable", "This upload cannot be completed.")
	}
	if t := a.transfers[id]; t != nil && t.busy {
		return problem(409, "busy", "The file is still being transferred.")
	}
	if err = a.attemptValid(v); err != nil {
		status, _, _ := classify(err)
		if status < 500 {
			if e := a.cancel(id, "canceled"); e != nil {
				return e
			}
		}
		return err
	}
	dest := a.completed(id)
	st, e := regularStat(dest)
	if isMissing(e) {
		st, err = regularStat(a.partial(id))
		if err != nil {
			return err
		}
		if st.Size() != v.Size {
			return problem(409, "incomplete", "The upload is not complete.")
		}
		if _, err = a.db.Exec("UPDATE attempts SET status='finalizing',last=CASE WHEN status='finalizing' THEN last ELSE ? END WHERE id=?", a.now().Unix(), id); err != nil {
			return err
		}
		if err = syncFile(a.partial(id)); err != nil {
			return err
		}
		if err = a.rename(a.partial(id), dest); err != nil {
			return err
		}
		if err = syncDir(filepath.Dir(dest)); err != nil {
			return err
		}
		if err = syncDir(filepath.Dir(a.partial(id))); err != nil {
			return err
		}
	} else if e != nil {
		return e
	}
	if st.Size() != v.Size {
		return errors.New("completed file length mismatch")
	}
	l, err := a.link(v.LinkID)
	if err != nil {
		return err
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT OR IGNORE INTO files(id,container_id,link_id,name,original_name,sender,comment,size,created)
	 VALUES(?,?,?,?,?,?,?,?,?)`, id, l.ContainerID, l.ID, v.Name, v.Name, l.SenderLabel, v.Comment, v.Size, a.now().Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE attempts SET status='completed',reserved=0,cleanup=1 WHERE id=?", id); err != nil {
		return err
	}
	if _, err = tx.Exec("INSERT INTO audit(actor,action,target,created) VALUES('uploader','file.received',?,?)", id, a.now().Unix()); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	return a.cleanupAttempt(id)
}

type bufferedResponse struct {
	header     http.Header
	code       int
	body       bytes.Buffer
	underlying http.ResponseWriter
}

func (w *bufferedResponse) Unwrap() http.ResponseWriter { return w.underlying }
func (w *bufferedResponse) Header() http.Header         { return w.header }
func (w *bufferedResponse) WriteHeader(code int) {
	if w.code == 0 {
		w.code = code
	}
}
func (w *bufferedResponse) Write(b []byte) (int, error) {
	if w.code == 0 {
		w.code = 200
	}
	return w.body.Write(b)
}

type progressBody struct {
	io.ReadCloser
	app       *App
	id        string
	lastCheck time.Time
}

func (b *progressBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.app.mu.Lock()
		t := b.app.transfers[b.id]
		if t == nil || !t.busy {
			b.app.mu.Unlock()
			return 0, errors.New("upload was stopped")
		}
		t.last = b.app.now()
		if t.last.Sub(b.lastCheck) >= time.Second {
			b.lastCheck = t.last
			v, e := b.app.attempt(b.id)
			if e == nil && v.Status != "uploading" {
				e = problem(410, "attempt_unavailable", "This upload ended.")
			}
			if e == nil {
				e = b.app.attemptValid(v)
			}
			if e != nil {
				b.app.mu.Unlock()
				return 0, e
			}
		}
		b.app.mu.Unlock()
	}
	return n, err
}

func (a *App) uploadHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	v, err := a.owned(r)
	if err == nil && !safeTusHeaders(r) {
		err = invalid("Unsupported tus headers or protocol version.")
	}
	if err == nil && v.Status == "completed" {
		a.mu.Unlock()
		if r.Method != "HEAD" {
			a.fail(w, problem(409, "already_completed", "The file is already received."))
			return
		}
		w.Header().Set("Tus-Resumable", "1.0.0")
		w.Header().Set("Upload-Offset", strconv.FormatInt(v.Size, 10))
		w.Header().Set("Upload-Length", strconv.FormatInt(v.Size, 10))
		w.WriteHeader(200)
		return
	}
	if err == nil && v.Status != "uploading" {
		err = problem(410, "attempt_unavailable", "This upload ended. Start the file again.")
	}
	if err == nil {
		err = a.attemptValid(v)
	}
	if err == nil {
		last := time.Unix(v.Last, 0)
		if t := a.transfers[v.ID]; t != nil {
			last = t.last
		}
		if a.now().Sub(last) >= a.cfg.Lease {
			err = problem(410, "attempt_unavailable", "The upload was idle too long. Reset it and start again.")
		}
	}
	if err == nil && r.Method == "PATCH" && (r.ContentLength < 0 || r.ContentLength > a.cfg.ChunkBytes) {
		err = problem(413, "too_large", "Upload chunks must have a known length within the configured chunk limit.")
	}
	if err == nil && r.Method == "PATCH" && r.ContentLength > v.Size-v.Offset {
		err = problem(413, "too_large", "This chunk exceeds the remaining declared file size.")
	}
	if err == nil && r.Method == "PATCH" && r.Header.Get("Content-Type") != "application/offset+octet-stream" {
		err = invalid("Unsupported upload content type.")
	}
	if err == nil {
		if t := a.transfers[v.ID]; t != nil && t.busy {
			err = problem(409, "busy", "Another request is writing this file.")
		}
	}
	if err != nil {
		a.mu.Unlock()
		a.fail(w, err)
		return
	}
	if _, err = regularStat(a.partial(v.ID)); err == nil {
		_, err = regularStat(a.partial(v.ID) + ".info")
	}
	if err != nil {
		a.mu.Unlock()
		a.fail(w, err)
		return
	}
	if r.Method == "HEAD" {
		u, e := a.store.GetUpload(r.Context(), v.ID)
		if e == nil {
			var info tus.FileInfo
			info, e = u.GetInfo(r.Context())
			if e == nil {
				w.Header().Set("Tus-Resumable", "1.0.0")
				w.Header().Set("Upload-Offset", strconv.FormatInt(info.Offset, 10))
				w.Header().Set("Upload-Length", strconv.FormatInt(v.Size, 10))
				w.WriteHeader(200)
			}
		}
		a.mu.Unlock()
		if e != nil {
			a.fail(w, e)
		}
		return
	}
	original := r.Body
	controller := http.NewResponseController(w)
	a.transfers[v.ID] = &transfer{busy: true, last: a.now(), body: original, stop: func() {
		// Interrupt a blocked network read before Close can try to drain the body.
		_ = controller.SetReadDeadline(time.Now())
		_ = original.Close()
	}}
	a.mu.Unlock()
	// Refresh network deadlines on actual socket reads; buffered response cannot expose a controller.
	_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(time.Minute))
	r2 := r.Clone(r.Context())
	u := *r.URL
	u.Path = "/" + v.ID
	r2.URL = &u
	r2.Body = &deadlineBody{ReadCloser: &progressBody{ReadCloser: http.MaxBytesReader(w, original, a.cfg.ChunkBytes), app: a, id: v.ID}, controller: http.NewResponseController(w)}
	out := &bufferedResponse{header: make(http.Header), underlying: w}
	a.tusHTTP.ServeHTTP(out, r2)
	a.mu.Lock()
	if t := a.transfers[v.ID]; t != nil {
		t.busy = false
		t.body = nil
	}
	current, e := a.attempt(v.ID)
	if e == nil && current.Status == "uploading" {
		_, e = a.db.Exec("UPDATE attempts SET last=? WHERE id=?", a.transfers[v.ID].last.Unix(), v.ID)
		if e == nil && current.Offset == current.Size {
			e = a.finalize(v.ID)
		}
	} else if e == nil && current.Cleanup {
		e = a.cleanupAttempt(v.ID)
	}
	if e == nil && current.Status != "uploading" && current.Status != "completed" {
		e = problem(410, "attempt_unavailable", "The upload was canceled or revoked.")
	}
	delete(a.transfers, v.ID)
	a.mu.Unlock()
	if e != nil {
		a.fail(w, e)
		return
	}
	for k, values := range out.header {
		if len(k) >= 14 && k[:14] == "Access-Control" {
			continue
		}
		w.Header()[k] = values
	}
	if out.code == 0 {
		out.code = 500
	}
	w.WriteHeader(out.code)
	if out.body.Len() > 0 && out.code != http.StatusNoContent {
		if _, e = w.Write(out.body.Bytes()); e != nil {
			slog.Warn("upload response interrupted", "error", e)
		}
	}
}

type deadlineBody struct {
	io.ReadCloser
	controller *http.ResponseController
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(time.Now().Add(time.Minute))
	return b.ReadCloser.Read(p)
}

func (a *App) cleanupAttempt(id string) error {
	v, err := a.attempt(id)
	if err != nil {
		return err
	}
	if !v.Cleanup {
		return nil
	}
	if t := a.transfers[id]; t != nil && t.busy {
		return nil
	}
	if v.Status != "completed" {
		if err = removeFile(a.partial(id)); err != nil {
			return err
		}
		if err = removeFile(a.completed(id)); err != nil {
			return err
		}
	}
	if err = removeFile(a.partial(id) + ".info"); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(a.partial(id))); err != nil {
		return err
	}
	if err = syncDir(filepath.Dir(a.completed(id))); err != nil {
		return err
	}
	_, err = a.db.Exec("UPDATE attempts SET reserved=0,cleanup=0 WHERE id=?", id)
	return err
}

func (a *App) cleanupFile(id string) error {
	for _, d := range a.downloads {
		if d.file == id {
			return nil
		}
	}
	if err := removeFile(a.completed(id)); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(a.completed(id))); err != nil {
		return err
	}
	_, err := a.db.Exec("UPDATE files SET status='deleted' WHERE id=? AND status='deleting'", id)
	return err
}

func (a *App) stopInvalidWriters() error {
	for id, t := range a.transfers {
		v, err := a.attempt(id)
		if err != nil {
			return err
		}
		if v.Status != "uploading" && t.busy && t.stop != nil {
			t.stop()
		}
	}
	return a.sweepCleanup()
}
func (a *App) sweepCleanup() error {
	ids, err := a.ids("SELECT id FROM attempts WHERE cleanup=1")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.cleanupAttempt(id); err != nil {
			return err
		}
	}
	ids, err = a.ids("SELECT id FROM files WHERE status='deleting'")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.cleanupFile(id); err != nil {
			return err
		}
	}
	_, err = a.db.Exec(`UPDATE containers SET status='deleted' WHERE status='deleting'
	 AND NOT EXISTS(SELECT 1 FROM files WHERE container_id=containers.id AND status!='deleted')
	 AND NOT EXISTS(SELECT 1 FROM attempts JOIN links ON links.id=attempts.link_id
	  WHERE links.container_id=containers.id AND (attempts.cleanup=1 OR attempts.reserved>0))`)
	return err
}

func (a *App) sweep() error {
	if err := a.invalidateOversize(); err != nil {
		return err
	}
	finalizing, err := a.ids("SELECT id FROM attempts WHERE status='finalizing'")
	if err != nil {
		return err
	}
	for _, id := range finalizing {
		v, e := a.attempt(id)
		if e != nil {
			return e
		}
		if e = a.finalize(id); e != nil {
			a.logCleanup(e)
			if a.now().Sub(time.Unix(v.Last, 0)) >= 2*time.Minute {
				if e = a.cancel(id, "canceled"); e != nil {
					return e
				}
			}
		}
	}
	ids, err := a.ids("SELECT id FROM attempts WHERE status IN ('uploading','allocating','abandoned')")
	if err != nil {
		return err
	}
	for _, id := range ids {
		v, e := a.attempt(id)
		if e != nil {
			return e
		}
		last := time.Unix(v.Last, 0)
		if t := a.transfers[id]; t != nil {
			last = t.last
		}
		if a.now().Sub(last) >= a.cfg.Retention {
			if e = a.cancel(id, "abandoned"); e != nil {
				return e
			}
		} else if v.Status != "abandoned" && a.now().Sub(last) >= a.cfg.Lease {
			if _, e = a.db.Exec("UPDATE attempts SET status='abandoned' WHERE id=?", id); e != nil {
				return e
			}
			if t := a.transfers[id]; t != nil && t.busy && t.stop != nil {
				t.stop()
			}
		}
	}
	if _, err = a.db.Exec("DELETE FROM sessions WHERE expires<=?", a.now().Unix()); err != nil {
		return err
	}
	if _, err = a.db.Exec("DELETE FROM audit WHERE id <= (SELECT coalesce(max(id),0)-10000 FROM audit)"); err != nil {
		return err
	}
	if err = a.sweepCleanup(); err != nil {
		return err
	}
	return a.pruneMetadata()
}

func (a *App) pruneMetadata() error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Keep idempotency tombstones while their session can still retry.
	queries := []string{
		`DELETE FROM files WHERE status='deleted' AND id IN
		 (SELECT id FROM attempts WHERE cleanup=0 AND reserved=0
		  AND NOT EXISTS(SELECT 1 FROM sessions WHERE hash=attempts.session_hash))`,
		`DELETE FROM attempts WHERE status IN ('completed','canceled','abandoned') AND cleanup=0 AND reserved=0
		 AND NOT EXISTS(SELECT 1 FROM files WHERE id=attempts.id)
		 AND NOT EXISTS(SELECT 1 FROM sessions WHERE hash=attempts.session_hash)`,
		`DELETE FROM links WHERE container_id IN (SELECT id FROM containers WHERE status='deleted')
		 AND NOT EXISTS(SELECT 1 FROM attempts WHERE link_id=links.id)
		 AND NOT EXISTS(SELECT 1 FROM files WHERE link_id=links.id)
		 AND NOT EXISTS(SELECT 1 FROM sessions WHERE link_id=links.id)`,
		`DELETE FROM containers WHERE status='deleted' AND NOT EXISTS(SELECT 1 FROM links WHERE container_id=containers.id)`,
	}
	for _, q := range queries {
		if _, err = tx.Exec(q); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (a *App) reconcile() error {
	ids, err := a.ids("SELECT id FROM attempts WHERE status IN ('allocating','uploading','finalizing')")
	if err != nil {
		return err
	}
	for _, id := range ids {
		v, e := a.attempt(id)
		if e != nil {
			return e
		}
		if e = a.attemptValid(v); e != nil {
			status, _, _ := classify(e)
			if status >= 500 {
				return e
			}
			if e = a.cancel(id, "canceled"); e != nil {
				return e
			}
			continue
		}
		switch v.Status {
		case "allocating":
			if e = a.allocate(v); e != nil {
				return e
			}
		case "finalizing":
			if e = a.finalize(id); e != nil {
				return e
			}
		case "uploading":
			if _, e = regularStat(a.partial(id)); e != nil {
				return fmt.Errorf("missing upload storage %s: %w", id, e)
			}
			if v.Offset > v.Size {
				return errors.New("stored upload exceeds declared size")
			}
			if v.Offset == v.Size {
				if e = a.finalize(id); e != nil {
					return e
				}
			}
		}
	}
	// Unknown artifacts are corruption, not permission to silently delete data.
	for _, dir := range []string{"partial", "files"} {
		entries, e := os.ReadDir(filepath.Join(a.cfg.DataDir, dir))
		if e != nil {
			return e
		}
		for _, entry := range entries {
			id := entry.Name()
			if filepath.Ext(id) == ".info" {
				id = id[:len(id)-5]
			}
			if !validID(id) {
				return fmt.Errorf("unexpected storage artifact in %s", dir)
			}
			if _, e = a.attempt(id); e != nil {
				return fmt.Errorf("untracked storage artifact %s: %w", id, e)
			}
		}
	}
	return a.sweep()
}
func (a *App) logCleanup(err error) { slog.Error("storage cleanup pending", "error", err) }
