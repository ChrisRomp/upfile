package app

import (
	"net/url"
	"testing"
)

func TestConfigExactHTTPSOrigins(t *testing.T) {
	tests := []struct {
		name   string
		origin string
		valid  bool
	}{
		{"production", "https://drop.example.com", true},
		{"production admin", "https://admin.example.com", true},
		{"test domain", "https://drop.test", true},
		{"localhost", "https://localhost", true},
		{"development public", "https://localhost:8443", true},
		{"development admin", "https://localhost:8444", true},
		{"non-default port", "https://drop.example.com:444", true},
		{"maximum port", "https://drop.example.com:65535", true},
		{"zero port", "https://drop.example.com:0", true},
		{"IPv4 loopback", "https://127.0.0.1:8443", true},
		{"IPv6 loopback", "https://[::1]:8444", true},
		{"IPv6 unspecified", "https://[::]", true},
		{"IPv6", "https://[2001:db8::1]", true},
		{"IPv6 mapped IPv4", "https://[::ffff:c000:201]", true},
		{"punycode", "https://xn--bcher-kva.example", true},
		{"trailing domain dot", "https://drop.example.com.", true},
		{"nonnumeric final label", "https://drop.123abc", true},
		{"empty", "", false},
		{"HTTP", "http://drop.example.com", false},
		{"missing host", "https://", false},
		{"opaque URL", "https:drop.example.com", false},
		{"scheme case", "HTTPS://drop.example.com", false},
		{"host case", "https://Drop.Example.com", false},
		{"userinfo", "https://user@drop.example.com", false},
		{"empty userinfo", "https://@drop.example.com", false},
		{"path", "https://drop.example.com/path", false},
		{"trailing slash", "https://drop.example.com/", false},
		{"escaped path", "https://drop.example.com/%2f", false},
		{"query", "https://drop.example.com?key=value", false},
		{"empty query", "https://drop.example.com?", false},
		{"fragment", "https://drop.example.com#fragment", false},
		{"empty fragment", "https://drop.example.com#", false},
		{"empty query and fragment", "https://drop.example.com?#", false},
		{"default port", "https://drop.example.com:443", false},
		{"empty port", "https://drop.example.com:", false},
		{"port leading zero", "https://drop.example.com:08443", false},
		{"port overflow", "https://drop.example.com:65536", false},
		{"non-numeric port", "https://drop.example.com:https", false},
		{"Unicode host", "https://bücher.example", false},
		{"escaped host", "https://%64rop.example.com", false},
		{"escaped Unicode host", "https://b%C3%BCcher.example", false},
		{"forbidden host character", "https://drop^example.com", false},
		{"backslash", "https://drop.example.com\\path", false},
		{"leading whitespace", " https://drop.example.com", false},
		{"trailing whitespace", "https://drop.example.com ", false},
		{"IPv4 shortened", "https://127.1", false},
		{"IPv4 integer", "https://2130706433", false},
		{"IPv4 octal", "https://0177.0.0.1", false},
		{"IPv4 hexadecimal", "https://0x7f000001", false},
		{"IPv4 trailing dot", "https://127.0.0.1.", false},
		{"IPv4 overflow", "https://256.0.0.1", false},
		{"numeric domain suffix", "https://drop.123", false},
		{"hexadecimal domain suffix", "https://drop.0xff", false},
		{"empty hexadecimal suffix", "https://drop.0x", false},
		{"unbracketed IPv6", "https://::1", false},
		{"expanded IPv6", "https://[0:0:0:0:0:0:0:1]", false},
		{"IPv6 leading zeros", "https://[2001:0db8::1]", false},
		{"IPv6 uppercase", "https://[2001:DB8::1]", false},
		{"IPv6 incorrect compression", "https://[2001:0:0:1::1:1]", false},
		{"IPv6 dotted IPv4", "https://[::ffff:192.0.2.1]", false},
		{"IPv6 zone", "https://[fe80::1%25en0]", false},
		{"bracketed hostname", "https://[drop.example.com]", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, field := range []string{"public", "admin"} {
				t.Run(field, func(t *testing.T) {
					cfg := Config{DataDir: ".", PublicOrigin: "https://public.test", AdminOrigin: "https://administrator.test"}
					if field == "public" {
						cfg.PublicOrigin = test.origin
					} else {
						cfg.AdminOrigin = test.origin
					}
					originalPublic, originalAdmin := cfg.PublicOrigin, cfg.AdminOrigin
					err := cfg.defaults()
					if (err == nil) != test.valid {
						t.Fatalf("defaults() for %q: error = %v, want valid = %v", test.origin, err, test.valid)
					}
					if cfg.PublicOrigin != originalPublic || cfg.AdminOrigin != originalAdmin {
						t.Fatal("defaults changed an origin instead of validating it")
					}
					if test.valid {
						u, err := url.Parse(test.origin + "/api/links/test/uploads/test")
						if err != nil || u.Path != "/api/links/test/uploads/test" || u.RawQuery != "" || u.Fragment != "" {
							t.Fatalf("origin is not safe for upload URL concatenation: %v, %v", u, err)
						}
					}
				})
			}
		})
	}
}

func TestConfigOriginsMustDiffer(t *testing.T) {
	cfg := Config{DataDir: ".", PublicOrigin: "https://drop.test", AdminOrigin: "https://drop.test"}
	if err := cfg.defaults(); err == nil {
		t.Fatal("identical public and admin origins accepted")
	}
}
