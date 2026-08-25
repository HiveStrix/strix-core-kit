package divisions

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	divisionsv1 "github.com/hs-javierviquez/strix-core-kit/gen/divisions/v1"
	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// fakeDivisions sirve un árbol fijo por gRPC real (socket de loopback), no
// una función sustituida: los tests ejercitan la misma ruta de red que
// producción, incluida la metadata.
//
//	general(1)
//	├── centroamerica(2)
//	│   ├── cr(4)
//	│   └── gt(5, inactiva)
//	│       └── suc-gt(6)
//	└── europa(3)
type fakeDivisions struct {
	divisionsv1.UnimplementedDivisionsServiceServer
	calls    atomic.Int64
	lastAuth atomic.Value // string
}

func (f *fakeDivisions) GetTree(ctx context.Context, _ *divisionsv1.GetTreeRequest) (*divisionsv1.Tree, error) {
	f.calls.Add(1)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get("authorization"); len(a) > 0 {
			f.lastAuth.Store(a[0])
		}
	}
	return &divisionsv1.Tree{
		Divisions: []*divisionsv1.Division{
			{Id: 1, ParentId: 0, Code: "general", Active: true},
			{Id: 2, ParentId: 1, Code: "centroamerica", Active: true},
			{Id: 3, ParentId: 1, Code: "europa", Active: true},
			{Id: 4, ParentId: 2, Code: "cr", Active: true},
			{Id: 5, ParentId: 2, Code: "gt", Active: false},
			{Id: 6, ParentId: 5, Code: "suc-gt", Active: true},
		},
	}, nil
}

func serveFake(t *testing.T) (*fakeDivisions, *GRPCClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeDivisions{}
	divisionsv1.RegisterDivisionsServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	client, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return fake, client
}

// ctxFor arma el contexto como lo deja el PEP de un core consumidor: tenant
// sembrado y el bearer del caller en la metadata ENTRANTE.
func ctxFor(tenant string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer token-del-caller"))
	return tenantctx.WithTenant(ctx, tenant)
}

// La semántica de validez replica la del core: existe, activa, y toda la
// cadena de ancestros activa. Este test es el contrato de esa réplica.
func TestValidateRefsChecksExistenceStateAndAncestorChain(t *testing.T) {
	_, c := serveFake(t)
	got, err := c.ValidateRefs(ctxFor("t1"), []int64{4, 5, 6, 99})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got[4].Valid {
		t.Fatalf("4 debía ser válida: %+v", got[4])
	}
	if got[5].Valid || got[5].Reason != "división inactiva" {
		t.Fatalf("5: %+v", got[5])
	}
	if got[6].Valid || got[6].Reason != "ancestro inactivo: gt" {
		t.Fatalf("6: %+v", got[6])
	}
	if got[99].Valid || got[99].Reason != "no existe" {
		t.Fatalf("99: %+v", got[99])
	}
}

func TestSubtreeIncludesSelfAndDescendants(t *testing.T) {
	_, c := serveFake(t)
	got, err := c.Subtree(ctxFor("t1"), 2)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := map[int64]bool{2: true, 4: true, 5: true, 6: true}
	if len(got) != len(want) {
		t.Fatalf("subtree(2) = %v", got)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("subtree(2) incluye %d", id)
		}
	}
	if _, err := c.Subtree(ctxFor("t1"), 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, esperaba ErrNotFound", err)
	}
}

func TestPathJoinsCodesFromRoot(t *testing.T) {
	_, c := serveFake(t)
	got, err := c.Path(ctxFor("t1"), 6)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if got != "general/centroamerica/gt/suc-gt" {
		t.Fatalf("path = %q", got)
	}
}

// La unidad de caché es el árbol POR TENANT: dos operaciones del mismo tenant
// dentro del TTL comparten una sola GetTree; otro tenant trae la suya, porque
// el token del caller viaja y la foto no puede cruzarse entre tenants.
func TestCacheIsPerTenantWithinTTL(t *testing.T) {
	fake, c := serveFake(t)
	if _, err := c.ValidateRefs(ctxFor("t1"), []int64{4}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if _, err := c.Subtree(ctxFor("t1"), 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := fake.calls.Load(); n != 1 {
		t.Fatalf("GetTree se llamó %d veces para t1, esperaba 1", n)
	}
	if _, err := c.Path(ctxFor("t2"), 4); err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("GetTree se llamó %d veces tras t2, esperaba 2", n)
	}
}

// Vencido el TTL, la foto se refresca — el peor caso diseñado es operar un
// minuto con un árbol viejo, nunca para siempre.
func TestCacheExpiresAfterTTL(t *testing.T) {
	fake, c := serveFake(t)
	base := time.Now()
	c.now = func() time.Time { return base }
	if _, err := c.Subtree(ctxFor("t1"), 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	c.now = func() time.Time { return base.Add(DefaultTTL + time.Second) }
	if _, err := c.Subtree(ctxFor("t1"), 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	if n := fake.calls.Load(); n != 2 {
		t.Fatalf("GetTree se llamó %d veces, esperaba 2 (TTL vencido)", n)
	}
}

// El bearer del CALLER viaja hasta core-divisions — nunca una identidad de
// servicio del core intermediario.
func TestCallerTokenIsForwarded(t *testing.T) {
	fake, c := serveFake(t)
	if _, err := c.Subtree(ctxFor("t1"), 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got, _ := fake.lastAuth.Load().(string); got != "Bearer token-del-caller" {
		t.Fatalf("authorization reenviado = %q", got)
	}
}

// Sin tenant en el contexto no hay a quién cachear ni a quién preguntar:
// error de cableado, no de usuario.
func TestNoTenantInContextFailsClosed(t *testing.T) {
	_, c := serveFake(t)
	if _, err := c.Subtree(context.Background(), 1); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("err = %v, esperaba ErrNoTenant", err)
	}
}

// El Stub es fail-closed: sin backend configurado, NINGUNA operación finge
// que la referencia existe.
func TestStubFailsClosedOnEveryMethod(t *testing.T) {
	s := Stub{}
	if _, err := s.ValidateRefs(context.Background(), []int64{1}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ValidateRefs: %v", err)
	}
	if _, err := s.Subtree(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Subtree: %v", err)
	}
	if _, err := s.Path(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("Path: %v", err)
	}
}
