package app

import (
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schema = `
CREATE TABLE IF NOT EXISTS settings (
 id INTEGER PRIMARY KEY CHECK(id=1), max_file INTEGER NOT NULL DEFAULT 0,
 budget INTEGER NOT NULL DEFAULT 0, lifetime INTEGER NOT NULL DEFAULT 168);
INSERT OR IGNORE INTO settings(id) VALUES(1);
CREATE TABLE IF NOT EXISTS containers (
 id TEXT PRIMARY KEY, name TEXT NOT NULL, instructions TEXT NOT NULL, max_file INTEGER,
 created INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'active');
CREATE TABLE IF NOT EXISTS links (
 id TEXT PRIMARY KEY, container_id TEXT NOT NULL REFERENCES containers(id), sender TEXT NOT NULL,
 expires INTEGER NOT NULL, max_file INTEGER, created INTEGER NOT NULL, hash TEXT NOT NULL,
 revoked INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS links_container ON links(container_id);
CREATE TABLE IF NOT EXISTS sessions (
 hash TEXT PRIMARY KEY, link_id TEXT NOT NULL REFERENCES links(id), expires INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_link ON sessions(link_id);
CREATE TABLE IF NOT EXISTS attempts (
 id TEXT PRIMARY KEY, link_id TEXT NOT NULL REFERENCES links(id), session_hash TEXT NOT NULL,
 key TEXT NOT NULL, name TEXT NOT NULL, comment TEXT NOT NULL, size INTEGER NOT NULL CHECK(size>=0),
 status TEXT NOT NULL, created INTEGER NOT NULL, last INTEGER NOT NULL, reserved INTEGER NOT NULL,
 cleanup INTEGER NOT NULL DEFAULT 0, UNIQUE(session_hash,key));
CREATE INDEX IF NOT EXISTS attempts_link ON attempts(link_id,status);
CREATE TABLE IF NOT EXISTS files (
 id TEXT PRIMARY KEY REFERENCES attempts(id), container_id TEXT NOT NULL REFERENCES containers(id),
 link_id TEXT NOT NULL REFERENCES links(id), name TEXT NOT NULL, original_name TEXT NOT NULL,
 sender TEXT NOT NULL, comment TEXT NOT NULL, size INTEGER NOT NULL, created INTEGER NOT NULL,
 status TEXT NOT NULL DEFAULT 'ready');
CREATE INDEX IF NOT EXISTS files_container ON files(container_id,created);
CREATE TABLE IF NOT EXISTS audit (
 id INTEGER PRIMARY KEY AUTOINCREMENT, actor TEXT NOT NULL, action TEXT NOT NULL,
 target TEXT NOT NULL, created INTEGER NOT NULL);
PRAGMA user_version=1;
`

func openDB(dir string) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.Join(dir, "metadata.db")}
	q := u.Query()
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err == nil && version > 1 {
		err = fmt.Errorf("database schema %d is newer than this application", version)
	}
	if err == nil {
		_, err = db.Exec(schema)
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

func (a *App) settings() (s Settings, err error) {
	err = a.db.QueryRow("SELECT max_file,budget,lifetime FROM settings WHERE id=1").Scan(&s.MaxFileBytes, &s.StorageBudget, &s.DefaultLinkHours)
	if err != nil {
		return
	}
	s.Configured = s.MaxFileBytes > 0 && s.StorageBudget > 0
	s.ChunkBytes, s.LeaseSeconds, s.MaxRecords = a.cfg.ChunkBytes, int64(a.cfg.Lease.Seconds()), a.cfg.MaxRecords
	err = a.db.QueryRow(`SELECT (SELECT coalesce(sum(size),0) FROM files WHERE status!='deleted'),
	 (SELECT coalesce(sum(reserved),0) FROM attempts),
	 (SELECT count(*) FROM attempts)+(SELECT count(*) FROM links)+(SELECT count(*) FROM containers)+(SELECT count(*) FROM sessions),
	 (SELECT count(*) FROM attempts WHERE cleanup=1)+(SELECT count(*) FROM files WHERE status='deleting')`).Scan(
		&s.StoredBytes, &s.ReservedBytes, &s.RecordCount, &s.CleanupErrors)
	return
}

func (a *App) container(id string) (c Container, err error) {
	err = a.db.QueryRow(`SELECT id,name,instructions,max_file,created,status,
	 (SELECT count(*) FROM files WHERE container_id=c.id AND status!='deleted'),
	 (SELECT coalesce(sum(size),0) FROM files WHERE container_id=c.id AND status!='deleted'),
	 (SELECT count(*) FROM attempts JOIN links ON links.id=attempts.link_id WHERE links.container_id=c.id AND attempts.status IN ('allocating','uploading','finalizing')),
	 (SELECT count(*) FROM links WHERE container_id=c.id)
	 FROM containers c WHERE id=? AND status!='deleted'`, id).Scan(&c.ID, &c.Name, &c.Instructions, &c.MaxFileBytes, &c.CreatedAt, &c.Status, &c.FileCount, &c.StoredBytes, &c.ActiveUploads, &c.LinkCount)
	if err != nil {
		return
	}
	s, err := a.settings()
	if err != nil {
		return c, err
	}
	c.EffectiveMax = effective(s.MaxFileBytes, c.MaxFileBytes)
	if err = a.db.QueryRow(`SELECT max(?,coalesce((SELECT max(created) FROM files WHERE container_id=?),0),
	 coalesce((SELECT max(created) FROM links WHERE container_id=?),0))`, c.CreatedAt, id, id).Scan(&c.LastActivity); err != nil {
		return c, err
	}
	for _, d := range a.downloads {
		if d.container == id {
			c.ActiveDownloads++
		}
	}
	return c, nil
}

func (a *App) link(id string) (l Link, err error) {
	err = a.db.QueryRow(`SELECT id,container_id,sender,expires,max_file,created,hash,revoked,
	 (SELECT count(*) FROM files WHERE link_id=l.id AND status!='deleted'),
	 (SELECT count(*) FROM attempts WHERE link_id=l.id AND status IN ('allocating','uploading','finalizing'))
	 FROM links l WHERE id=?`, id).Scan(&l.ID, &l.ContainerID, &l.SenderLabel, &l.ExpiresAt, &l.MaxFileBytes, &l.CreatedAt, &l.Hash, &l.Revoked, &l.FileCount, &l.Active)
	if err != nil {
		return
	}
	c, err := a.container(l.ContainerID)
	if err != nil {
		return l, err
	}
	l.EffectiveMax = effective(c.EffectiveMax, l.MaxFileBytes)
	l.Status = "active"
	if l.ExpiresAt <= a.now().Unix() {
		l.Status = "expired"
	}
	if l.Revoked || c.Status != "active" {
		l.Status = "revoked"
	}
	return l, nil
}

func (a *App) attempt(id string) (v Attempt, err error) {
	err = a.db.QueryRow(`SELECT id,link_id,session_hash,key,name,comment,size,status,created,last,reserved,cleanup
	 FROM attempts WHERE id=?`, id).Scan(&v.ID, &v.LinkID, &v.SessionHash, &v.Key, &v.Name, &v.Comment, &v.Size, &v.Status, &v.CreatedAt, &v.Last, &v.Reserved, &v.Cleanup)
	if err == nil {
		v.UploadURL = a.cfg.PublicOrigin + "/api/links/" + v.LinkID + "/uploads/" + v.ID
		if v.Status == "completed" {
			v.Offset = v.Size
		} else {
			stat, e := regularStat(a.partial(v.ID))
			if e == nil {
				v.Offset = stat.Size()
			} else if !isMissing(e) {
				err = e
			}
		}
	}
	return
}

func (a *App) file(id string) (v File, err error) {
	err = a.db.QueryRow(`SELECT id,container_id,link_id,name,original_name,sender,comment,size,created,status
	 FROM files WHERE id=? AND status!='deleted'`, id).Scan(&v.ID, &v.ContainerID, &v.LinkID, &v.Name, &v.OriginalName, &v.SenderLabel, &v.Comment, &v.Size, &v.CreatedAt, &v.Status)
	return
}

func (a *App) ids(query string, args ...any) ([]string, error) {
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func (a *App) audit(actor, action, target string) error {
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT INTO audit(actor,action,target,created) VALUES(?,?,?,?)", actor, action, target, a.now().Unix()); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM audit WHERE id <= (SELECT coalesce(max(id),0)-10000 FROM audit)"); err != nil {
		return err
	}
	return tx.Commit()
}
