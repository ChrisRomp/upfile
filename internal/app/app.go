package app

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/tus/tusd/v2/pkg/filestore"
	tus "github.com/tus/tusd/v2/pkg/handler"
	expslog "golang.org/x/exp/slog"
	"golang.org/x/sys/unix"
	"upfile/internal/auth"
)

type transfer struct {
	busy bool
	last time.Time
	body io.ReadCloser
	stop func()
}

type download struct {
	file, container string
	body            *os.File
}

type App struct {
	cfg       Config
	db        *sql.DB
	mu        sync.Mutex
	transfers map[string]*transfer
	downloads map[string]*download
	store     filestore.FileStore
	tus       *tus.UnroutedHandler
	tusHTTP   http.Handler
	verify    auth.Verifier
	assets    fs.FS
	now       func() time.Time
	stop      chan struct{}
	done      chan struct{}
	lock      *os.File
	rates     map[string]rate
	rename    func(string, string) error
}

type rate struct {
	start time.Time
	count int
}

func New(cfg Config, verify auth.Verifier, assets fs.FS) (*App, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	if verify == nil {
		return nil, errors.New("administrator verifier is required")
	}
	abs, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	cfg.DataDir = abs
	for _, dir := range []string{abs, filepath.Join(abs, "partial"), filepath.Join(abs, "files")} {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
		for _, name := range []string{"instance.lock", "metadata.db", "metadata.db-wal", "metadata.db-shm"} {
			if _, e := regularStat(filepath.Join(abs, name)); e != nil && !isMissing(e) {
				return nil, fmt.Errorf("unsafe storage file %s: %w", name, e)
			}
		}
		info, e := os.Lstat(dir)
		if e != nil {
			return nil, e
		}
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return nil, errors.New("storage directories must not be symlinks")
		}
	}
	lock, err := os.OpenFile(filepath.Join(abs, "instance.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("storage is already in use: %w", err)
	}
	db, err := openDB(abs)
	if err != nil {
		lock.Close()
		return nil, err
	}
	a := &App{cfg: cfg, db: db, verify: verify, assets: assets, now: time.Now, stop: make(chan struct{}), done: make(chan struct{}),
		lock: lock, transfers: map[string]*transfer{}, downloads: map[string]*download{}, rates: map[string]rate{}, rename: os.Rename}
	a.store = filestore.New(filepath.Join(abs, "partial"))
	a.store.FileModePerm = 0600
	a.store.DirModePerm = 0700
	composer := tus.NewStoreComposer()
	composer.UseCore(a.store)
	a.tus, err = tus.NewUnroutedHandler(tus.Config{
		StoreComposer: composer, DisableDownload: true, DisableTermination: true, DisableConcatenation: true,
		NetworkTimeout: time.Minute, GracefulRequestCompletionTimeout: time.Second,
		Logger: expslog.New(expslog.NewTextHandler(os.Stderr, &expslog.HandlerOptions{Level: expslog.LevelWarn})),
	})
	if err == nil {
		a.tusHTTP = a.tus.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case "HEAD":
				a.tus.HeadFile(w, r)
			case "PATCH":
				a.tus.PatchFile(w, r)
			default:
				http.Error(w, "method not allowed", 405)
			}
		}))
		err = a.reconcile()
	}
	if err != nil {
		db.Close()
		lock.Close()
		return nil, err
	}
	go a.housekeeping()
	return a, nil
}

func (a *App) Close() error {
	close(a.stop)
	<-a.done
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, t := range a.transfers {
		if t.stop != nil {
			t.stop()
		}
	}
	for _, d := range a.downloads {
		_ = d.body.Close()
	}
	err := a.db.Close()
	e := a.lock.Close()
	return errors.Join(err, e)
}

func (a *App) housekeeping() {
	defer close(a.done)
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-a.stop:
			return
		case <-tick.C:
			a.mu.Lock()
			if err := a.sweep(); err != nil {
				slog.Error("storage housekeeping failed", "error", err)
			}
			a.mu.Unlock()
		}
	}
}

func (a *App) partial(id string) string   { return filepath.Join(a.cfg.DataDir, "partial", id) }
func (a *App) completed(id string) string { return filepath.Join(a.cfg.DataDir, "files", id) }
func isMissing(err error) bool            { return errors.Is(err, os.ErrNotExist) }
func regularStat(path string) (os.FileInfo, error) {
	s, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !s.Mode().IsRegular() {
		return nil, errors.New("storage artifact is not a regular file")
	}
	return s, nil
}

