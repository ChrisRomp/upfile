package app

import (
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const MaxSafeInteger int64 = 9007199254740991

type Config struct {
	DataDir       string
	PublicOrigin  string
	AdminOrigin   string
	ChunkBytes    int64
	HeadroomBytes int64
	MaxRecords    int
	MaxActive     int
	Lease         time.Duration
	SessionTTL    time.Duration
	Retention     time.Duration
}

func (c *Config) defaults() error {
	if c.DataDir == "" {
		return errors.New("data directory is required")
	}
	for _, origin := range []string{c.PublicOrigin, c.AdminOrigin} {
		if !exactHTTPSOrigin(origin) {
			return errors.New("public and admin origins must be canonical HTTPS origins without paths, queries, fragments, or the default :443 port")
		}
	}
	if c.PublicOrigin == c.AdminOrigin {
		return errors.New("public and admin origins must differ")
	}
	if c.ChunkBytes == 0 {
		c.ChunkBytes = 16 << 20
	}
	if c.HeadroomBytes == 0 {
		c.HeadroomBytes = 256 << 20
	}
	if c.MaxRecords == 0 {
		c.MaxRecords = 100000
	}
	if c.MaxActive == 0 {
		c.MaxActive = 8
	}
	if c.Lease == 0 {
		c.Lease = 5 * time.Minute
	}
	if c.SessionTTL == 0 {
		c.SessionTTL = 24 * time.Hour
	}
	if c.Retention == 0 {
		c.Retention = 24 * time.Hour
	}
	if c.ChunkBytes < 64<<10 || c.ChunkBytes > 64<<20 || c.HeadroomBytes < 0 || c.MaxRecords < 1 || c.MaxActive < 1 ||
		c.Lease < time.Second || c.SessionTTL < time.Minute || c.Retention < c.Lease {
		return errors.New("invalid resource limits")
	}
	return nil
}

func exactHTTPSOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.ForceQuery || origin != "https://"+u.Host {
		return false
	}
	host := u.Hostname()
	canonicalHost := host
	if ip, err := netip.ParseAddr(host); err == nil {
		if ip.Zone() != "" {
			return false
		}
		canonicalHost = ip.String()
		if ip.Is6() {
			if ip.Is4In6() {
				// Browsers serialize mapped IPv4 addresses as hexadecimal IPv6.
				b := ip.As16()
				canonicalHost = fmt.Sprintf("::ffff:%x:%x", uint16(b[12])<<8|uint16(b[13]), uint16(b[14])<<8|uint16(b[15]))
			}
			canonicalHost = "[" + canonicalHost + "]"
		}
	} else {
		// Browser hosts are lowercase ASCII; international names need punycode.
		if host == "" || strings.ContainsAny(host, "#%/:<>?@[\\]^|") {
			return false
		}
		for _, c := range host {
			if c <= ' ' || c > '~' || c >= 'A' && c <= 'Z' {
				return false
			}
		}
		last := strings.TrimSuffix(host, ".")
		last = last[strings.LastIndexByte(last, '.')+1:]
		// Browsers interpret a numeric final label as IPv4, including legacy
		// shortened, octal and hexadecimal forms that netip correctly rejects.
		if last != "" && (strings.Trim(last, "0123456789") == "" ||
			strings.HasPrefix(last, "0x") && strings.Trim(last[2:], "0123456789abcdef") == "") {
			return false
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.ParseUint(port, 10, 16)
		if err != nil || n == 443 {
			return false
		}
		canonicalHost += ":" + strconv.FormatUint(n, 10)
	}
	return u.Host == canonicalHost
}
