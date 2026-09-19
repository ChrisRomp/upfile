package app

import (
	"database/sql"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strings"
	"time"
)

func (a *App) getSettings(w http.ResponseWriter, r *http.Request) error {
	s, err := a.settings()
	if err != nil {
		return err
	}
	writeJSON(w, 200, s)
	return nil
}
func (a *App) putSettings(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Max      int64 `json:"max_file_bytes"`
		Budget   int64 `json:"storage_budget_bytes"`
		Lifetime int64 `json:"default_link_hours"`
	}
	if err := readJSON(w, r, &v); err != nil {
		return err
	}
	if v.Max <= 0 || v.Max > MaxSafeInteger || v.Budget <= 0 || v.Budget > MaxSafeInteger || v.Lifetime < 1 || v.Lifetime > 87600 {
		return invalid("Set positive byte limits and a link lifetime between 1 hour and 10 years.")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE settings SET max_file=?,budget=?,lifetime=? WHERE id=1", v.Max, v.Budget, v.Lifetime); err != nil {
		return err
	}
	if err = a.auditTx(tx, actor(r), "settings.update", "settings"); err != nil {
		return err
	}
	ids, err := idsFrom(tx, "SELECT DISTINCT link_id FROM attempts WHERE status IN ('allocating','uploading','finalizing')")
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = a.invalidateEditedLinkTx(tx, id); err != nil {
			return err
		}
	}
	s, err := a.settingsFrom(tx)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = a.stopInvalidWriters(); err != nil {
		a.logCleanup(fmt.Errorf("settings.update: %w", err))
	}
	writeJSON(w, 200, s)
	return nil
}

func (a *App) capacity() error {
	return a.capacityFor(1)
}

func (a *App) capacityFor(records int) error {
	s, err := a.settings()
	if err != nil {
		return err
	}
	if !s.Configured {
		return problem(409, "setup_required", "The administrator must configure storage limits first.")
	}
	if records > a.cfg.MaxRecords-s.RecordCount {
		return problem(507, "record_limit", "Metadata capacity is full. Contact the administrator.")
	}
	return nil
}