func syncFile(path string) error {
	if _, err := regularStat(path); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func syncDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func removeFile(path string) error {
	_, err := regularStat(path)
	if isMissing(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return os.Remove(path)
}

func (a *App) diskRoom(size, reserved int64) error {
	var st unix.Statfs_t
	if err := unix.Statfs(a.cfg.DataDir, &st); err != nil {
		return err
	}
	free := uint64(st.Bavail) * uint64(st.Bsize)
	// Counting reservations conservatively also covers partial-file bookkeeping.
	if uint64(size)+uint64(reserved)+uint64(a.cfg.HeadroomBytes) > free {
		return problem(507, "storage_full", "Storage is full. Ask the administrator to free space.")
	}
	return nil
}

func (a *App) limited(key string, limit int) bool {
	now := a.now()
	v := a.rates[key]
	if now.Sub(v.start) >= time.Minute {
		v = rate{start: now}
	}
	v.count++
	if len(a.rates) >= 2048 {
		for k, entry := range a.rates {
			if now.Sub(entry.start) >= time.Minute {
				delete(a.rates, k)
			}
		}
		if _, ok := a.rates[key]; !ok && len(a.rates) >= 2048 {
			return true
		}
	}
	a.rates[key] = v
	return v.count > limit
}

func (a *App) Public() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ready"}) })
	m.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/admin/", http.StatusPermanentRedirect)
	})
	m.HandleFunc("GET /api/surface", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]string{"surface": "public"})
	})
	m.HandleFunc("POST /api/links/{link}/exchange", a.endpoint(a.exchange))
	m.HandleFunc("GET /api/links/{link}", a.endpoint(a.publicLink))
	m.HandleFunc("POST /api/links/{link}/attempts", a.endpoint(a.admit))
	m.HandleFunc("GET /api/links/{link}/attempts/{attempt}", a.endpoint(a.receipt))
	m.HandleFunc("POST /api/links/{link}/attempts/{attempt}/cancel", a.endpoint(a.cancelAttempt))
	m.HandleFunc("POST /api/links/{link}/reset", a.endpoint(a.reset))
	m.HandleFunc("HEAD /api/links/{link}/uploads/{attempt}", a.uploadHTTP)
	m.HandleFunc("PATCH /api/links/{link}/uploads/{attempt}", a.uploadHTTP)
	m.HandleFunc("/", a.static(false))
	return a.middleware(m, a.cfg.Origin, false)
}

func (a *App) Admin() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /api/surface", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"surface": "admin"}) })
	m.HandleFunc("GET /api/settings", a.endpoint(a.getSettings))
	m.HandleFunc("PUT /api/settings", a.endpoint(a.putSettings))
	m.HandleFunc("GET /api/containers", a.endpoint(a.listContainers))
	m.HandleFunc("POST /api/containers", a.endpoint(a.saveContainer))
	m.HandleFunc("GET /api/containers/{container}", a.endpoint(a.getContainer))
	m.HandleFunc("PUT /api/containers/{container}", a.endpoint(a.saveContainer))
	m.HandleFunc("DELETE /api/containers/{container}", a.endpoint(a.deleteContainer))
	m.HandleFunc("GET /api/containers/{container}/links", a.endpoint(a.listLinks))
	m.HandleFunc("POST /api/containers/{container}/links", a.endpoint(a.saveLink))
	m.HandleFunc("PUT /api/links/{link}", a.endpoint(a.saveLink))
	m.HandleFunc("POST /api/links/{link}/revoke", a.endpoint(a.revoke))
	m.HandleFunc("POST /api/links/{link}/rotate", a.endpoint(a.rotate))
	m.HandleFunc("GET /api/containers/{container}/files", a.endpoint(a.listFiles))
	m.HandleFunc("PUT /api/files/{file}", a.endpoint(a.renameFile))
	m.HandleFunc("DELETE /api/files/{file}", a.endpoint(a.deleteFile))
	m.HandleFunc("GET /api/files/{file}/download", a.downloadHTTP)
	m.HandleFunc("GET /api/audit", a.endpoint(a.listAudit))
	m.HandleFunc("/", a.static(true))
	protected := a.middleware(m, a.cfg.Origin, true)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		canonical := strings.TrimSuffix(r.URL.Path, "/")
		if r.URL.RawPath != "" || !strings.HasPrefix(r.URL.Path, "/admin/") ||
			(r.URL.Path != "/admin/" && path.Clean(r.URL.Path) != canonical) {
			http.NotFound(w, r)
			return
		}
		http.StripPrefix("/admin", protected).ServeHTTP(w, r)
	})
}

type actorKey struct{}

func actor(r *http.Request) string {
	if p, ok := r.Context().Value(actorKey{}).(auth.Principal); ok {
		return p.Subject
	}
	return "uploader"
}

func (a *App) middleware(next http.Handler, origin string, admin bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if (r.Method == "GET" || r.Method == "HEAD" || r.Method == "DELETE") && (r.ContentLength != 0 || len(r.TransferEncoding) > 0) {
			r.Close = true
			w.Header().Set("Connection", "close")
			_ = http.NewResponseController(w).SetReadDeadline(time.Now())
			a.fail(w, invalid("This request method does not accept a body."))
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("Origin") != origin || r.Header.Get("X-Upfile-Request") != "1" {
				a.fail(w, problem(403, "origin_rejected", "This request is not from the application."))
				return
			}
		}
		if r.Header.Get("X-HTTP-Method-Override") != "" {
			a.fail(w, invalid("Method overrides are not supported."))
			return
		}
		if admin {
			p, err := a.verify.Verify(r.Context(), r.Header.Get("Cf-Access-Jwt-Assertion"))
			if err != nil {
				a.fail(w, problem(401, "authentication_required", "Sign in through Cloudflare Access to continue."))
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), actorKey{}, p))
		}
		if r.Method == "POST" || r.Method == "PUT" {
			// Read bounded JSON before taking the application mutex.
			_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(15 * time.Second))
			body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
			_ = r.Body.Close()
			if err != nil {
				a.fail(w, invalid("Invalid, timed-out, or oversized request."))
				return
			}
			r.Body = io.NopCloser(bytes.NewReader(body))
		}
		next.ServeHTTP(w, r)
	})
}
