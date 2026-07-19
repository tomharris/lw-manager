package config

import (
	"log/slog"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() with a clean env: %v", err)
	}
	if cfg.Blob.Backend != BlobFS {
		t.Errorf("default blob backend = %q, want %q", cfg.Blob.Backend, BlobFS)
	}
	if cfg.Log.Level != slog.LevelInfo {
		t.Errorf("default log level = %v, want info", cfg.Log.Level)
	}
	if cfg.ADBPath != "adb" {
		t.Errorf("default adb path = %q, want \"adb\"", cfg.ADBPath)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("LW_BLOB_BACKEND", "s3")
	t.Setenv("LW_LOG_LEVEL", "DEBUG")
	t.Setenv("LW_S3_USE_SSL", "true")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load(): %v", err)
	}
	if cfg.Blob.Backend != BlobS3 {
		t.Errorf("blob backend = %q, want s3", cfg.Blob.Backend)
	}
	if cfg.Log.Level != slog.LevelDebug {
		t.Errorf("log level = %v, want debug", cfg.Log.Level)
	}
	if !cfg.Blob.S3UseSSL {
		t.Error("S3UseSSL = false, want true")
	}
}

// A malformed value must fail loudly rather than silently falling back.
func TestLoadRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ key, val string }{
		{"LW_BLOB_BACKEND", "gcs"},
		{"LW_LOG_LEVEL", "verbose"},
		{"LW_LOG_FORMAT", "xml"},
		{"LW_S3_USE_SSL", "yes-please"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			t.Setenv(tc.key, tc.val)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() with %s=%q returned nil error, want failure", tc.key, tc.val)
			}
		})
	}
}
