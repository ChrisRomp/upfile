package app

import (
	"strings"
	"testing"
	"time"
)

func TestInvalidExchangesDoNotConsumeAuthenticatedAllowance(t *testing.T) {
	for _, kind := range []string{"malformed ID", "unknown ID", "wrong secret", "short secret"} {
		t.Run(kind, func(t *testing.T) {
			h := setup(t)
			_, link, cookie := h.link(nil)
			id, secret := link.ID, strings.Split(link.URL, "#")[1]
			validSecret := secret
			switch kind {
			case "malformed ID":
				id = "invalid"
			case "unknown ID":
				id = strings.Repeat("f", 32)
			case "wrong secret":
				secret = strings.Repeat("0", 64)
			case "short secret":
				secret = "invalid"
			}
			h.a.mu.Lock()
			before := h.a.rates["exchange:"+link.ID]
			h.a.mu.Unlock()
			for i := 0; i < 121; i++ {
				h.call(false, "POST", "/api/links/"+id+"/exchange", map[string]string{"secret": secret}, nil, 410)
			}
			h.a.mu.Lock()
			after := h.a.rates["exchange:"+link.ID]
			buckets := len(h.a.rates)
			h.a.mu.Unlock()
			if after != before || buckets != 1 {
				t.Fatalf("invalid exchanges consumed allowance: before=%+v after=%+v buckets=%d", before, after, buckets)
			}
			h.call(false, "POST", "/api/links/"+link.ID+"/exchange", map[string]string{"secret": validSecret}, nil, 200)
			h.call(false, "GET", "/api/links/"+link.ID, nil, cookie, 200)
		})
	}
}

func TestExchangeLimitsArePerLinkAndExpire(t *testing.T) {
	h := setup(t)
	now := time.Now()
	h.a.mu.Lock()
	h.a.now = func() time.Time { return now }
	h.a.mu.Unlock()
	_, first, firstCookie := h.link(nil)
	_, second, secondCookie := h.link(nil)
	secret := strings.Split(first.URL, "#")[1]
	// The helper already used one exchange to obtain the cookie.
	for i := 1; i < 120; i++ {
		h.call(false, "POST", "/api/links/"+first.ID+"/exchange", map[string]string{"secret": secret}, firstCookie, 200)
	}
	w := h.call(false, "POST", "/api/links/"+first.ID+"/exchange", map[string]string{"secret": secret}, nil, 429)
	if w.Header().Get("Retry-After") != "60" {
		t.Fatal("missing retry guidance")
	}
	h.call(false, "GET", "/api/links/"+first.ID, nil, firstCookie, 200)
	h.call(false, "POST", "/api/links/"+second.ID+"/exchange", map[string]string{"secret": strings.Split(second.URL, "#")[1]}, secondCookie, 200)
	h.a.mu.Lock()
	now = now.Add(time.Minute)
	h.a.mu.Unlock()
	h.call(false, "POST", "/api/links/"+first.ID+"/exchange", map[string]string{"secret": secret}, nil, 200)
}

func TestRevokedExchangesDoNotAllocateLimiterBuckets(t *testing.T) {
	h := setup(t)
	_, link, _ := h.link(nil)
	h.call(true, "POST", "/api/links/"+link.ID+"/revoke", map[string]any{}, nil, 200)
	h.a.mu.Lock()
	before := h.a.rates["exchange:"+link.ID]
	h.a.mu.Unlock()
	h.call(false, "POST", "/api/links/"+link.ID+"/exchange", map[string]string{"secret": strings.Split(link.URL, "#")[1]}, nil, 410)
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	if h.a.rates["exchange:"+link.ID] != before {
		t.Fatal("revoked exchange consumed rate allowance")
	}
}
