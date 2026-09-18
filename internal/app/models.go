package app

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Settings struct {
	Configured       bool  `json:"configured"`
	MaxFileBytes     int64 `json:"max_file_bytes"`
	StorageBudget    int64 `json:"storage_budget_bytes"`
	DefaultLinkHours int64 `json:"default_link_hours"`
	StoredBytes      int64 `json:"stored_bytes"`
	ReservedBytes    int64 `json:"reserved_bytes"`
	ChunkBytes       int64 `json:"chunk_bytes"`
	LeaseSeconds     int64 `json:"lease_seconds"`
	MaxRecords       int   `json:"max_records"`
	RecordCount      int   `json:"record_count"`
	CleanupErrors    int   `json:"cleanup_errors"`
}

type Container struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Instructions    string `json:"instructions"`
	MaxFileBytes    *int64 `json:"max_file_bytes"`
	CreatedAt       int64  `json:"created_at"`
	LastActivity    int64  `json:"last_activity"`
	Status          string `json:"status"`
	FileCount       int    `json:"file_count"`
	StoredBytes     int64  `json:"stored_bytes"`
	ActiveUploads   int    `json:"active_uploads"`
	ActiveDownloads int    `json:"active_downloads"`
	LinkCount       int    `json:"link_count"`
	EffectiveMax    int64  `json:"effective_max_bytes"`
}

type CreatedContainer struct {
	Container
	InitialLink Link `json:"initial_link"`
}

type Link struct {
	ID           string `json:"id"`
	ContainerID  string `json:"container_id"`
	SenderLabel  string `json:"sender_label"`
	ExpiresAt    int64  `json:"expires_at"`
	MaxFileBytes *int64 `json:"max_file_bytes"`
	CreatedAt    int64  `json:"created_at"`
	Status       string `json:"status"`
	FileCount    int    `json:"file_count"`
	Active       int    `json:"active_uploads"`
	EffectiveMax int64  `json:"effective_max_bytes"`
	URL          string `json:"url,omitempty"`
	Hash         string `json:"-"`
	Revoked      bool   `json:"-"`
}

type Attempt struct {
	ID          string `json:"id"`
	Status      string `json:"status"`
	Size        int64  `json:"size"`
	Offset      int64  `json:"offset"`
	UploadURL   string `json:"upload_url"`
	CreatedAt   int64  `json:"created_at"`
	LinkID      string `json:"-"`
	SessionHash string `json:"-"`
	Key         string `json:"-"`
	Name        string `json:"-"`
	Comment     string `json:"-"`
	Last        int64  `json:"-"`
	Reserved    int64  `json:"-"`
	Cleanup     bool   `json:"-"`
}

type File struct {
	ID           string `json:"id"`
	ContainerID  string `json:"container_id"`
	LinkID       string `json:"link_id"`
	Name         string `json:"name"`
	OriginalName string `json:"original_name"`
	SenderLabel  string `json:"sender_label"`
	Comment      string `json:"comment"`
	Size         int64  `json:"size"`
	CreatedAt    int64  `json:"created_at"`
	Status       string `json:"status"`
}

type apiError struct {
	status int
	code   string
	msg    string
}

func (e *apiError) Error() string                { return e.msg }
func problem(status int, code, msg string) error { return &apiError{status, code, msg} }
func invalid(msg string) error                   { return problem(400, "invalid_input", msg) }
func unavailable() error {
	return problem(410, "link_unavailable", "This link is expired, revoked, or unavailable.")
}

func opaque(n int) string {
	return hex.EncodeToString(randomBytes(n))
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	// crypto/rand.Read terminates the process if secure entropy is unavailable.
	_, _ = rand.Read(b)
	return b
}

func digest(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func validID(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}

func validText(s string, max int, multiline bool) bool {
	if len(s) > max || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) && !(multiline && (r == '\n' || r == '\t' || r == '\r')) {
			return false
		}
	}
	return true
}

func validName(s string) bool {
	return strings.TrimSpace(s) != "" && validText(s, 255, false) &&
		s != "." && s != ".." && filepath.Base(s) == s && !strings.ContainsAny(s, "/\\")
}

func validOverride(v *int64, parent int64) bool {
	return v == nil || (*v > 0 && *v <= parent)
}

func effective(global int64, values ...*int64) int64 {
	for _, value := range values {
		if value != nil && *value < global {
			global = *value
		}
	}
	return global
}

func classify(err error) (int, string, string) {
	var e *apiError
	if errors.As(err, &e) {
		return e.status, e.code, e.msg
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 404, "not_found", "The requested item was not found."
	}
	return http.StatusInternalServerError, "internal", "The operation failed. Please retry or contact the administrator."
}
