package blob

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

func acme() context.Context { return tenantctx.WithTenant(context.Background(), "acme") }

func TestCheckKey(t *testing.T) {
	good := []string{"acme/asset/abc/x.jpg", "acme/purchase/p-1/9f86d081", "acme/x"}
	for _, k := range good {
		if err := CheckKey(acme(), k); err != nil {
			t.Errorf("%q: %v", k, err)
		}
	}
	bad := map[string]string{
		"other tenant":      "globex/asset/abc/x.jpg",
		"tenant prefix":     "acme2/asset/x",
		"tenant only":       "acme",
		"absolute":          "/acme/asset/x",
		"dotdot":            "acme/../globex/x",
		"dot":               "acme/./x",
		"empty segment":     "acme//x",
		"trailing slash":    "acme/x/",
		"backslash":         "acme/x\\..\\y",
		"control":           "acme/x\n",
		"empty":             "",
		"too long":          "acme/" + strings.Repeat("a", 1024),
		"dotdot at the end": "acme/x/..",
	}
	for name, k := range bad {
		if err := CheckKey(acme(), k); !errors.Is(err, ErrInvalidKey) {
			t.Errorf("%s (%q): %v, want ErrInvalidKey", name, k, err)
		}
	}
	if err := CheckKey(context.Background(), "acme/x"); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("no tenant in context: %v", err)
	}
}

// Every Store behaves the same: round trip, ErrNotFound, idempotent delete,
// and the tenant check on every call.
func TestStores(t *testing.T) {
	stores := map[string]Store{"mem": NewMemStore(), "disk": NewDiskStore(t.TempDir())}
	if s := s3FromEnv(t); s != nil {
		stores["s3"] = s
	}
	for name, s := range stores {
		t.Run(name, func(t *testing.T) { exerciseStore(t, s) })
	}
}

func exerciseStore(t *testing.T, s Store) {
	ctx := acme()
	key := "acme/asset/abc/" + time.Now().Format("150405.000000") + ".jpg"
	if err := s.Put(ctx, key, "image/jpeg", []byte("hello")); err != nil {
		t.Fatalf("put: %v", err)
	}
	data, ct, err := s.Get(ctx, key)
	if err != nil || string(data) != "hello" || ct != "image/jpeg" {
		t.Fatalf("get = %q, %q, %v", data, ct, err)
	}
	// Another tenant cannot reach it, not even by naming it.
	globex := tenantctx.WithTenant(context.Background(), "globex")
	if _, _, err := s.Get(globex, key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("cross-tenant get: %v", err)
	}
	if err := s.Delete(globex, key); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("cross-tenant delete: %v", err)
	}
	if err := s.Put(globex, key, "text/plain", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("cross-tenant put: %v", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, _, err := s.Get(ctx, key); !errors.Is(err, ErrNotFound) || !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("get after delete = %v, want ErrNotFound", err)
	}
	if err := s.Delete(ctx, key); err != nil {
		t.Fatalf("second delete: %v", err)
	}
	if err := s.Put(ctx, "acme/../escape", "text/plain", []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("traversal: %v", err)
	}
}

func TestMemStoreCopiesAndPresigns(t *testing.T) {
	m := NewMemStore()
	buf := []byte("abc")
	if err := m.Put(acme(), "acme/a/b", "text/plain", buf); err != nil {
		t.Fatal(err)
	}
	buf[0] = 'X'
	if got, _, _ := m.Get(acme(), "acme/a/b"); string(got) != "abc" {
		t.Fatalf("the store aliases the caller's slice: %q", got)
	}
	u, err := m.PresignGet(acme(), "acme/a/b", time.Minute)
	if err != nil || !strings.HasPrefix(u, "mem:///acme/a/b?") {
		t.Fatalf("PresignGet = %q, %v", u, err)
	}
	for _, ttl := range []time.Duration{0, -time.Second, MaxPresignTTL + time.Second} {
		if _, err := m.PresignPut(acme(), "acme/a/b", "text/plain", ttl); err == nil {
			t.Errorf("ttl %v must be refused", ttl)
		}
	}
	if _, err := m.PresignGet(tenantctx.WithTenant(context.Background(), "globex"), "acme/a/b", time.Minute); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("cross-tenant presign: %v", err)
	}
}

