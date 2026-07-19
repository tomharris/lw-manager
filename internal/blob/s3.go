package blob

import (
	"context"
	"fmt"
	"io"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/tomharris/lw-manager/internal/config"
)

// S3Store keeps objects in an S3-compatible store. MinIO in compose today;
// the same code works against real S3 if the fleet ever outgrows one box.
type S3Store struct {
	client *minio.Client
	bucket string
}

// NewS3Store connects and ensures the bucket exists.
func NewS3Store(ctx context.Context, cfg config.BlobConfig) (*S3Store, error) {
	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: cfg.S3UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("blob: connecting to %s: %w", cfg.S3Endpoint, err)
	}

	exists, err := client.BucketExists(ctx, cfg.S3Bucket)
	if err != nil {
		return nil, fmt.Errorf("blob: checking bucket %s: %w", cfg.S3Bucket, err)
	}
	if !exists {
		if err := client.MakeBucket(ctx, cfg.S3Bucket, minio.MakeBucketOptions{}); err != nil {
			return nil, fmt.Errorf("blob: creating bucket %s: %w", cfg.S3Bucket, err)
		}
	}

	return &S3Store{client: client, bucket: cfg.S3Bucket}, nil
}

func (s *S3Store) Put(ctx context.Context, key string, r io.Reader) error {
	// Size -1 streams via multipart; screenshots are small but the size is not
	// always known up front when the reader is a pipe.
	_, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{
		ContentType: "image/png",
	})
	if err != nil {
		return fmt.Errorf("blob: putting %s: %w", key, err)
	}
	return nil
}

func (s *S3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("blob: getting %s: %w", key, err)
	}
	// GetObject is lazy: it does not contact the server until first read, so a
	// missing key only surfaces here.
	if _, err := obj.Stat(); err != nil {
		obj.Close()
		if isNoSuchKey(err) {
			return nil, fmt.Errorf("blob: %s: %w", key, ErrNotFound)
		}
		return nil, fmt.Errorf("blob: stat %s: %w", key, err)
	}
	return obj, nil
}

func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if isNoSuchKey(err) {
			return false, nil
		}
		return false, fmt.Errorf("blob: stat %s: %w", key, err)
	}
	return true, nil
}

func isNoSuchKey(err error) bool {
	resp := minio.ToErrorResponse(err)
	return resp.Code == "NoSuchKey" || resp.StatusCode == 404
}

// New builds the store selected by configuration.
func New(ctx context.Context, cfg config.BlobConfig) (Store, error) {
	switch cfg.Backend {
	case config.BlobS3:
		return NewS3Store(ctx, cfg)
	case config.BlobFS:
		return NewFSStore(cfg.FSRoot)
	default:
		return nil, fmt.Errorf("blob: unknown backend %q", cfg.Backend)
	}
}
