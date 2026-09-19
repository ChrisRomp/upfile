package app

import (
	"crypto/subtle"
	"fmt"
	"net/http"
	"strings"
	"time"
)

func cookieName(link string) string { return "__Host-upfile_" + link }

func (a *App) authorized(r *http.Request) (Link, string, int64, error) {
	id := r.PathValue("link")
	if !validID(id) {
		return Link{}, "", 0, unavailable()
	}
	l, err := a.link(id)
	if noRows(err) {
		return l, "", 0, unavailable()
	}
	if err != nil {
		return l, "", 0, err
	}
	if l.Status != "active" {
		return l, "", 0, unavailable()
	}
	cookie, err := r.Cookie(cookieName(id))
	if err != nil || len(cookie.Value) != 64 {
		return l, "", 0, problem(401, "session_required", "Reopen the original shared link to continue.")
	}
	hash := digest(cookie.Value)
	var expires int64
	if err = a.db.QueryRow("SELECT expires FROM sessions WHERE hash=? AND link_id=?", hash, id).Scan(&expires); err != nil {
		if noRows(err) {
			return l, "", 0, problem(401, "session_required", "Reopen the original shared link to continue.")
		}
		return l, "", 0, err
	}
	if expires <= a.now().Unix() {
		return l, "", 0, problem(401, "session_required", "Your upload session expired. Reopen the original shared link.")
	}
	return l, hash, expires, nil
}

func (a *App) linkInfo(l Link, expires int64) (map[string]any, error) {
	c, err := a.container(l.ContainerID)
	if err != nil {
		return nil, err
	}
	ids, err := a.ids("SELECT id FROM attempts WHERE link_id=? AND status IN ('allocating','uploading','finalizing','abandoned')", l.ID)
	if err != nil {
		return nil, err
	}
	busy, reset := false, false
	for _, id := range ids {
		v, e := a.attempt(id)
		if e != nil {
			return nil, e
		}
		if v.Status == "abandoned" {
			reset = true
			continue
		}
		busy = true
		last := time.Unix(v.Last, 0)
		if t := a.transfers[id]; t != nil {
			last = t.last
		}
		if v.Status != "finalizing" && a.now().Sub(last) >= a.cfg.Lease {
			reset = true
		}
	}
	return map[string]any{"id": l.ID, "title": c.Name, "instructions": c.Instructions, "max_file_bytes": l.EffectiveMax,
		"expires_at": l.ExpiresAt, "chunk_bytes": a.cfg.ChunkBytes, "session_expires_at": expires, "busy": busy, "reset_available": reset}, nil
}

func (a *App) exchange(w http.ResponseWriter, r *http.Request) error {
	id := r.PathValue("link")
	if !validID(id) {
		return unavailable()
	}
	var v struct {
		Secret string `json:"secret"`
	}
	if err := readJSON(w, r, &v); err != nil {
		return err
	}
	l, err := a.link(id)
	if noRows(err) {
		return unavailable()
	}
	if err != nil {
		return err
	}
	if len(v.Secret) != 64 || subtle.ConstantTimeCompare([]byte(digest(v.Secret)), []byte(l.Hash)) != 1 || l.Status != "active" {
		return unavailable()
	}
	if a.limited("exchange:"+l.ID, 120) {
		return problem(429, "rate_limited", "Too many attempts for this link. Please wait a minute.")
	}
	if existing, _, expiry, e := a.authorized(r); e == nil {
		info, e := a.linkInfo(existing, expiry)
		if e != nil {
			return e
		}
		writeJSON(w, 200, info)
		return nil
	} else if status, _, _ := classify(e); status >= 500 {
		return e
	}
	if err = a.capacity(); err != nil {
		return err
	}
	secret := opaque(32)
	expiry := a.now().Add(a.cfg.SessionTTL).Unix()
	if l.ExpiresAt < expiry {
		expiry = l.ExpiresAt
	}
	if _, err = a.db.Exec("INSERT INTO sessions(hash,link_id,expires) VALUES(?,?,?)", digest(secret), id, expiry); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName(id), Value: secret, Path: "/", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: time.Unix(expiry, 0), MaxAge: int(expiry - a.now().Unix())})
	info, err := a.linkInfo(l, expiry)
	if err != nil {
		return err
	}
	writeJSON(w, 200, info)
	return nil
}

func (a *App) publicLink(w http.ResponseWriter, r *http.Request) error {
	l, _, expiry, err := a.authorized(r)
	if err != nil {
		return err
	}
	info, err := a.linkInfo(l, expiry)
	if err != nil {
		return err
	}
	writeJSON(w, 200, info)
	return nil
}

