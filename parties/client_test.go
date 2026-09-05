package parties

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	clientsv1 "github.com/hs-javierviquez/strix-core-kit/gen/clients/v1"
)

// fakeClients sirve LookupSuppliers por gRPC real (socket de loopback) con
// dos proveedores fijos: uno activo que emite recibo y otro bloqueado que no.
type fakeClients struct {
	clientsv1.UnimplementedClientsServiceServer
	calls    atomic.Int64
	lastAuth atomic.Value // string
	lastIDs  atomic.Value // []uint32
}

func (f *fakeClients) LookupSuppliers(ctx context.Context, req *clientsv1.LookupSuppliersRequest) (*clientsv1.LookupSuppliersResponse, error) {
	f.calls.Add(1)
	f.lastIDs.Store(req.GetIds())
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get("authorization"); len(a) > 0 {
			f.lastAuth.Store(a[0])
		}
	}
	all := map[uint32]*clientsv1.SupplierRef{
		7: {Id: 7, Code: "PRV0001", Name: "Distribuidora X", Active: true, IssuesReceipt: true, CreditDays: 30},
		9: {Id: 9, Code: "PRV0002", Name: "Don Chico", Active: false, IssuesReceipt: false},
	}
	resp := &clientsv1.LookupSuppliersResponse{}
	for _, id := range req.GetIds() {
		if s, ok := all[id]; ok {
			resp.Suppliers = append(resp.Suppliers, s)
		}
	}
	return resp, nil
}

func serveFake(t *testing.T) (*fakeClients, *GRPCClient) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeClients{}
	clientsv1.RegisterClientsServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	c, err := Dial(lis.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return fake, c
}

// El lote vuelve como mapa: los que existen traen su proyección, los que no
// simplemente no están — y el consumidor decide qué hacer con eso.
func TestLookupSuppliersReturnsOnlyKnownIDs(t *testing.T) {
	_, c := serveFake(t)
	got, err := c.LookupSuppliers(context.Background(), []int64{7, 9, 99})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, esperaba 2: %+v", len(got), got)
	}
	if s := got[7]; !s.Active || !s.IssuesReceipt || s.CreditDays != 30 || s.Code != "PRV0001" {
		t.Fatalf("7 = %+v", s)
	}
	if s := got[9]; s.Active || s.IssuesReceipt {
		t.Fatalf("9 = %+v", s)
	}
	if _, ok := got[99]; ok {
		t.Fatal("99 no debía venir")
	}
}

// Sin caché: cada escritura pregunta. Un proveedor bloqueado hace un minuto
// tiene que rebotar ahora, no cuando venza una foto.
func TestEveryLookupHitsTheServer(t *testing.T) {
	fake, c := serveFake(t)
	for i := 0; i < 3; i++ {
		if _, err := c.LookupSuppliers(context.Background(), []int64{7}); err != nil {
			t.Fatalf("err = %v", err)
		}
	}
	if n := fake.calls.Load(); n != 3 {
		t.Fatalf("calls = %d, esperaba 3", n)
	}
}

// Un lote vacío no viaja; ids imposibles para el contrato (uint32) se
// descartan antes de preguntar.
func TestEmptyAndImpossibleIDsDoNotTravel(t *testing.T) {
	fake, c := serveFake(t)
	got, err := c.LookupSuppliers(context.Background(), nil)
	if err != nil || len(got) != 0 || fake.calls.Load() != 0 {
		t.Fatalf("got = %v, err = %v, calls = %d", got, err, fake.calls.Load())
	}
	if _, err := c.LookupSuppliers(context.Background(), []int64{-1, 7, 1 << 40}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if ids := fake.lastIDs.Load().([]uint32); len(ids) != 1 || ids[0] != 7 {
		t.Fatalf("ids enviados = %v, esperaba [7]", ids)
	}
}

func TestCallerTokenIsForwarded(t *testing.T) {
	fake, c := serveFake(t)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs("authorization", "Bearer caller-token"))
	if _, err := c.LookupSuppliers(ctx, []int64{7}); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := fake.lastAuth.Load(); got != "Bearer caller-token" {
		t.Fatalf("authorization reenviado = %v", got)
	}
}

func TestStubFailsClosed(t *testing.T) {
	if _, err := (Stub{}).LookupSuppliers(context.Background(), []int64{1}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, esperaba ErrNotConfigured", err)
	}
}
