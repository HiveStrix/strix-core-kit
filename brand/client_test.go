package brand

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/genproto/googleapis/api/httpbody"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	brandv1 "github.com/hs-javierviquez/strix-core-kit/gen/brand/v1"
	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// fakeBrand sirve una marca por gRPC real (socket de loopback): los tests
// ejercitan la misma ruta de red que producción, incluida la metadata.
type fakeBrand struct {
	brandv1.UnimplementedBrandServiceServer
	mu        sync.Mutex
	fail      error  // si no es nil, toda RPC falla con esto
	name      string // trade_name
	sha       string // sha del logo PRIMARY; vacío = sin logo
	logoData  []byte
	brandHits atomic.Int64
	logoHits  atomic.Int64
	lastAuth  atomic.Value // string
}

func (f *fakeBrand) set(fn func(*fakeBrand)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeBrand) GetBrand(ctx context.Context, _ *brandv1.GetBrandRequest) (*brandv1.Brand, error) {
	f.brandHits.Add(1)
	if md, ok := metadata.FromIncomingContext(ctx); ok {
		if a := md.Get("authorization"); len(a) > 0 {
			f.lastAuth.Store(a[0])
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	b := &brandv1.Brand{
		Configured: f.name != "",
		Version:    7,
		Profile: &brandv1.CompanyProfile{
			TradeName: f.name, LegalIdType: brandv1.LegalIdType_LEGAL_ID_TYPE_JURIDICA, LegalId: "3-101-123456",
		},
		Colors: []*brandv1.BrandColor{{
			Role: brandv1.ColorRole_COLOR_ROLE_PRIMARY, Hex: "#0B5FFF", OnColor: "#FFFFFF",
			ContrastOnWhite: 5.1, ContrastOnDark: 3.4,
		}},
	}
	if f.sha != "" {
		b.Logos = []*brandv1.LogoRef{{
			Variant: brandv1.LogoVariant_LOGO_VARIANT_PRIMARY, ContentType: "image/png",
			WidthPx: 320, HeightPx: 120, ByteSize: int32(len(f.logoData)), Sha256: f.sha,
		}}
	}
	return b, nil
}

func (f *fakeBrand) GetLogo(_ context.Context, req *brandv1.GetLogoRequest) (*httpbody.HttpBody, error) {
	f.logoHits.Add(1)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return nil, f.fail
	}
	if f.sha == "" || req.GetVariant() != brandv1.LogoVariant_LOGO_VARIANT_PRIMARY {
		return nil, status.Error(codes.NotFound, "no existe")
	}
	return &httpbody.HttpBody{ContentType: "image/png", Data: f.logoData}, nil
}

// clock es un reloj que el test adelanta.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// logs junta lo que el cliente escribe, para mirar el nivel.
type logs struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logs) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}
func (l *logs) String() string { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func serveFake(t *testing.T) (*fakeBrand, *GRPCClient, *clock, *logs) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	fake := &fakeBrand{name: "Alquileres Opra", sha: "aaa", logoData: []byte("png-uno")}
	brandv1.RegisterBrandServiceServer(srv, fake)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)

	out := &logs{}
	client, err := Dial(lis.Addr().String(), WithLogger(slog.New(slog.NewTextHandler(out, nil))))
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ck := &clock{t: time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)}
	client.now = ck.now
	return fake, client, ck, out
}

// ctxFor arma el contexto como lo deja el PEP de un core consumidor: tenant
// sembrado y el bearer del caller en la metadata ENTRANTE.
func ctxFor(tenant string) context.Context {
	ctx := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs("authorization", "Bearer token-del-caller"))
	return tenantctx.WithTenant(ctx, tenant)
}

func TestGetTraeLaMarcaYReenviaElTokenDelCaller(t *testing.T) {
	fake, c, _, _ := serveFake(t)
	b, err := c.Get(ctxFor("acme"))
	if err != nil {
		t.Fatal(err)
	}
	if !b.Configured || b.Version != 7 || b.Profile.TradeName != "Alquileres Opra" || b.Profile.LegalIDType != "JURIDICA" {
		t.Errorf("marca: %+v", b)
	}
	col, ok := b.Color(RolePrimary)
	if !ok || col.Hex != "#0B5FFF" || col.OnColor != "#FFFFFF" || col.ContrastOnWhite != 5.1 {
		t.Errorf("color: %+v %v", col, ok)
	}
	if _, ok := b.Color(RoleAccent); ok {
		t.Error("no había color de acento")
	}
	if ref, ok := b.LogoRef(LogoPrimary); !ok || ref.SHA256 != "aaa" || ref.Width != 320 {
		t.Errorf("logo ref: %+v %v", ref, ok)
	}
	if got, _ := fake.lastAuth.Load().(string); got != "Bearer token-del-caller" {
		t.Errorf("authorization = %q", got)
	}
}