// DiskStore reads what the Cores' copies wrote: same file and sidecar.
func TestDiskStoreLayoutMatchesTheCoresCopies(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "acme", "asset", "abc", "x.jpg")
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	_ = os.WriteFile(p, []byte("old"), 0o644)
	_ = os.WriteFile(p+".ct", []byte("image/jpeg"), 0o644)
	data, ct, err := NewDiskStore(dir).Get(acme(), "acme/asset/abc/x.jpg")
	if err != nil || string(data) != "old" || ct != "image/jpeg" {
		t.Fatalf("Get = %q, %q, %v", data, ct, err)
	}
}

func TestS3ObjectNamesAndPresign(t *testing.T) {
	if _, err := NewS3(S3Config{Endpoint: "minio:9000", Bucket: "b"}); err == nil {
		t.Fatal("a store without Core must be refused")
	}
	if _, err := NewS3(S3Config{Endpoint: "minio:9000", Bucket: "b", Core: "a/b"}); err == nil {
		t.Fatal("a Core with a slash must be refused")
	}
	s, err := NewS3(S3Config{Endpoint: "minio.data:9000", Bucket: "files", Core: "core-maintenance", Region: "us-east-1",
		AccessKey: "ak", SecretKey: "sk", PublicEndpoint: "files.strixapps.com", PublicSecure: true})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := s.PresignGet(acme(), "acme/asset/abc/x.jpg", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(raw)
	if u.Scheme != "https" || u.Host != "files.strixapps.com" || u.Path != "/files/core-maintenance/acme/asset/abc/x.jpg" {
		t.Fatalf("presigned URL = %s", raw)
	}
	if u.Query().Get("X-Amz-Expires") != "300" || u.Query().Get("X-Amz-Signature") == "" {
		t.Fatalf("presigned URL lacks expiry or signature: %s", raw)
	}
	if _, err := s.PresignPut(acme(), "acme/x", "image/png", 2*time.Hour); err == nil {
		t.Fatal("a ttl above MaxPresignTTL must be refused")
	}
	if _, err := s.PresignGet(acme(), "globex/x", time.Minute); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("cross-tenant presign: %v", err)
	}
}

// Against an S3-compatible store (MinIO, RustFS):
//
//	docker run --rm -d --name kit-s3 -p 59000:9000 -e RUSTFS_ACCESS_KEY=kitkey -e RUSTFS_SECRET_KEY=kitsecret123 rustfs/rustfs:latest
//	BLOB_TEST_S3=127.0.0.1:59000 BLOB_TEST_S3_KEY=kitkey BLOB_TEST_S3_SECRET=kitsecret123 go test ./blob -v
func s3FromEnv(t *testing.T) *S3Store {
	ep := os.Getenv("BLOB_TEST_S3")
	if ep == "" {
		return nil
	}
	s, err := NewS3(S3Config{Endpoint: ep, Bucket: "kit-test", Core: "core-test", Region: "us-east-1",
		AccessKey: os.Getenv("BLOB_TEST_S3_KEY"), SecretKey: os.Getenv("BLOB_TEST_S3_SECRET")})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if ok, err := s.client.BucketExists(ctx, "kit-test"); err != nil {
		t.Fatalf("bucket: %v", err)
	} else if !ok {
		if err := s.client.MakeBucket(ctx, "kit-test", minioMakeBucketOptions()); err != nil {
			t.Fatalf("make bucket: %v", err)
		}
	}
	return s
}

// The signed URLs work against the real store, and the object lives under
// the Core's prefix.
func TestS3PresignedRoundTrip(t *testing.T) {
	s := s3FromEnv(t)
	if s == nil {
		t.Skip("BLOB_TEST_S3 not set")
	}
	ctx := acme()
	key := "acme/upload/" + time.Now().Format("150405.000000")
	putURL, err := s.PresignPut(ctx, key, "text/plain", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPut, putURL, strings.NewReader("via url"))
	req.Header.Set("Content-Type", "text/plain")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("presigned PUT = %v, %v", resp, err)
	}
	resp.Body.Close()
	if _, err := s.client.StatObject(context.Background(), "kit-test", "core-test/"+key, minioStatOptions()); err != nil {
		t.Fatalf("object not under the core prefix: %v", err)
	}
	getURL, err := s.PresignGet(ctx, key, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err = http.Get(getURL)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("presigned GET = %v, %v", resp, err)
	}
	resp.Body.Close()
	_ = s.Delete(ctx, key)
}
