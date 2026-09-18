package app

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
)

func TestProcessKilledDuringFinalization(t *testing.T) {
	dir := os.Getenv("UPFILE_CRASH_CHILD_DIR")
	if dir != "" {
		a, err := New(Config{DataDir: dir, PublicOrigin: "https://drop.test", AdminOrigin: "https://admin.test", HeadroomBytes: 1}, testVerifier{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		h := &harness{a: a, t: t, admin: a.Admin(), public: a.Public()}
		h.call(true, "PUT", "/api/settings", map[string]any{"max_file_bytes": 1024, "storage_budget_bytes": 4096, "default_link_hours": 168}, nil, 200)
		_, l, c := h.link(nil)
		v := h.admit(l, c, "process-crash-key", 3, 201)
		a.rename = func(old, new string) error {
			if err := os.Rename(old, new); err != nil {
				return err
			}
			return syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
		h.patch(l, c, v, 0, "abc", 204)
		t.Fatal("child did not terminate at the crash boundary")
	}
	dir = t.TempDir()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-test.run=^TestProcessKilledDuringFinalization$")
	cmd.Env = append(os.Environ(), "UPFILE_CRASH_CHILD_DIR="+dir)
	output, err := cmd.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("child did not crash: %v %s", err, output)
	}
	if status, ok := exit.ProcessState.Sys().(syscall.WaitStatus); !ok || status.Signal() != syscall.SIGKILL {
		t.Fatalf("unexpected child exit: %v %s", err, output)
	}
	a, err := New(Config{DataDir: dir, PublicOrigin: "https://drop.test", AdminOrigin: "https://admin.test", HeadroomBytes: 1}, testVerifier{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	a.mu.Lock()
	defer a.mu.Unlock()
	ids, err := a.ids("SELECT id FROM files WHERE status='ready'")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("recovery published %d files", len(ids))
	}
	b, err := os.ReadFile(a.completed(ids[0]))
	if err != nil || string(b) != "abc" {
		t.Fatalf("recovered bytes %q: %v", b, err)
	}
	s, err := a.settings()
	if err != nil {
		t.Fatal(err)
	}
	if s.StoredBytes != 3 || s.ReservedBytes != 0 {
		t.Fatalf("recovered accounting %+v", s)
	}
	if err = a.reconcile(); err != nil {
		t.Fatal(err)
	}
}