func TestGetCacheaPorTenantHastaElTTL(t *testing.T) {
	fake, c, ck, _ := serveFake(t)
	for i := 0; i < 3; i++ {
		if _, err := c.Get(ctxFor("acme")); err != nil {
			t.Fatal(err)
		}
	}
	if n := fake.brandHits.Load(); n != 1 {
		t.Fatalf("tres Get frescos = %d llamadas, quería 1", n)
	}
	// Otro tenant no comparte la caché.
	if _, err := c.Get(ctxFor("otra")); err != nil {
		t.Fatal(err)
	}
	if n := fake.brandHits.Load(); n != 2 {
		t.Fatalf("otro tenant: %d llamadas, quería 2", n)
	}
	ck.add(DefaultTTL + time.Second)
	fake.set(func(f *fakeBrand) { f.name = "Opra S.A." })
	b, _ := c.Get(ctxFor("acme"))
	if b.Profile.TradeName != "Opra S.A." || fake.brandHits.Load() != 3 {
		t.Errorf("vencido el TTL tenía que releer: %q, %d llamadas", b.Profile.TradeName, fake.brandHits.Load())
	}
}

func TestInvalidateFuerzaLaRelectura(t *testing.T) {
	fake, c, _, _ := serveFake(t)
	_, _ = c.Get(ctxFor("acme"))
	fake.set(func(f *fakeBrand) { f.name = "Nombre Nuevo" })
	c.Invalidate("acme")
	c.Invalidate("nadie") // un tenant que no está no rompe nada
	if b, _ := c.Get(ctxFor("acme")); b.Profile.TradeName != "Nombre Nuevo" {
		t.Errorf("después de Invalidate: %q", b.Profile.TradeName)
	}
}

// La marca adorna, nunca bloquea: con core-brand caído sale la última conocida,
// sin importar la edad, y sin error.
func TestGetConElCoreCaidoDevuelveLaUltimaConocida(t *testing.T) {
	fake, c, ck, out := serveFake(t)
	if _, err := c.Get(ctxFor("acme")); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeBrand) { f.fail = status.Error(codes.Unavailable, "caído") })
	ck.add(48 * time.Hour)
	b, err := c.Get(ctxFor("acme"))
	if err != nil {
		t.Fatalf("una caída no puede ser un error: %v", err)
	}
	if !b.Configured || b.Profile.TradeName != "Alquileres Opra" {
		t.Errorf("quería la última conocida, obtuve %+v", b)
	}
	if !strings.Contains(out.String(), "level=WARN") || strings.Contains(out.String(), "level=ERROR") {
		t.Errorf("una caída se avisa como WARN:\n%s", out.String())
	}
	// Y cuando vuelve, se actualiza.
	fake.set(func(f *fakeBrand) { f.fail = nil; f.name = "De Vuelta" })
	if b, _ := c.Get(ctxFor("acme")); b.Profile.TradeName != "De Vuelta" {
		t.Errorf("al volver el core: %q", b.Profile.TradeName)
	}
}

func TestGetSinNadaGuardadoYCoreCaidoDevuelveSinConfigurar(t *testing.T) {
	fake, c, _, _ := serveFake(t)
	fake.set(func(f *fakeBrand) { f.fail = status.Error(codes.Unavailable, "caído") })
	b, err := c.Get(ctxFor("acme"))
	if err != nil || b == nil || b.Configured {
		t.Fatalf("quería &Brand{Configured:false}, nil; obtuve %+v, %v", b, err)
	}
	if l, err := c.Logo(ctxFor("acme"), LogoPrimary); l != nil || err != nil {
		t.Errorf("sin marca no hay logo: %v, %v", l, err)
	}
}

// Una negativa de autenticación (típicamente «audience mismatch») tampoco se
// propaga, pero es una configuración rota: va como ERROR.
func TestErrorDeAuthNoSePropagaYSeRegistraComoError(t *testing.T) {
	for _, code := range []codes.Code{codes.Unauthenticated, codes.PermissionDenied} {
		fake, c, _, out := serveFake(t)
		fake.set(func(f *fakeBrand) { f.fail = status.Error(code, "audience mismatch") })
		b, err := c.Get(ctxFor("acme"))
		if err != nil || b.Configured {
			t.Fatalf("%v: obtuve %+v, %v", code, b, err)
		}
		if !strings.Contains(out.String(), "level=ERROR") || !strings.Contains(out.String(), "CORE_BRAND_AUDIENCE") {
			t.Errorf("%v: quería un ERROR que nombre la audiencia:\n%s", code, out.String())
		}
	}
}

func TestGetSinTenantEsElUnicoError(t *testing.T) {
	_, c, _, _ := serveFake(t)
	if _, err := c.Get(context.Background()); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Get sin tenant: %v", err)
	}
	if _, err := c.Logo(context.Background(), LogoPrimary); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Logo sin tenant: %v", err)
	}
}