func (a *App) listContainers(w http.ResponseWriter, r *http.Request) error {
	limit, off, err := page(r)
	if err != nil {
		return err
	}
	max, err := globalMaxFrom(a.db)
	if err != nil {
		return err
	}
	q := "%" + r.URL.Query().Get("q") + "%"
	var total int
	if err = a.db.QueryRow("SELECT count(*) FROM containers WHERE status!='deleted' AND name LIKE ?", q).Scan(&total); err != nil {
		return err
	}
	ids, err := a.ids(`SELECT id FROM containers WHERE status!='deleted' AND name LIKE ?
	 ORDER BY max(created,coalesce((SELECT max(created) FROM files WHERE container_id=containers.id),0),
	 coalesce((SELECT max(created) FROM links WHERE container_id=containers.id),0)) DESC,id LIMIT ? OFFSET ?`, q, limit, off)
	if err != nil {
		return err
	}
	items := []Container{}
	for _, id := range ids {
		v, err := a.containerWithMaxFrom(a.db, id, max)
		if err != nil {
			return err
		}
		items = append(items, v)
	}
	collection(w, items, total)
	return nil
}
func (a *App) getContainer(w http.ResponseWriter, r *http.Request) error {
	v, err := a.container(r.PathValue("container"))
	if err != nil {
		return err
	}
	writeJSON(w, 200, v)
	return nil
}
func (a *App) saveContainer(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Name         string `json:"name"`
		Instructions string `json:"instructions"`
		Max          *int64 `json:"max_file_bytes"`
	}
	if err := readJSON(w, r, &v); err != nil {
		return err
	}
	s, err := a.settings()
	if err != nil {
		return err
	}
	if strings.TrimSpace(v.Name) == "" || !validText(v.Name, 255, false) || !validText(v.Instructions, 4096, true) || !validOverride(v.Max, s.MaxFileBytes) {
		return invalid("Provide a name, instructions up to 4096 bytes, and an optional limit no larger than the global maximum.")
	}
	id := r.PathValue("container")
	if id == "" {
		if err = a.capacityFor(2); err != nil {
			return err
		}
		id = opaque(16)
		linkID, secret, now := opaque(16), opaque(32), a.now()
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("INSERT INTO containers(id,name,instructions,max_file,created) VALUES(?,?,?,?,?)", id, v.Name, v.Instructions, v.Max, now.Unix()); err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO links(id,container_id,sender,expires,max_file,created,hash)
			VALUES(?,?,?,?,NULL,?,?)`, linkID, id, v.Name,
			now.Add(time.Duration(s.DefaultLinkHours)*time.Hour).Unix(), now.Unix(), digest(secret)); err != nil {
			return err
		}
		if err = a.auditTx(tx, actor(r), "container.save", id); err != nil {
			return err
		}
		if err = a.auditTx(tx, actor(r), "link.save", linkID); err != nil {
			return err
		}
		container, err := a.containerFrom(tx, id)
		if err != nil {
			return err
		}
		link, err := a.linkFrom(tx, linkID)
		if err != nil {
			return err
		}
		link.URL = a.cfg.Origin + "/u/" + linkID + "#" + secret
		if err = tx.Commit(); err != nil {
			return err
		}
		writeJSON(w, 200, CreatedContainer{Container: container, InitialLink: link})
		return nil
	}
	c, err := a.container(id)
	if err != nil {
		return err
	}
	if c.Status != "active" {
		return problem(409, "deleting", "This container is being deleted.")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE containers SET name=?,instructions=?,max_file=? WHERE id=?", v.Name, v.Instructions, v.Max, id); err != nil {
		return err
	}
	if err = a.auditTx(tx, actor(r), "container.save", id); err != nil {
		return err
	}
	ids, err := idsFrom(tx, "SELECT id FROM links WHERE container_id=?", id)
	if err != nil {
		return err
	}
	for _, linkID := range ids {
		if err = a.invalidateEditedLinkTx(tx, linkID); err != nil {
			return err
		}
	}
	vout, err := a.containerFrom(tx, id)
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if err = a.stopInvalidWriters(); err != nil {
		a.logCleanup(fmt.Errorf("container.save %s: %w", id, err))
	}
	writeJSON(w, 200, vout)
	return nil
}

func (a *App) listLinks(w http.ResponseWriter, r *http.Request) error {
	c, err := a.container(r.PathValue("container"))
	if err != nil {
		return err
	}
	limit, off, err := page(r)
	if err != nil {
		return err
	}
	q := "%" + r.URL.Query().Get("q") + "%"
	var total int
	if err = a.db.QueryRow("SELECT count(*) FROM links WHERE container_id=? AND sender LIKE ?", c.ID, q).Scan(&total); err != nil {
		return err
	}
	ids, err := a.ids("SELECT id FROM links WHERE container_id=? AND sender LIKE ? ORDER BY created DESC,id LIMIT ? OFFSET ?", c.ID, q, limit, off)
	if err != nil {
		return err
	}
	items := []Link{}
	for _, id := range ids {
		l, err := linkRecordFrom(a.db, id)
		if err != nil {
			return err
		}
		items = append(items, a.linkWithContainer(l, c))
	}
	collection(w, items, total)
	return nil
}

func (a *App) saveLink(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Sender  string `json:"sender_label"`
		Expires int64  `json:"expires_at"`
		Max     *int64 `json:"max_file_bytes"`
	}
	if err := readJSON(w, r, &v); err != nil {
		return err
	}
	id, cid := r.PathValue("link"), r.PathValue("container")
	if id != "" {
		l, err := a.link(id)
		if err != nil {
			return err
		}
		cid = l.ContainerID
	}
	c, err := a.container(cid)
	if err != nil {
		return err
	}
	if c.Status != "active" {
		return unavailable()
	}
	s, err := a.settings()
	if err != nil {
		return err
	}
	if v.Expires == 0 && id == "" {
		v.Expires = a.now().Add(time.Duration(s.DefaultLinkHours) * time.Hour).Unix()
	}
	if strings.TrimSpace(v.Sender) == "" || !validText(v.Sender, 255, false) || v.Expires <= a.now().Unix() || v.Expires > a.now().AddDate(10, 0, 0).Unix() || !validOverride(v.Max, c.EffectiveMax) {
		return invalid("Set a sender label, a future expiration within ten years, and an optional stricter size limit.")
	}
	secret := ""
	if id == "" {
		if err = a.capacity(); err != nil {
			return err
		}
		id, secret = opaque(16), opaque(32)
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if secret != "" {
		_, err = tx.Exec("INSERT INTO links(id,container_id,sender,expires,max_file,created,hash) VALUES(?,?,?,?,?,?,?)", id, cid, v.Sender, v.Expires, v.Max, a.now().Unix(), digest(secret))
	} else {
		_, err = tx.Exec("UPDATE links SET sender=?,expires=?,max_file=? WHERE id=?", v.Sender, v.Expires, v.Max, id)
	}
	if err != nil {
		return err
	}
	if err = a.auditTx(tx, actor(r), "link.save", id); err != nil {
		return err
	}
	if secret == "" {
		if err = a.invalidateEditedLinkTx(tx, id); err != nil {
			return err
		}
	}
	l, err := a.linkFrom(tx, id)
	if err != nil {
		return err
	}
	if secret != "" {
		l.URL = a.cfg.Origin + "/u/" + id + "#" + secret
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if secret == "" {
		if err = a.stopInvalidWriters(); err != nil {
			a.logCleanup(fmt.Errorf("link.save %s: %w", id, err))
		}
	}
	writeJSON(w, 200, l)
	return nil
}

func (a *App) invalidateEditedLinkTx(tx *sql.Tx, id string) error {
	l, err := a.linkFrom(tx, id)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE attempts SET status='canceled',cleanup=1
		WHERE link_id=? AND status IN ('allocating','uploading','finalizing')
		AND (?!='active' OR size>? OR NOT EXISTS(
			SELECT 1 FROM sessions WHERE hash=attempts.session_hash
			AND link_id=attempts.link_id AND expires>?))`, id, l.Status, l.EffectiveMax, a.now().Unix())
	return err
}
func (a *App) revokeLink(id, actor string, rotate bool) (Link, error) {
	l, err := a.link(id)
	if err != nil {
		return Link{}, err
	}
	action := "link.revoke"
	l.Active, l.Revoked, l.Status = 0, !rotate, "revoked"
	if rotate {
		action = "link.rotate"
		secret := opaque(32)
		l.Hash = digest(secret)
		l.URL = a.cfg.Origin + "/u/" + id + "#" + secret
	}
	tx, err := a.db.Begin()
	if err != nil {
		return Link{}, err
	}
	defer tx.Rollback()
	if rotate {
		_, err = tx.Exec("UPDATE links SET hash=?,revoked=0 WHERE id=?", l.Hash, id)
	} else {
		_, err = tx.Exec("UPDATE links SET revoked=1 WHERE id=?", id)
	}
	if err != nil {
		return Link{}, err
	}
	if _, err = tx.Exec("DELETE FROM sessions WHERE link_id=?", id); err != nil {
		return Link{}, err
	}
	if _, err = tx.Exec("UPDATE attempts SET status='canceled',cleanup=1 WHERE link_id=? AND status IN ('allocating','uploading','finalizing','abandoned')", id); err != nil {
		return Link{}, err
	}
	if err = a.auditTx(tx, actor, action, id); err != nil {
		return Link{}, err
	}
	if rotate {
		var containerStatus string
		if err = tx.QueryRow("SELECT status FROM containers WHERE id=?", l.ContainerID).Scan(&containerStatus); err != nil {
			return Link{}, err
		}
		if containerStatus == "active" {
			l.Status = "active"
			if l.ExpiresAt <= a.now().Unix() {
				l.Status = "expired"
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return Link{}, err
	}
	// The security change is durable. Cleanup retries must not hide the one-time URL.
	if err = a.stopInvalidWriters(); err != nil {
		a.logCleanup(fmt.Errorf("%s %s: %w", action, id, err))
	}
	return l, nil
}
func (a *App) revoke(w http.ResponseWriter, r *http.Request) error {
	if _, err := a.revokeLink(r.PathValue("link"), actor(r), false); err != nil {
		return err
	}
	writeJSON(w, 200, map[string]string{"status": "revoked"})
	return nil
}
func (a *App) rotate(w http.ResponseWriter, r *http.Request) error {
	l, err := a.revokeLink(r.PathValue("link"), actor(r), true)
	if err != nil {
		return err
	}
	writeJSON(w, 200, l)
	return nil
}
func (a *App) listFiles(w http.ResponseWriter, r *http.Request) error {
	c, err := a.container(r.PathValue("container"))
	if err != nil {
		return err
	}
	limit, off, err := page(r)
	if err != nil {
		return err
	}
	q := "%" + r.URL.Query().Get("q") + "%"
	var total int
	if err = a.db.QueryRow("SELECT count(*) FROM files WHERE container_id=? AND status!='deleted' AND (name LIKE ? OR sender LIKE ?)", c.ID, q, q).Scan(&total); err != nil {
		return err
	}
	ids, err := a.ids("SELECT id FROM files WHERE container_id=? AND status!='deleted' AND (name LIKE ? OR sender LIKE ?) ORDER BY created DESC,id LIMIT ? OFFSET ?", c.ID, q, q, limit, off)
	if err != nil {
		return err
	}
	items := []File{}
	for _, id := range ids {
		v, e := a.file(id)
		if e != nil {
			return e
		}
		items = append(items, v)
	}
	collection(w, items, total)
	return nil
}
func (a *App) renameFile(w http.ResponseWriter, r *http.Request) error {
	var v struct {
		Name string `json:"name"`
	}
	if err := readJSON(w, r, &v); err != nil {
		return err
	}
	if !validName(v.Name) {
		return invalid("Use a filename up to 255 bytes without path separators or control characters.")
	}
	f, err := a.file(r.PathValue("file"))
	if err != nil {
		return err
	}
	if f.Status != "ready" {
		return problem(409, "deleting", "This file is being deleted.")
	}
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE files SET name=? WHERE id=?", v.Name, f.ID); err != nil {
		return err
	}
	if err = a.auditTx(tx, actor(r), "file.rename", f.ID); err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	f.Name = v.Name
	writeJSON(w, 200, f)
	return nil
}
func (a *App) deleteFile(w http.ResponseWriter, r *http.Request) error {
	f, err := a.file(r.PathValue("file"))
	if err != nil {
		return err
	}
	if f.Status != "deleting" {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.Exec("UPDATE files SET status='deleting' WHERE id=?", f.ID); err != nil {
			return err
		}
		if err = a.auditTx(tx, actor(r), "file.delete", f.ID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	a.stopDownloads(f.ID, "")
	if err = a.cleanupFile(f.ID); err != nil {
		a.logCleanup(fmt.Errorf("file.delete %s: %w", f.ID, err))
	}
	state := a.deletionStatus("SELECT status FROM files WHERE id=?", f.ID, "file.delete")
	writeJSON(w, 200, map[string]string{"status": state})
	return nil
}
func (a *App) deleteContainer(w http.ResponseWriter, r *http.Request) error {
	c, err := a.container(r.PathValue("container"))
	if err != nil {
		return err
	}
	if c.Status != "deleting" {
		tx, err := a.db.Begin()
		if err != nil {
			return err
		}
		defer tx.Rollback()
		for _, q := range []string{
			"UPDATE containers SET status='deleting' WHERE id=?",
			"UPDATE links SET revoked=1 WHERE container_id=?",
			"DELETE FROM sessions WHERE link_id IN (SELECT id FROM links WHERE container_id=?)",
			"UPDATE attempts SET status='canceled',cleanup=1 WHERE link_id IN (SELECT id FROM links WHERE container_id=?) AND status IN ('allocating','uploading','finalizing','abandoned')",
			"UPDATE files SET status='deleting' WHERE container_id=? AND status!='deleted'",
		} {
			if _, err = tx.Exec(q, c.ID); err != nil {
				return err
			}
		}
		if err = a.auditTx(tx, actor(r), "container.delete", c.ID); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	a.stopDownloads("", c.ID)
	if err = a.stopInvalidWriters(); err != nil {
		a.logCleanup(fmt.Errorf("container.delete %s: %w", c.ID, err))
	}
	state := a.deletionStatus("SELECT status FROM containers WHERE id=?", c.ID, "container.delete")
	writeJSON(w, 200, map[string]string{"status": state})
	return nil
}

func (a *App) deletionStatus(query, id, action string) string {
	var state string
	if err := a.db.QueryRow(query, id).Scan(&state); err != nil {
		a.logCleanup(fmt.Errorf("%s %s: confirm deletion: %w", action, id, err))
		return "deleting"
	}
	if state == "deleted" {
		return state
	}
	return "deleting"
}

func (a *App) stopDownloads(file, container string) {
	for _, d := range a.downloads {
		if (file != "" && d.file == file) || (container != "" && d.container == container) {
			_ = d.body.Close()
		}
	}
}

func (a *App) downloadHTTP(w http.ResponseWriter, r *http.Request) {
	a.mu.Lock()
	f, err := a.file(r.PathValue("file"))
	var body *os.File
	if err == nil {
		if f.Status != "ready" {
			err = problem(410, "deleting", "This file is no longer available.")
		}
	}
	if err == nil {
		_, err = regularStat(a.completed(f.ID))
	}
	if err == nil {
		body, err = os.Open(a.completed(f.ID))
	}
	if err == nil {
		err = a.audit(actor(r), "file.download", f.ID)
	}
	if err != nil {
		if body != nil {
			body.Close()
		}
		a.mu.Unlock()
		a.fail(w, err)
		return
	}
	id := opaque(16)
	a.downloads[id] = &download{file: f.ID, container: f.ContainerID, body: body}
	a.mu.Unlock()
	defer func() {
		body.Close()
		a.mu.Lock()
		defer a.mu.Unlock()
		delete(a.downloads, id)
		if err := a.sweepCleanup(); err != nil {
			a.logCleanup(err)
		}
	}()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": f.Name}))
	w.Header().Set("ETag", fmt.Sprintf(`"%s"`, f.ID))
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(2 * time.Hour))
	http.ServeContent(w, r, f.Name, time.Unix(f.CreatedAt, 0), body)
}

func (a *App) listAudit(w http.ResponseWriter, r *http.Request) error {
	limit, off, err := page(r)
	if err != nil {
		return err
	}
	q := "%" + r.URL.Query().Get("q") + "%"
	const filter = " WHERE actor LIKE ? OR action LIKE ? OR target LIKE ?"
	var total int
	if err = a.db.QueryRow("SELECT count(*) FROM audit"+filter, q, q, q).Scan(&total); err != nil {
		return err
	}
	rows, err := a.db.Query("SELECT id,actor,action,target,created FROM audit"+filter+" ORDER BY id DESC LIMIT ? OFFSET ?", q, q, q, limit, off)
	if err != nil {
		return err
	}
	defer rows.Close()
	type event struct {
		ID      int64  `json:"id"`
		Actor   string `json:"actor"`
		Action  string `json:"action"`
		Target  string `json:"target"`
		Created int64  `json:"created_at"`
	}
	items := []event{}
	for rows.Next() {
		var e event
		if err = rows.Scan(&e.ID, &e.Actor, &e.Action, &e.Target, &e.Created); err != nil {
			return err
		}
		items = append(items, e)
	}
	if err = rows.Err(); err != nil {
		return err
	}
	collection(w, items, total)
	return nil
}

func noRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
