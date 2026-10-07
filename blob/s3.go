package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3Config is what S3Store needs. Credentials are the Core's own (Vault via
// ESO); never a shared admin key.
type S3Config struct {
	// Endpoint is host[:port], without scheme, e.g.
	// "minio.data.svc.cluster.local:9000" or "s3.amazonaws.com".
	Endpoint string
	// Secure selects https.
	Secure    bool
	Region    string
	Bucket    string
	AccessKey string
	SecretKey string
	// Core is the first segment of every object name, "<core>/<key>". It is
	// what the credential's policy is written against.
	Core string
	// PublicEndpoint, when set, is the host[:port] presigned URLs are signed
	// for: the browser reaches the store by another name than the Core does
	// (an ingress in front of an in-cluster MinIO). Signing is done for that
	// host, so the URL is valid where it is used.
	PublicEndpoint string
	PublicSecure   bool
}

// S3Store is the production Store and Presigner over any S3-compatible
// store.
type S3Store struct {
	client  *minio.Client
	signer  *minio.Client
	bucket  string
	core    string
	timeout time.Duration
}

// NewS3 builds an S3Store. It does not contact the store: a store that is
// down at startup must not keep the Core from serving what does not need it.
func NewS3(cfg S3Config) (*S3Store, error) {
	if cfg.Endpoint == "" || cfg.Bucket == "" || cfg.Core == "" {
		return nil, errors.New("blob: S3Config needs Endpoint, Bucket and Core")
	}
	if strings.ContainsAny(cfg.Core, "/\\ ") || cfg.Core == "." || cfg.Core == ".." {
		return nil, errors.New("blob: S3Config.Core must be a single path segment")
	}
	newClient := func(endpoint string, secure bool) (*minio.Client, error) {
		return minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
			Secure: secure,
			Region: cfg.Region,
		})
	}
	c, err := newClient(cfg.Endpoint, cfg.Secure)
	if err != nil {
		return nil, fmt.Errorf("blob: s3 client: %w", err)
	}
	signer := c
	if cfg.PublicEndpoint != "" {
		if signer, err = newClient(cfg.PublicEndpoint, cfg.PublicSecure); err != nil {
			return nil, fmt.Errorf("blob: s3 public client: %w", err)
		}
	}
	return &S3Store{client: c, signer: signer, bucket: cfg.Bucket, core: cfg.Core, timeout: 30 * time.Second}, nil
}

func (s *S3Store) object(ctx context.Context, key string) (string, error) {
	if err := CheckKey(ctx, key); err != nil {
		return "", err
	}
	return s.core + "/" + key, nil
}

// Put implements Store.
func (s *S3Store) Put(ctx context.Context, key, contentType string, data []byte) error {
	obj, err := s.object(ctx, key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if _, err := s.client.PutObject(ctx, s.bucket, obj, bytes.NewReader(data), int64(len(data)),
		minio.PutObjectOptions{ContentType: contentType}); err != nil {
		return fmt.Errorf("blob: put %s: %w", key, err)
	}
	return nil
}

// Get implements Store. A missing key is ErrNotFound.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, string, error) {
	obj, err := s.object(ctx, key)
	if err != nil {
		return nil, "", err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	o, err := s.client.GetObject(ctx, s.bucket, obj, minio.GetObjectOptions{})
	if err != nil {
		return nil, "", s.wrap(key, err)
	}
	defer o.Close()
	info, err := o.Stat()
	if err != nil {
		return nil, "", s.wrap(key, err)
	}
	data, err := io.ReadAll(o)
	if err != nil {
		return nil, "", s.wrap(key, err)
	}
	return data, info.ContentType, nil
}

// Delete implements Store. S3 deletes are idempotent already.
func (s *S3Store) Delete(ctx context.Context, key string) error {
	obj, err := s.object(ctx, key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if err := s.client.RemoveObject(ctx, s.bucket, obj, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("blob: delete %s: %w", key, err)
	}
	return nil
}

// PresignGet implements Presigner.
func (s *S3Store) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	obj, err := s.object(ctx, key)
	if err != nil {
		return "", err
	}
	if err := checkTTL(ttl); err != nil {
		return "", err
	}
	u, err := s.signer.PresignedGetObject(ctx, s.bucket, obj, ttl, url.Values{})
	if err != nil {
		return "", fmt.Errorf("blob: presign get %s: %w", key, err)
	}
	return u.String(), nil
}

// PresignPut implements Presigner. The content type is not part of the
// signature (S3 presigned PUTs sign the URL, not the headers the browser
// sends); the Core records the type it expects in the metadata row and
// checks it when it reads the object back.
func (s *S3Store) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	obj, err := s.object(ctx, key)
	if err != nil {
		return "", err
	}
	if err := checkTTL(ttl); err != nil {
		return "", err
	}
	u, err := s.signer.PresignedPutObject(ctx, s.bucket, obj, ttl)
	if err != nil {
		return "", fmt.Errorf("blob: presign put %s: %w", key, err)
	}
	return u.String(), nil
}

func (s *S3Store) wrap(key string, err error) error {
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	return fmt.Errorf("blob: get %s: %w", key, err)
}
