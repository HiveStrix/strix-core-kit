package blob

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// DiskStore keeps each blob as a file under a base directory, with a sidecar
// "<file>.ct" holding its content type: the layout of the Cores' copies, so
// a dev directory written by them reads back here.
//
// FOR DEV AND TESTS ONLY. A shared multi-tenant process must not serve
// production bytes off its own disk; that is S3Store.
type DiskStore struct{ base string }

// NewDiskStore opens a store over a directory.
func NewDiskStore(base string) *DiskStore { return &DiskStore{base: base} }

func (d *DiskStore) path(ctx context.Context, key string) (string, error) {
	if err := CheckKey(ctx, key); err != nil {
		return "", err
	}
	return filepath.Join(d.base, filepath.FromSlash(key)), nil
}

// Put implements Store.
func (d *DiskStore) Put(ctx context.Context, key, contentType string, data []byte) error {
	p, err := d.path(ctx, key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(p+".ct", []byte(contentType), 0o644)
}

// Get implements Store. A missing key is ErrNotFound (fs.ErrNotExist).
func (d *DiskStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	p, err := d.path(ctx, key)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, "", err
	}
	ct, _ := os.ReadFile(p + ".ct")
	return data, string(ct), nil
}

// Delete implements Store.
func (d *DiskStore) Delete(ctx context.Context, key string) error {
	p, err := d.path(ctx, key)
	if err != nil {
		return err
	}
	_ = os.Remove(p + ".ct")
	if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
