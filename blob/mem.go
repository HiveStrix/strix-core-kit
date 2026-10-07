package blob

import (
	"context"
	"fmt"
	"net/url"
	"sync"
	"time"
)

// MemStore is an in-memory Store and Presigner for tests. It enforces the
// same key rules as S3Store, so a test that passes against it does not
// pass by skipping the tenant check.
type MemStore struct {
	mu   sync.Mutex
	objs map[string]memObject
	now  func() time.Time
}

type memObject struct {
	data        []byte
	contentType string
}

// NewMemStore builds an empty MemStore.
func NewMemStore() *MemStore {
	return &MemStore{objs: map[string]memObject{}, now: time.Now}
}

// Put implements Store. The bytes are copied.
func (m *MemStore) Put(ctx context.Context, key, contentType string, data []byte) error {
	if err := CheckKey(ctx, key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObject{data: append([]byte(nil), data...), contentType: contentType}
	return nil
}

// Get implements Store.
func (m *MemStore) Get(ctx context.Context, key string) ([]byte, string, error) {
	if err := CheckKey(ctx, key); err != nil {
		return nil, "", err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	if !ok {
		return nil, "", fmt.Errorf("blob: %s: %w", key, ErrNotFound)
	}
	return append([]byte(nil), o.data...), o.contentType, nil
}

// Delete implements Store.
func (m *MemStore) Delete(ctx context.Context, key string) error {
	if err := CheckKey(ctx, key); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.objs, key)
	return nil
}

// PresignGet implements Presigner with a mem:// URL naming the key and expiry.
func (m *MemStore) PresignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	return m.presign(ctx, "GET", key, ttl)
}

// PresignPut implements Presigner.
func (m *MemStore) PresignPut(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	return m.presign(ctx, "PUT", key, ttl)
}

func (m *MemStore) presign(ctx context.Context, method, key string, ttl time.Duration) (string, error) {
	if err := CheckKey(ctx, key); err != nil {
		return "", err
	}
	if err := checkTTL(ttl); err != nil {
		return "", err
	}
	u := url.URL{Scheme: "mem", Path: "/" + key, RawQuery: url.Values{
		"method":  {method},
		"expires": {m.now().Add(ttl).UTC().Format(time.RFC3339)},
	}.Encode()}
	return u.String(), nil
}
