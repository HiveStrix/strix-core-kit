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

// fakeCatalogs sirve los dos catálogos por gRPC real, sobre el MISMO
// servidor que el árbol (así se prueba que un solo Dial alcanza los tres).
//
//	empresa(1)
//	├── operacion(2 → centroamerica)
//	│   ├── cocina(3)
//	│   └── obra(4, temporal 2026-09-01..2026-09-30)
//	└── cerrado(5, inactivo)
//	    └── bajo-cerrado(6)
type fakeCatalogs struct {
	divisionsv1.UnimplementedCostCentersServiceServer
	divisionsv1.UnimplementedAssetTypesServiceServer
	ccCalls  atomic.Int64
	atCalls  atomic.Int64
	lastAuth atomic.Value // string
}

func (f *fakeCatalogs) GetCostCenterTree(ctx context.Context, _ *divisionsv1.GetCostCenterTreeRequest) (*divisionsv1.CostCenterTree, error) {
	f.ccCalls.Add(1)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get("authorization"); len(a) > 0 {
			f.lastAuth.Store(a[0])
		}
	}
	return &divisionsv1.CostCenterTree{CostCenters: []*divisionsv1.CostCenter{
		{Id: 1, ParentId: 0, DivisionId: 1, Code: "empresa", Active: true},
		{Id: 2, ParentId: 1, DivisionId: 2, Code: "operacion", Active: true},
		{Id: 3, ParentId: 2, DivisionId: 2, Code: "cocina", Active: true},
		{Id: 4, ParentId: 2, DivisionId: 2, Code: "obra", Temporary: true, StartsOn: "2026-09-01", EndsOn: "2026-09-30", Active: true},
		{Id: 5, ParentId: 1, DivisionId: 1, Code: "cerrado", Active: false},
		{Id: 6, ParentId: 5, DivisionId: 1, Code: "bajo-cerrado", Active: true},
	}}, nil
}

func (f *fakeCatalogs) ListAssetTypes(_ context.Context, req *divisionsv1.ListAssetTypesRequest) (*divisionsv1.ListAssetTypesResponse, error) {
	f.atCalls.Add(1)
	if !req.GetIncludeInactive() {
		return nil, errors.New("el kit debe pedir el catálogo completo")
	}
	return &divisionsv1.ListAssetTypesResponse{AssetTypes: []*divisionsv1.AssetType{
		{Id: 1, Code: "product", Name: "Producto", Recognition: "inventoried", Outflow: "sale", StockMode: "quantity", Active: true},
		{Id: 5, Code: "machinery", Name: "Maquinaria", Recognition: "capitalizable", Outflow: "usage", StockMode: "unit", Active: false},
	}}, nil
}

func serveCatalogs(t *testing.T) (*fakeCatalogs, *GRPCClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeCatalogs{}
	divisionsv1.RegisterDivisionsServiceServer(srv, &fakeDivisions{})
	divisionsv1.RegisterCostCentersServiceServer(srv, fake)
	divisionsv1.RegisterAssetTypesServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return fake, c
}

func at(day string) time.Time {
	t, err := time.Parse("2006-01-02", day)
	if err != nil {
		panic(err)
	}
	return t
}

// La validez de un centro replica la del core: estado, cadena de ancestros
// y ventana temporal — y depende del día, que es el reloj del cliente.
func TestCostCentersValidateRefsChecksStateChainAndWindow(t *testing.T) {
	_, c := serveCatalogs(t)
	ctx := tenantctx.WithTenant(context.Background(), "prueba")

	c.now = func() time.Time { return at("2026-09-15") }
	got, err := c.CostCenters().ValidateRefs(ctx, []int64{3, 4, 5, 6, 99})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	cases := map[int64]RefStatus{
		3:  {Valid: true},
		4:  {Valid: true},
		5:  {Reason: "centro de costo inactivo"},
		6:  {Reason: "ancestro inactivo: cerrado"},
		99: {Reason: "no existe"},
	}
	for id, want := range cases {
		if got[id] != want {
			t.Fatalf("%d = %+v, esperaba %+v", id, got[id], want)
		}
	}

	// Un mes después la obra está cerrada, con la MISMA foto en caché.
	c.now = func() time.Time { return at("2026-10-15") }
	got, _ = c.CostCenters().ValidateRefs(ctx, []int64{4})
	if got[4].Valid || got[4].Reason != "cerrado desde el 2026-09-30" {
		t.Fatalf("obra en octubre = %+v", got[4])
	}
}

