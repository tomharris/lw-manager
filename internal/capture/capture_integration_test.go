//go:build integration

package capture

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"testing"

	"github.com/tomharris/lw-manager/internal/blob"
	"github.com/tomharris/lw-manager/internal/config"
	"github.com/tomharris/lw-manager/internal/db"
	"github.com/tomharris/lw-manager/internal/dbtest"
	"github.com/tomharris/lw-manager/internal/transport"
)

// Exercises the real M0 capture path against real Postgres and real MinIO,
// substituting only the device. This is the M0 gate minus the emulator.
func TestCaptureEndToEnd(t *testing.T) {
	ctx := context.Background()

	url, err := dbtest.Prepare(ctx, db.Migrate)
	if err != nil {
		t.Fatalf("dbtest.Prepare(): %v", err)
	}
	pool, err := db.Connect(ctx, url)
	if err != nil {
		t.Fatalf("Connect(): %v", err)
	}
	t.Cleanup(pool.Close)

	if _, err := pool.Exec(ctx, `DELETE FROM devices WHERE serial = 'e2e-emulator'`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	dev, err := pool.UpsertDevice(ctx, "e2e-emulator", "adb", 1080, 2400)
	if err != nil {
		t.Fatalf("UpsertDevice(): %v", err)
	}

	var appInstanceID, accountID int64
	if err := pool.QueryRow(ctx,
		`INSERT INTO app_instances (device_id) VALUES ($1) RETURNING id`, dev.ID,
	).Scan(&appInstanceID); err != nil {
		t.Fatalf("insert app_instance: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`INSERT INTO accounts (app_instance_id, nickname, role) VALUES ($1, 'e2ealt', 'alliance_data') RETURNING id`,
		appInstanceID,
	).Scan(&accountID); err != nil {
		t.Fatalf("insert account: %v", err)
	}

	blobs, err := blob.NewS3Store(ctx, config.BlobConfig{
		Backend:     config.BlobS3,
		S3Endpoint:  "localhost:9000",
		S3AccessKey: "minioadmin",
		S3SecretKey: "minioadmin",
		S3Bucket:    "screenshots-test",
	})
	if err != nil {
		t.Fatalf("NewS3Store(): %v", err)
	}

	// A frame the device claims is 1080x2400, matching the registered device.
	img := image.NewRGBA(image.Rect(0, 0, 1080, 2400))
	for y := 0; y < 2400; y += 7 {
		for x := 0; x < 1080; x += 5 {
			img.Set(x, y, color.RGBA{R: uint8(x % 251), G: uint8(y % 253), B: 90, A: 255})
		}
	}
	rt, err := transport.NewReplayTransportFromImages(img)
	if err != nil {
		t.Fatalf("NewReplayTransportFromImages(): %v", err)
	}

	svc := New(pool, blobs, func(context.Context, db.CaptureTarget) (transport.Transport, error) {
		return rt, nil
	})

	res, err := svc.Capture(ctx, accountID)
	if err != nil {
		t.Fatalf("Capture(): %v", err)
	}
	if res.ScreenshotID == 0 {
		t.Fatal("no screenshot row was written")
	}

	// The row must exist in Postgres with the digest we recorded.
	var key, sum string
	if err := pool.QueryRow(ctx,
		`SELECT object_key, sha256 FROM screenshots WHERE id = $1`, res.ScreenshotID,
	).Scan(&key, &sum); err != nil {
		t.Fatalf("reading back screenshot row: %v", err)
	}
	if key != res.ObjectKey || sum != res.SHA256 {
		t.Errorf("row has key=%q sum=%q, want key=%q sum=%q", key, sum, res.ObjectKey, res.SHA256)
	}

	// And the object must be in MinIO, decoding to the resolution we captured.
	data, err := blob.GetContent(ctx, blobs, key, sum)
	if err != nil {
		t.Fatalf("fetching stored blob: %v", err)
	}
	decoded, _, err := image.Decode(newReader(data))
	if err != nil {
		t.Fatalf("stored blob is not a decodable image: %v", err)
	}
	if b := decoded.Bounds(); b.Dx() != 1080 || b.Dy() != 2400 {
		t.Errorf("stored image is %dx%d, want 1080x2400", b.Dx(), b.Dy())
	}
}

func newReader(b []byte) *bytes.Reader { return bytes.NewReader(b) }
