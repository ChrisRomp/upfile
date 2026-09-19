package app

import (
	"strings"
	"testing"
	"time"
)

func TestCreatingContainerIncludesUsableDefaultLink(t *testing.T) {
	h := setup(t)
	h.call(true, "PUT", "/api/settings", map[string]any{
		"max_file_bytes": 1024, "storage_budget_bytes": 4096, "default_link_hours": 48,
	}, nil, 200)
	now := time.Now().Truncate(time.Second)
	h.a.mu.Lock()
	h.a.now = func() time.Time { return now }
	h.a.mu.Unlock()
	created := decode[CreatedContainer](t, h.call(true, "POST", "/api/containers", map[string]any{
		"name": "One-step request", "instructions": "Send files", "max_file_bytes": 512,
	}, nil, 200))
	link := created.InitialLink
	if created.LinkCount != 1 || link.ContainerID != created.ID || link.SenderLabel != created.Name || link.MaxFileBytes != nil || link.EffectiveMax != 512 {
		t.Fatalf("incorrect default link: %+v %+v", created.Container, link)
	}
	if link.ExpiresAt != now.Add(48*time.Hour).Unix() {
		t.Fatal("default expiration was not applied")
	}
	parts := strings.Split(link.URL, "#")
	if len(parts) != 2 || len(parts[1]) != 64 {
		t.Fatal("missing secure upload URL")
	}
	if parts[0] != "https://drop.test/u/"+link.ID {
		t.Fatal("incorrect public URL")
	}
	var hash string
	h.a.mu.Lock()
	err := h.a.db.QueryRow("SELECT hash FROM links WHERE id=?", link.ID).Scan(&hash)
	h.a.mu.Unlock()
	if err != nil || hash != digest(parts[1]) {
		t.Fatal("link secret was not stored hashed")
	}
	exchange := h.call(false, "POST", "/api/links/"+link.ID+"/exchange", map[string]string{"secret": parts[1]}, nil, 200)
	cookies := exchange.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("default link is unusable")
	}
	attempt := h.admit(link, cookies[0], "default-link-file1", 3, 201)
	h.patch(link, cookies[0], attempt, 0, "abc", 204)
	h.call(false, "GET", "/api/links/"+link.ID, nil, cookies[0], 200)

	for _, path := range []string{"/api/containers/" + created.ID, "/api/containers/" + created.ID + "/links"} {
		response := h.call(true, "GET", path, nil, nil, 200)
		if strings.Contains(response.Body.String(), parts[1]) || strings.Contains(response.Body.String(), `"initial_link"`) || strings.Contains(response.Body.String(), `"url"`) {
			t.Fatal("creation secret leaked in a subsequent read")
		}
	}
	edited := decode[Container](t, h.call(true, "PUT", "/api/containers/"+created.ID, map[string]any{
		"name": "Renamed", "instructions": "Updated", "max_file_bytes": 256,
	}, nil, 200))
	if edited.LinkCount != 1 {
		t.Fatal("editing created another link")
	}
	h.a.mu.Lock()
	current, err := h.a.link(link.ID)
	h.a.mu.Unlock()
	if err != nil || current.Hash != hash || current.EffectiveMax != 256 {
		t.Fatal("link did not inherit the edited limit")
	}
	if current.SenderLabel != created.Name {
		t.Fatal("renaming the container unexpectedly renamed its initial link")
	}
}

func TestDefaultLinkCreationRollsBackContainerOnFailure(t *testing.T) {
	h := setup(t)
	h.a.mu.Lock()
	_, err := h.a.db.Exec(`CREATE TRIGGER fail_default_link BEFORE INSERT ON links
	 BEGIN SELECT RAISE(ABORT,'simulated link insert failure'); END`)
	h.a.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	h.call(true, "POST", "/api/containers", map[string]any{"name": "Must roll back", "instructions": "", "max_file_bytes": nil}, nil, 500)
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	for _, table := range []string{"containers", "links"} {
		var count int
		if err = h.a.db.QueryRow("SELECT count(*) FROM " + table).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("partial creation left records in %s", table)
		}
	}
	var events int
	if err = h.a.db.QueryRow("SELECT count(*) FROM audit WHERE action IN ('container.save','link.save')").Scan(&events); err != nil {
		t.Fatal(err)
	}
	if events != 0 {
		t.Fatal("failed transaction left success audit events")
	}
}

func TestContainerCreationReservesCapacityForBothRecords(t *testing.T) {
	h := setup(t)
	h.a.mu.Lock()
	h.a.cfg.MaxRecords = 1
	h.a.mu.Unlock()
	payload := map[string]any{"name": "Capacity boundary", "instructions": "", "max_file_bytes": nil}
	h.call(true, "POST", "/api/containers", payload, nil, 507)
	s := decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.RecordCount != 0 {
		t.Fatal("rejected create left metadata")
	}
	h.a.mu.Lock()
	h.a.cfg.MaxRecords = 2
	h.a.mu.Unlock()
	created := decode[CreatedContainer](t, h.call(true, "POST", "/api/containers", payload, nil, 200))
	if created.LinkCount != 1 {
		t.Fatal("missing link at exact record-capacity boundary")
	}
	s = decode[Settings](t, h.call(true, "GET", "/api/settings", nil, nil, 200))
	if s.RecordCount != 2 {
		t.Fatalf("record count: %d", s.RecordCount)
	}
}
