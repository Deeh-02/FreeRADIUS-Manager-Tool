package handlers

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"github.com/freeradius-manager/backend/internal/settings"
	"github.com/gin-gonic/gin"
)

// ─────────────────────────────────────────────────────────────────────────────
// Database backups
// ─────────────────────────────────────────────────────────────────────────────
//
// pg_dump (installed in the backend image) writes a gzipped plain-SQL dump to BACKUP_DIR
// (default /backups, a host folder in docker-compose.yml). The backup_database scheduled task
// runs daily and deletes dumps older than backup_retention_days. Staff can also start one and
// download it from the Settings page.
//
// A backup on the same disk as the database does not survive losing that disk: copy the
// files off the server (download them, or sync ./backups elsewhere). Restoring is a manual,
// deliberate step (see RestoreBackup); the API never overwrites the live database.

var (
	backupMu      sync.Mutex
	backupNameRe  = regexp.MustCompile(`^radius_backup_\d{8}_\d{6}\.sql\.gz$`)
	backupRunning bool
	lastBackupErr string
)

func backupDir() string {
	if d := os.Getenv("BACKUP_DIR"); d != "" {
		return d
	}
	return "/backups"
}

// runBackup dumps the database. Only one dump runs at a time.
func runBackup() (string, error) {
	backupMu.Lock()
	if backupRunning {
		backupMu.Unlock()
		return "", errors.New("a backup is already running")
	}
	backupRunning = true
	backupMu.Unlock()
	defer func() { backupMu.Lock(); backupRunning = false; backupMu.Unlock() }()

	name, err := dumpDatabase()
	backupMu.Lock()
	if err != nil {
		lastBackupErr = time.Now().Format(time.RFC3339) + ": " + err.Error()
	} else {
		lastBackupErr = ""
	}
	backupMu.Unlock()
	return name, err
}

func dumpDatabase() (string, error) {
	dir := backupDir()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return "", fmt.Errorf("backup folder %s: %w", dir, err)
	}
	if _, err := exec.LookPath("pg_dump"); err != nil {
		return "", errors.New("pg_dump is not installed in this environment")
	}
	name := "radius_backup_" + time.Now().Format("20060102_150405") + ".sql.gz"
	final := filepath.Join(dir, name)
	tmp := final + ".part"

	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return "", err
	}
	gz := gzip.NewWriter(f)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	cmd := exec.CommandContext(ctx, "pg_dump", "--no-owner", "--no-privileges", "--clean", "--if-exists")
	cmd.Env = append(os.Environ(),
		"PGHOST="+envOr("DB_HOST", "localhost"), "PGPORT="+envOr("DB_PORT", "5432"),
		"PGUSER="+envOr("DB_USER", "radius_user"), "PGPASSWORD="+os.Getenv("DB_PASSWORD"),
		"PGDATABASE="+envOr("DB_NAME", "radius"), "PGSSLMODE="+envOr("DB_SSL_MODE", "disable"))
	cmd.Stdout = gz
	var stderr limitedBuffer
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	closeErr := gz.Close()
	if err := f.Close(); err != nil && closeErr == nil {
		closeErr = err
	}
	if runErr != nil || closeErr != nil {
		os.Remove(tmp)
		if runErr != nil {
			return "", fmt.Errorf("pg_dump failed: %v %s", runErr, stderr.String())
		}
		return "", closeErr
	}
	if err := os.Rename(tmp, final); err != nil {
		return "", err
	}
	return name, nil
}

// pruneBackups deletes automatic dumps older than retentionDays (0 keeps everything).
func pruneBackups(retentionDays int) int {
	if retentionDays <= 0 {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -retentionDays)
	removed := 0
	for _, b := range listBackupFiles() {
		if b.Modified.Before(cutoff) {
			if os.Remove(filepath.Join(backupDir(), b.Name)) == nil {
				removed++
			}
		}
	}
	return removed
}

type backupFile struct {
	Name     string    `json:"name"`
	Size     int64     `json:"size_bytes"`
	Modified time.Time `json:"created_at"`
}

func listBackupFiles() []backupFile {
	out := []backupFile{}
	entries, err := os.ReadDir(backupDir())
	if err != nil {
		return out
	}
	for _, e := range entries {
		if !backupNameRe.MatchString(e.Name()) {
			continue
		}
		if info, err := e.Info(); err == nil {
			out = append(out, backupFile{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out
}

// CreateBackup starts a dump in the background (it can take longer than an HTTP request).
func (h *Handler) CreateBackup(c *gin.Context) {
	go func() {
		name, err := runBackup()
		if err != nil {
			h.log.WithError(err).Error("backup failed")
			return
		}
		h.log.Infof("backup written: %s", name)
	}()
	c.JSON(http.StatusAccepted, gin.H{"message": "backup started; it appears in the list when finished"})
}

// ListBackups lists the dumps in BACKUP_DIR.
func (h *Handler) ListBackups(c *gin.Context) {
	backupMu.Lock()
	running, lastErr := backupRunning, lastBackupErr
	backupMu.Unlock()
	c.JSON(http.StatusOK, gin.H{
		"backups":        listBackupFiles(),
		"running":        running,
		"last_error":     lastErr,
		"directory":      backupDir(),
		"retention_days": settings.Int("backup_retention_days", "BACKUP_RETENTION_DAYS", 14),
	})
}

// DownloadBackup streams one dump so it can be kept off the server.
func (h *Handler) DownloadBackup(c *gin.Context) {
	name := c.Param("name")
	if !backupNameRe.MatchString(name) {
		respondError(c, http.StatusBadRequest, "invalid backup name")
		return
	}
	f, err := os.Open(filepath.Join(backupDir(), name))
	if err != nil {
		respondError(c, http.StatusNotFound, "backup not found")
		return
	}
	defer f.Close()
	// Large files outlast the server's 30s write timeout; lift it for this response.
	http.NewResponseController(c.Writer).SetWriteDeadline(time.Time{})
	c.Header("Content-Type", "application/gzip")
	c.Header("Content-Disposition", `attachment; filename="`+name+`"`)
	io.Copy(c.Writer, f)
}

// DeleteBackup removes one dump.
func (h *Handler) DeleteBackup(c *gin.Context) {
	name := c.Param("name")
	if !backupNameRe.MatchString(name) {
		respondError(c, http.StatusBadRequest, "invalid backup name")
		return
	}
	if err := os.Remove(filepath.Join(backupDir(), name)); err != nil {
		respondError(c, http.StatusNotFound, "backup not found")
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "backup deleted"})
}

// RestoreBackup explains how to restore. Restoring replaces every table, so it is done by an
// operator on the server, never through the web API.
func (h *Handler) RestoreBackup(c *gin.Context) {
	c.JSON(http.StatusOK, gin.H{
		"message": "Restore on the server: docker compose stop backend freeradius && " +
			"gunzip -c backups/<file>.sql.gz | docker exec -i radius_postgres psql -U <DB_USER> -d <DB_NAME> && " +
			"docker compose start backend freeradius",
	})
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// limitedBuffer keeps the first 4 KB written (pg_dump's error output).
type limitedBuffer struct{ b []byte }

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := 4096 - len(l.b); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		l.b = append(l.b, p[:room]...)
	}
	return len(p), nil
}
func (l *limitedBuffer) String() string { return string(l.b) }
