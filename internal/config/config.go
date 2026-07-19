// Package config loads runtime configuration from the environment.
//
// Everything is env-driven so the same binary runs unchanged in local dev and
// in compose. No secrets are ever committed; see .env.example for the shape.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// BlobBackend selects which blob store implementation to construct.
type BlobBackend string

const (
	// BlobFS writes blobs to a local directory. Default for dev and tests.
	BlobFS BlobBackend = "fs"
	// BlobS3 writes blobs to an S3-compatible store (MinIO in compose).
	BlobS3 BlobBackend = "s3"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	DatabaseURL string
	Blob        BlobConfig
	Log         LogConfig
	// ADBPath is the adb executable. Overridable so a vendored
	// platform-tools install can be used without touching PATH.
	ADBPath string
}

// BlobConfig configures the screenshot object store.
type BlobConfig struct {
	Backend BlobBackend

	// FSRoot is the directory root when Backend is BlobFS.
	FSRoot string

	// S3* apply when Backend is BlobS3.
	S3Endpoint  string
	S3AccessKey string
	S3SecretKey string
	S3Bucket    string
	S3UseSSL    bool
}

// LogConfig configures the slog handler.
type LogConfig struct {
	Level  slog.Level
	Format string // "text" or "json"
}

// Load reads configuration from the environment, applying defaults suited to
// local development. It returns an error rather than falling back to a
// surprising default when a value is present but unparseable — a typo in
// LW_LOG_LEVEL should be loud, not silently ignored.
func Load() (Config, error) {
	cfg := Config{
		DatabaseURL: env("LW_DATABASE_URL", "postgres://lw:lw@localhost:5432/lw_manager?sslmode=disable"),
		ADBPath:     env("LW_ADB_PATH", "adb"),
		Blob: BlobConfig{
			Backend:     BlobBackend(env("LW_BLOB_BACKEND", string(BlobFS))),
			FSRoot:      env("LW_BLOB_FS_ROOT", "./data/blobs"),
			S3Endpoint:  env("LW_S3_ENDPOINT", "localhost:9000"),
			S3AccessKey: env("LW_S3_ACCESS_KEY", "minioadmin"),
			S3SecretKey: env("LW_S3_SECRET_KEY", "minioadmin"),
			S3Bucket:    env("LW_S3_BUCKET", "screenshots"),
		},
		Log: LogConfig{
			Format: env("LW_LOG_FORMAT", "text"),
		},
	}

	switch cfg.Blob.Backend {
	case BlobFS, BlobS3:
	default:
		return Config{}, fmt.Errorf("config: LW_BLOB_BACKEND must be %q or %q, got %q", BlobFS, BlobS3, cfg.Blob.Backend)
	}

	useSSL, err := strconv.ParseBool(env("LW_S3_USE_SSL", "false"))
	if err != nil {
		return Config{}, fmt.Errorf("config: parsing LW_S3_USE_SSL: %w", err)
	}
	cfg.Blob.S3UseSSL = useSSL

	level, err := parseLevel(env("LW_LOG_LEVEL", "info"))
	if err != nil {
		return Config{}, err
	}
	cfg.Log.Level = level

	switch cfg.Log.Format {
	case "text", "json":
	default:
		return Config{}, fmt.Errorf("config: LW_LOG_FORMAT must be \"text\" or \"json\", got %q", cfg.Log.Format)
	}

	return cfg, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("config: unknown LW_LOG_LEVEL %q", s)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}
