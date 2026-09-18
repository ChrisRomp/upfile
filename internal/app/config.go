package app

import (
	"errors"
	"net/url"
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
		u, err := url.Parse(origin)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("public and admin origins must be exact HTTPS origins without paths")
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