func TestLogoSeBajaUnaVezPorSha(t *testing.T) {
	fake, c, ck, _ := serveFake(t)
	for i := 0; i < 3; i++ {
		l, err := c.Logo(ctxFor("acme"), LogoPrimary)
		if err != nil || l == nil || string(l.Data) != "png-uno" || l.ContentType != "image/png" || l.Width != 320 {
			t.Fatalf("logo: %+v, %v", l, err)
		}
	}
	if n := fake.logoHits.Load(); n != 1 {
		t.Fatalf("tres Logo = %d bajadas, quería 1", n)
	}
	// La marca se relee (venció el TTL) pero el sha es el mismo: no se baja.
	ck.add(DefaultTTL + time.Second)
	if _, err := c.Logo(ctxFor("acme"), LogoPrimary); err != nil || fake.logoHits.Load() != 1 {
		t.Fatalf("mismo sha no se vuelve a bajar: %d bajadas", fake.logoHits.Load())
	}
	// Cambia el sha: se baja el nuevo.
	fake.set(func(f *fakeBrand) { f.sha = "bbb"; f.logoData = []byte("png-dos") })
	ck.add(DefaultTTL + time.Second)
	l, _ := c.Logo(ctxFor("acme"), LogoPrimary)
	if l == nil || string(l.Data) != "png-dos" || l.SHA256 != "bbb" || fake.logoHits.Load() != 2 {
		t.Fatalf("sha nuevo: %+v, %d bajadas", l, fake.logoHits.Load())
	}
	// Una variante que no se subió: nil, sin error y sin llamada.
	if l, err := c.Logo(ctxFor("acme"), LogoIcon); l != nil || err != nil || fake.logoHits.Load() != 2 {
		t.Errorf("variante ausente: %v, %v", l, err)
	}
}

func TestLogoConElCoreCaidoDevuelveElAnterior(t *testing.T) {
	fake, c, ck, _ := serveFake(t)
	if l, _ := c.Logo(ctxFor("acme"), LogoPrimary); l == nil {
		t.Fatal("sin logo inicial")
	}
	// La marca alcanza a releerse con un sha nuevo, pero el byte no baja.
	fake.set(func(f *fakeBrand) { f.sha = "ccc" })
	ck.add(DefaultTTL + time.Second)
	if _, err := c.Get(ctxFor("acme")); err != nil {
		t.Fatal(err)
	}
	fake.set(func(f *fakeBrand) { f.fail = status.Error(codes.Unavailable, "caído") })
	l, err := c.Logo(ctxFor("acme"), LogoPrimary)
	if err != nil || l == nil || string(l.Data) != "png-uno" {
		t.Errorf("quería el logo anterior, obtuve %+v, %v", l, err)
	}
}

func TestLogoQuitado(t *testing.T) {
	fake, c, ck, _ := serveFake(t)
	_, _ = c.Logo(ctxFor("acme"), LogoPrimary)
	fake.set(func(f *fakeBrand) { f.sha = "" })
	ck.add(DefaultTTL + time.Second)
	if l, err := c.Logo(ctxFor("acme"), LogoPrimary); l != nil || err != nil {
		t.Errorf("un logo que la empresa quitó no se sigue dibujando: %v, %v", l, err)
	}
}

func TestStubEsFailOpen(t *testing.T) {
	var r Reader = Stub{}
	b, err := r.Get(ctxFor("acme"))
	if err != nil || b == nil || b.Configured {
		t.Errorf("Stub.Get: %+v, %v", b, err)
	}
	if l, err := r.Logo(ctxFor("acme"), LogoPrimary); l != nil || err != nil {
		t.Errorf("Stub.Logo: %v, %v", l, err)
	}
	if _, err := r.Get(context.Background()); !errors.Is(err, ErrNoTenant) {
		t.Errorf("Stub sin tenant: %v", err)
	}
}

func TestOpenSinDireccionEsElStub(t *testing.T) {
	r, closer, err := Open("  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(Stub); !ok {
		t.Errorf("Open(\"\") = %T, quería Stub", r)
	}
	if err := closer(); err != nil {
		t.Error(err)
	}
	r, closer, err = Open("127.0.0.1:1", WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithTTL(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = closer() }()
	if _, ok := r.(*GRPCClient); !ok {
		t.Errorf("Open(addr) = %T", r)
	}
	// Nadie escucha ahí: igual contesta, sin configurar y sin error.
	if b, err := r.Get(ctxFor("acme")); err != nil || b.Configured {
		t.Errorf("sin servidor: %+v, %v", b, err)
	}
}

func TestHelpersSobreNil(t *testing.T) {
	var b *Brand
	if _, ok := b.Color(RolePrimary); ok {
		t.Error("Color sobre nil")
	}
	if _, ok := b.LogoRef(LogoPrimary); ok {
		t.Error("LogoRef sobre nil")
	}
}
