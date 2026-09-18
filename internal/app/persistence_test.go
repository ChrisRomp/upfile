package app

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRestartAndStoppedBackupRestore(t *testing.T) {
	dir := t.TempDir()
	config := Config{DataDir: dir, PublicOrigin: "https://drop.test", AdminOrigin: "https://admin.test", HeadroomBytes: 1}
	a, err := New(config, testVerifier{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{a: a, t: t, admin: a.Admin(), public: a.Public()}
	h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": 1024, "storage_budget_bytes": 4096, "default_link_hours": 168}, nil, 200)
	container, l, c := h.link(nil)
	v := h.admit(l, c, "persist-upload-key", 6, 201)
	h.patch(l, c, v, 0, "abcdef", 204)
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	backup := t.TempDir()
	if err = os.CopyFS(backup, os.DirFS(dir)); err != nil {
		t.Fatal(err)
	}
	for _, dataDir := range []string{dir, backup} {
		config.DataDir = dataDir
		reopened, e := New(config, testVerifier{}, nil)
		if e != nil {
			t.Fatal(e)
		}
		rh := &harness{a: reopened, t: t, admin: reopened.Admin(), public: reopened.Public()}
		rh.call(false, "GET", "/api/links/"+l.ID, nil, c, 200)
		info := decode[Container](t, rh.call(true, "GET", "/api/containers/"+container.ID, nil, nil, 200))
		if info.FileCount != 1 || info.StoredBytes != 6 {
			t.Fatalf("restored metadata %+v", info)
		}
		if b, e := os.ReadFile(filepath.Join(dataDir, "files", v.ID)); e != nil || string(b) != "abcdef" {
			t.Fatalf("restored content %q %v", b, e)
		}
		if err = reopened.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVolumeCannotHaveMultipleWriters(t *testing.T) {
	h := setup(t)
	if other, err := New(h.a.cfg, testVerifier{}, nil); err == nil {
		other.Close()
		t.Fatal("second process could claim storage")
	}
}

func TestSymlinkDatabaseRejected(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("do not open"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "metadata.db")); err != nil {
		t.Fatal(err)
	}
	if a, err := New(Config{DataDir: dir, PublicOrigin: "https://drop.test", AdminOrigin: "https://admin.test"}, testVerifier{}, nil); err == nil {
		a.Close()
		t.Fatal("symlink database accepted")
	}
}