func (a *App) admit(w http.ResponseWriter, r *http.Request) error {
	l, session, _, err := a.authorized(r)
	if err != nil {
		return err
	}
	if a.limited("admit:"+l.ID, 60) {
		return problem(429, "rate_limited", "Too many uploads. Please wait a minute.")
	}
	var v struct {
		Key     string `json:"key"`
		Name    string `json:"name"`
		Comment string `json:"comment"`
		Size    int64  `json:"size"`
	}
	if err = readJSON(w, r, &v); err != nil {
		return err
	}
	if len(v.Key) < 16 || len(v.Key) > 128 || !validText(v.Key, 128, false) || !validName(v.Name) || !validText(v.Comment, 2048, true) || v.Size < 0 || v.Size > MaxSafeInteger {
		return invalid("Choose a valid filename, a comment up to 2048 bytes, and a known file size.")
	}
	var existing string
	err = a.db.QueryRow("SELECT id FROM attempts WHERE session_hash=? AND key=?", session, v.Key).Scan(&existing)
	if err == nil {
		old, e := a.attempt(existing)
		if e != nil {
			return e
		}
		if old.LinkID != l.ID || old.Name != v.Name || old.AdmissionComment != v.Comment || old.Size != v.Size {
			return problem(409, "idempotency_conflict", "This upload key was already used for different content.")
		}
		if old.Status == "canceled" || old.Status == "abandoned" {
			return problem(410, "attempt_unavailable", "This upload ended. Start it again as a new upload.")
		}
		if old.Status == "allocating" {
			if e = a.allocate(old); e != nil {
				return e
			}
			old, e = a.attempt(old.ID)
			if e != nil {
				return e
			}
		}
		writeJSON(w, 200, old)
		return nil
	}
	if !noRows(err) {
		return err
	}
	if err = a.capacity(); err != nil {
		return err
	}
	if v.Size > l.EffectiveMax {
		return problem(413, "too_large", "This file exceeds the allowed per-file size.")
	}
	if err = a.sweep(); err != nil {
		return err
	}
	stopping := 0
	for id, t := range a.transfers {
		if t.busy {
			old, e := a.attempt(id)
			if e != nil {
				return e
			}
			if old.LinkID == l.ID {
				return problem(409, "busy", "A previous transfer is still stopping. Please retry shortly.")
			}
			if old.Status != "allocating" && old.Status != "uploading" && old.Status != "finalizing" {
				stopping++
			}
		}
	}
	var active, total int
	if err = a.db.QueryRow("SELECT count(*),coalesce(sum(CASE WHEN link_id=? THEN 1 ELSE 0 END),0) FROM attempts WHERE status IN ('allocating','uploading','finalizing')", l.ID).Scan(&total, &active); err != nil {
		return err
	}
	if active > 0 {
		return problem(409, "busy", "Another file is using this link. Wait for it to finish or reset an abandoned upload.")
	}
	if total+stopping >= a.cfg.MaxActive {
		return problem(429, "rate_limited", "The server is handling other uploads. Please retry shortly.")
	}
	s, err := a.settings()
	if err != nil {
		return err
	}
	if v.Size > s.StorageBudget-s.StoredBytes-s.ReservedBytes {
		return problem(507, "storage_full", "Storage capacity has been reached. Contact the administrator.")
	}
	if err = a.diskRoom(v.Size, s.ReservedBytes); err != nil {
		return err
	}
	id := opaque(16)
	now := a.now().Unix()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO attempts(id,link_id,session_hash,key,name,comment,admission_comment,size,status,created,last,reserved)
	 VALUES(?,?,?,?,?,?,?,?,'allocating',?,?,?)`, id, l.ID, session, v.Key, v.Name, v.Comment, v.Comment, v.Size, now, now, v.Size); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	attempt, err := a.attempt(id)
	if err != nil {
		return err
	}
	if err = a.allocate(attempt); err != nil {
		return err
	}
	attempt, err = a.attempt(id)
	if err != nil {
		return err
	}
	if attempt.Size == 0 {
		if err = a.finalize(attempt.ID); err != nil {
			return err
		}
		attempt, err = a.attempt(id)
		if err != nil {
			return err
		}
	}
	writeJSON(w, 201, attempt)
	return nil
}

func (a *App) owned(r *http.Request) (Attempt, error) {
	l, session, _, err := a.authorized(r)
	if err != nil {
		return Attempt{}, err
	}
	id := r.PathValue("attempt")
	if !validID(id) {
		return Attempt{}, problem(404, "attempt_unavailable", "This upload is unavailable.")
	}
	v, err := a.attempt(id)
	if err != nil {
		return v, err
	}
	if v.LinkID != l.ID || v.SessionHash != session {
		return Attempt{}, problem(404, "attempt_unavailable", "This upload is unavailable.")
	}
	return v, nil
}
func (a *App) receipt(w http.ResponseWriter, r *http.Request) error {
	v, err := a.owned(r)
	if err != nil {
		return err
	}
	if (v.Status == "uploading" && v.Offset == v.Size) || v.Status == "finalizing" {
		if err = a.finalize(v.ID); err != nil {
			return err
		}
		v, err = a.attempt(v.ID)
		if err != nil {
			return err
		}
	}
	writeJSON(w, 200, v)
	return nil
}

func (a *App) saveAttemptComment(w http.ResponseWriter, r *http.Request) error {
	v, err := a.owned(r)
	if err != nil {
		return err
	}
	switch v.Status {
	case "allocating", "uploading", "finalizing", "completed":
	default:
		return problem(410, "attempt_unavailable", "This upload ended. Its comment cannot be changed.")
	}
	var input struct {
		Comment *string `json:"comment"`
	}
	if err = readJSON(w, r, &input); err != nil {
		return err
	}
	if input.Comment == nil || !validText(*input.Comment, 2048, true) {
		return invalid("Provide a comment up to 2048 bytes, or an empty string to clear it.")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE attempts SET comment=? WHERE id=?", *input.Comment, v.ID); err != nil {
		return err
	}
	if v.Status == "completed" {
		result, err := tx.Exec("UPDATE files SET comment=? WHERE id=? AND status='ready'", *input.Comment, v.ID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return problem(410, "file_unavailable", "This file is no longer available. Its comment cannot be changed.")
		}
	}
	if err = a.auditTx(tx, "uploader", "file.comment", v.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]string{"comment": *input.Comment})
	return nil
}

func (a *App) cancelAttempt(w http.ResponseWriter, r *http.Request) error {
	v, err := a.owned(r)
	if err != nil {
		return err
	}
	if v.Status == "completed" {
		return problem(409, "already_completed", "The file is already received; only an administrator can delete it.")
	}
	if err = a.cancel(v.ID, "canceled"); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]string{"status": "canceled"})
	return nil
}
func (a *App) reset(w http.ResponseWriter, r *http.Request) error {
	l, _, _, err := a.authorized(r)
	if err != nil {
		return err
	}
	ids, err := a.ids("SELECT id FROM attempts WHERE link_id=? AND status IN ('allocating','uploading','finalizing','abandoned')", l.ID)
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
		if v.Status == "finalizing" || (v.Status != "abandoned" && a.now().Sub(last) < a.cfg.Lease) {
			return problem(409, "busy", "This upload is still active. Wait before resetting it.")
		}
	}
	for _, id := range ids {
		if err = a.cancel(id, "abandoned"); err != nil {
			return err
		}
	}
	writeJSON(w, 200, map[string]string{"status": "reset"})
	return nil
}

func (a *App) attemptValid(v Attempt) error {
	l, err := a.link(v.LinkID)
	if err != nil {
		return err
	}
	if l.Status != "active" {
		return unavailable()
	}
	if v.Size > l.EffectiveMax {
		return problem(413, "too_large", "The administrator lowered the size limit. This upload cannot continue.")
	}
	var expiry int64
	if err = a.db.QueryRow("SELECT expires FROM sessions WHERE hash=? AND link_id=?", v.SessionHash, v.LinkID).Scan(&expiry); err != nil {
		if noRows(err) {
			return problem(401, "session_required", "The upload session ended. Reopen the original link.")
		}
		return err
	}
	if expiry <= a.now().Unix() {
		return problem(401, "session_required", "The upload session expired. Reopen the original link.")
	}
	return nil
}

func (a *App) invalidateOversize() error {
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
		}
	}
	return nil
}

func (a *App) cancel(id, state string) error {
	if state != "canceled" && state != "abandoned" {
		return fmt.Errorf("invalid cancel state %q", state)
	}
	if _, err := a.db.Exec("UPDATE attempts SET status=?,cleanup=1 WHERE id=? AND status!='completed'", state, id); err != nil {
		return err
	}
	if t := a.transfers[id]; t != nil && t.busy {
		if t.stop != nil {
			t.stop()
		}
		return nil
	}
	return a.cleanupAttempt(id)
}

func safeTusHeaders(r *http.Request) bool {
	for _, h := range []string{"Upload-Concat", "Upload-Defer-Length", "Upload-Length", "Upload-Metadata", "X-HTTP-Method-Override"} {
		if strings.TrimSpace(r.Header.Get(h)) != "" {
			return false
		}
	}
	return r.Header.Get("Tus-Resumable") == "1.0.0"
}