func TestCostCentersSubtreeAndPath(t *testing.T) {
	_, c := serveCatalogs(t)
	ctx := tenantctx.WithTenant(context.Background(), "prueba")

	ids, err := c.CostCenters().Subtree(ctx, 2)
	if err != nil || len(ids) != 3 || ids[0] != 2 {
		t.Fatalf("subtree = %v, err = %v", ids, err)
	}
	p, err := c.CostCenters().Path(ctx, 3)
	if err != nil || p != "empresa/operacion/cocina" {
		t.Fatalf("path = %q, err = %v", p, err)
	}
	if _, err := c.CostCenters().Path(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, esperaba ErrNotFound", err)
	}
}

// Los tipos de activo se piden COMPLETOS (con inactivos): un artículo viejo
// puede referenciar un tipo desactivado y su comportamiento no cambia, así
// que Get lo devuelve; ValidateRefs es lo que lo rechaza para altas nuevas.
func TestAssetTypesGetListAndValidate(t *testing.T) {
	_, c := serveCatalogs(t)
	ctx := tenantctx.WithTenant(context.Background(), "prueba")

	a, err := c.AssetTypes().Get(ctx, 5)
	if err != nil || a.Active || a.Recognition != "capitalizable" || a.StockMode != "unit" {
		t.Fatalf("get(5) = %+v, err = %v", a, err)
	}
	if _, err := c.AssetTypes().Get(ctx, 99); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, esperaba ErrNotFound", err)
	}
	list, err := c.AssetTypes().List(ctx)
	if err != nil || len(list) != 2 || list[0].ID != 1 {
		t.Fatalf("list = %+v, err = %v", list, err)
	}
	got, err := c.AssetTypes().ValidateRefs(ctx, []int64{1, 5, 99})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !got[1].Valid || got[5].Valid || got[5].Reason != "tipo de activo inactivo" || got[99].Reason != "no existe" {
		t.Fatalf("got = %+v", got)
	}
}

// Cada catálogo tiene su propia caché por tenant con el mismo TTL, y el
// token del caller viaja en cada carga.
func TestCatalogCachesArePerTenantAndForwardTheToken(t *testing.T) {
	fake, c := serveCatalogs(t)
	now := at("2026-09-15")
	c.now = func() time.Time { return now }
	ctxA := metadata.NewIncomingContext(tenantctx.WithTenant(context.Background(), "a"), metadata.Pairs("authorization", "Bearer token-a"))
	ctxB := tenantctx.WithTenant(context.Background(), "b")

	for i := 0; i < 3; i++ {
		if _, err := c.CostCenters().Subtree(ctxA, 1); err != nil {
			t.Fatalf("err = %v", err)
		}
		if _, err := c.AssetTypes().List(ctxA); err != nil {
			t.Fatalf("err = %v", err)
		}
	}
	if fake.ccCalls.Load() != 1 || fake.atCalls.Load() != 1 {
		t.Fatalf("calls = cc %d / at %d, esperaba 1 / 1", fake.ccCalls.Load(), fake.atCalls.Load())
	}
	if got := fake.lastAuth.Load(); got != "Bearer token-a" {
		t.Fatalf("authorization reenviado = %v", got)
	}

	if _, err := c.CostCenters().Subtree(ctxB, 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	if fake.ccCalls.Load() != 2 {
		t.Fatalf("otro tenant debe cargar su propia foto: calls = %d", fake.ccCalls.Load())
	}

	now = now.Add(2 * DefaultTTL)
	if _, err := c.CostCenters().Subtree(ctxA, 1); err != nil {
		t.Fatalf("err = %v", err)
	}
	if fake.ccCalls.Load() != 3 {
		t.Fatalf("tras el TTL debe recargar: calls = %d", fake.ccCalls.Load())
	}
}

func TestCatalogsFailClosedWithoutTenantAndOnTheStub(t *testing.T) {
	_, c := serveCatalogs(t)
	if _, err := c.CostCenters().ValidateRefs(context.Background(), []int64{1}); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("err = %v, esperaba ErrNoTenant", err)
	}
	if _, err := c.AssetTypes().List(context.Background()); !errors.Is(err, ErrNoTenant) {
		t.Fatalf("err = %v, esperaba ErrNoTenant", err)
	}

	var cats Catalogs = Stub{}
	if _, err := cats.CostCenters().Path(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, esperaba ErrNotConfigured", err)
	}
	if _, err := cats.AssetTypes().Get(context.Background(), 1); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, esperaba ErrNotConfigured", err)
	}
	// *GRPCClient también satisface Catalogs: un consumidor declara UNA
	// variable y la config decide cuál de las dos corre.
	cats = c
	if _, err := cats.CostCenters().Subtree(tenantctx.WithTenant(context.Background(), "x"), 1); err != nil {
		t.Fatalf("err = %v", err)
	}
}
