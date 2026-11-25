// Package brand es el cliente de plataforma para la identidad de marca de la
// empresa (core-brand): cómo se llama, sus datos de contacto, su logo, sus
// colores y su tipografía. Lo usa todo core que arma algo que la empresa
// entrega —una boleta, un PDF, un correo— para no pedirle esos datos a cada
// empresa en un formulario propio.
//
// POR QUÉ VIVE EN EL KIT: igual que divisions, la marca es un contrato DE
// PLATAFORMA, transversal a todo core, no un contrato de dominio entre dos
// vecinos. Y sobre todo porque su postura de fallo es una decisión que no
// puede quedar a criterio de cada consumidor:
//
// LA MARCA ADORNA, NUNCA BLOQUEA. Es lo contrario de divisions (fail-closed):
// un documento sin logo es un documento feo; uno que no se puede armar porque
// core-brand está caído es un técnico parado frente a un cliente. Por eso:
//
//   - Get NO devuelve error por una caída. Si core-brand no contesta, devuelve
//     lo último que tuvo de ese tenant, sin importar la edad (stale-while-error);
//     si nunca tuvo nada, &Brand{Configured: false}. El único error es no tener
//     tenant en el contexto, que es un defecto de quien llama.
//   - Una empresa que no cargó nada no es un error: Configured = false, y el
//     consumidor usa SU valor por omisión.
//   - El Stub (sin dirección configurada) contesta Configured = false: fail-open.
//   - core-brand guarda lo que la marca ES; el consumidor decide si le sirve.
//     Cada Color trae la tinta que se lee encima y su contraste de WCAG para
//     que el documento decida con números.
//
// Reglas compartidas con divisions:
//
//   - El token del CALLER viaja siempre (auth.Outgoing); la identidad de este
//     core sólo cuando no hay nadie detrás y se configuró WithServiceTokens.
//     Consecuencia: la caché se clava por tenant.
//   - La unidad de caché es la MARCA ENTERA del tenant (una GetBrand), TTL de
//     un minuto. El byte de cada logo se cachea aparte por su sha256 y sólo se
//     vuelve a bajar cuando el sha cambia.
//   - Caché en memoria, nunca persistida: una lectura que escribe deja de ser
//     una lectura.
//
// El evento brand.identity.updated.v1 es opcional: quien quiera enterarse en
// el acto lo consume y llama a Invalidate; sin eso, la demora es el TTL.
package brand

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/status"

	"github.com/hs-javierviquez/strix-core-kit/auth"
	brandv1 "github.com/hs-javierviquez/strix-core-kit/gen/brand/v1"
	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// ErrNoTenant: no hay tenant en el contexto. Es el ÚNICO error de Get y Logo.
var ErrNoTenant = errors.New("brand: no hay tenant en el contexto")

// Roles de un color en la paleta.
const (
	RolePrimary   = "PRIMARY"
	RoleSecondary = "SECONDARY"
	RoleAccent    = "ACCENT"
)

// Variantes de logo.
const (
	LogoPrimary = "PRIMARY" // el de siempre, para fondo claro: el de un documento
	LogoOnDark  = "ON_DARK"
	LogoMono    = "MONO"
	LogoIcon    = "ICON"
)

// Brand es la marca de una empresa. Nunca es nil al salir de Get.
type Brand struct {
	// Configured es falso cuando la empresa no cargó nada —o cuando core-brand
	// no contestó y no había nada guardado—: todo lo demás viene vacío y el
	// consumidor usa su valor por omisión.
	Configured bool
	// Version sube en cada escritura de la marca.
	Version    int64
	Profile    Profile
	Colors     []Color
	Logos      []LogoRef
	Typography Typography
	UpdatedAt  time.Time
}

// Profile son los datos de la empresa. Lo vacío quiere decir «no lo dijo».
type Profile struct {
	// TradeName es cómo se presenta la empresa: lo que va en un encabezado.
	TradeName string
	// LegalName es la razón social, si es distinta.
	LegalName string
	// LegalIDType: "" | FISICA | JURIDICA | DIMEX | NITE | OTRO.
	LegalIDType string
	LegalID     string
	Tagline     string
	Phone       string
	Email       string
	Website     string
	Address     string
}

// Color es un color de la paleta con lo que core-brand deriva de él.
type Color struct {
	Role string
	// Hex es «#RRGGBB», en mayúscula.
	Hex string
	// OnColor es la tinta que mejor se lee encima («#FFFFFF» o «#1A1A1A»).
	OnColor string
	// Razones de contraste de WCAG (1 a 21) contra el blanco y contra la
	// tinta oscura. Con esto cada documento decide si el color le sirve.
	ContrastOnWhite float64
	ContrastOnDark  float64
}

// LogoRef describe un logo SIN su byte.
type LogoRef struct {
	Variant     string
	ContentType string // image/png | image/jpeg
	Width       int
	Height      int
	ByteSize    int
	// SHA256 cambia si y sólo si cambia el byte.
	SHA256    string
	UpdatedAt time.Time
}

// Logo es un logo con su byte, ya normalizado por core-brand (PNG de 8 bits
// sin entrelazar o JPEG RGB): cualquier librería de PDF lo dibuja.
type Logo struct {
	LogoRef
	Data []byte
}

// Typography son claves de la lista cerrada de core-brand; vacío = la de
// siempre de cada documento.
type Typography struct {
	HeadingFont string
	BodyFont    string
}

// Color devuelve el color de un rol.
func (b *Brand) Color(role string) (Color, bool) {
	if b == nil {
		return Color{}, false
	}
	for _, c := range b.Colors {
		if c.Role == role {
			return c, true
		}
	}
	return Color{}, false
}

// LogoRef devuelve la referencia del logo de una variante.
func (b *Brand) LogoRef(variant string) (LogoRef, bool) {
	if b == nil {
		return LogoRef{}, false
	}
	for _, l := range b.Logos {
		if l.Variant == variant {
			return l, true
		}
	}
	return LogoRef{}, false
}

// Reader es lo que un core consumidor necesita de la marca.
type Reader interface {
	// Get devuelve la marca del tenant del contexto. Nunca nil; el único
	// error posible es ErrNoTenant.
	Get(ctx context.Context) (*Brand, error)
	// Logo devuelve el logo de una variante, o nil (sin error) si la empresa
	// no lo subió o no se pudo conseguir. El único error es ErrNoTenant.
	Logo(ctx context.Context, variant string) (*Logo, error)
}

// Stub es el cliente sin backend: la marca nunca está configurada. Es
// fail-open a propósito (ver el comentario del paquete).
type Stub struct{}

// Get devuelve una marca sin configurar.
func (Stub) Get(ctx context.Context) (*Brand, error) {
	if tenantctx.Tenant(ctx) == "" {
		return nil, ErrNoTenant
	}
	return &Brand{}, nil
}

// Logo devuelve nil: sin backend no hay logo.
func (Stub) Logo(ctx context.Context, _ string) (*Logo, error) {
	if tenantctx.Tenant(ctx) == "" {
		return nil, ErrNoTenant
	}
	return nil, nil
}

// DefaultTTL es la vida de la marca en caché.
const DefaultTTL = time.Minute

// callTimeout acota cada llamada. Corto a propósito: quien espera es alguien
// que pidió un documento, y la respuesta ante la demora es seguir sin marca
// (o con la última conocida), no esperar. Sin WaitForReady.
const callTimeout = 2 * time.Second

// Audience es la audiencia que core-brand acepta en un token de servicio.
const Audience = "core-brand"

// GRPCClient es el cliente real contra core-brand.
type GRPCClient struct {
	conn    *grpc.ClientConn
	api     brandv1.BrandServiceClient
	ttl     time.Duration
	timeout time.Duration
	now     func() time.Time // seam de test
	log     *slog.Logger

	mu     sync.Mutex
	brands map[string]brandEntry // por tenant
	logos  map[logoKey]*Logo     // por (tenant, variante); el sha vive en el Logo

	// tokens acuña la identidad de ESTE core para las llamadas sin usuario
	// detrás; nil = solo reenviar el bearer.
	tokens *auth.ServiceTokens
}

type brandEntry struct {
	fetched time.Time
	brand   *Brand
}

type logoKey struct{ tenant, variant string }

// Option configura Dial.
type Option func(*GRPCClient)

// WithServiceTokens permite que las llamadas sin bearer de usuario —un
// consumidor de eventos, un job— identifiquen a este core con un token
// client_credentials del tenant en contexto. El bearer de una persona, cuando
// lo hay, sigue viajando tal cual: ver auth.Outgoing.
func WithServiceTokens(tokens *auth.ServiceTokens) Option {
	return func(c *GRPCClient) { c.tokens = tokens }
}

// WithTTL cambia la vida de la marca en caché.
func WithTTL(ttl time.Duration) Option {
	return func(c *GRPCClient) { c.ttl = ttl }
}

// WithLogger cambia a dónde van los avisos de fallback (por omisión,
// slog.Default()).
func WithLogger(l *slog.Logger) Option {
	return func(c *GRPCClient) { c.log = l }
}

// Dial conecta con core-brand (gRPC plaintext intra-cluster, mismo perfil de
// keepalive que el resto de clientes del estate). No contacta al servidor: un
// core-brand caído al arrancar no impide que el consumidor arranque.
//
// Con addr vacío NO usar Dial: usar Stub{}. Open hace esa elección.
func Dial(addr string, opts ...Option) (*GRPCClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("brand: dial %s: %w", addr, err)
	}
	c := &GRPCClient{
		conn:    conn,
		api:     brandv1.NewBrandServiceClient(conn),
		ttl:     DefaultTTL,
		timeout: callTimeout,
		now:     time.Now,
		brands:  make(map[string]brandEntry),
		logos:   make(map[logoKey]*Logo),
	}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Open es Dial con la elección del Stub adentro: con addr vacío devuelve
// Stub{} y un closer que no hace nada. Es lo que un core llama al arrancar.
func Open(addr string, opts ...Option) (Reader, func() error, error) {
	if strings.TrimSpace(addr) == "" {
		return Stub{}, func() error { return nil }, nil
	}
	c, err := Dial(addr, opts...)
	if err != nil {
		return nil, nil, err
	}
	return c, c.Close, nil
}

// Close libera la conexión.
func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

// Invalidate olvida la FRESCURA de la marca de un tenant: la próxima Get
// vuelve a preguntar. No borra lo guardado —sigue siendo el fallback si esa
// pregunta falla—, y los logos se revalidan solos por su sha.
func (c *GRPCClient) Invalidate(tenant string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.brands[tenant]; ok {
		e.fetched = time.Time{}
		c.brands[tenant] = e
	}
}

func (c *GRPCClient) logger() *slog.Logger {
	if c.log != nil {
		return c.log
	}
	return slog.Default()
}

// Get devuelve la marca del tenant del contexto: de caché si está fresca; si
// no, de core-brand; y si core-brand no contesta, la última conocida o una
// marca sin configurar. Ver el comentario del paquete.
func (c *GRPCClient) Get(ctx context.Context) (*Brand, error) {
	tenant := tenantctx.Tenant(ctx)
	if tenant == "" {
		return nil, ErrNoTenant
	}

	c.mu.Lock()
	entry, had := c.brands[tenant]
	c.mu.Unlock()
	if had && c.now().Sub(entry.fetched) < c.ttl {
		return entry.brand, nil
	}

	fresh, err := c.fetch(ctx)
	if err != nil {
		c.report(ctx, "get brand", tenant, had, err)
		if had {
			return entry.brand, nil
		}
		return &Brand{}, nil
	}

	c.mu.Lock()
	c.brands[tenant] = brandEntry{fetched: c.now(), brand: fresh}
	c.mu.Unlock()
	return fresh, nil
}

// Logo devuelve el logo de una variante. El byte se baja una vez por sha: si
// la marca dice que el sha no cambió, sale de memoria.
func (c *GRPCClient) Logo(ctx context.Context, variant string) (*Logo, error) {
	b, err := c.Get(ctx)
	if err != nil {
		return nil, err
	}
	tenant := tenantctx.Tenant(ctx)
	ref, ok := b.LogoRef(variant)
	if !ok {
		return nil, nil
	}
	key := logoKey{tenant, variant}

	c.mu.Lock()
	cached := c.logos[key]
	c.mu.Unlock()
	if cached != nil && cached.SHA256 == ref.SHA256 {
		return cached, nil
	}

	outCtx, err := auth.Outgoing(ctx, c.tokens, Audience)
	if err == nil {
		callCtx, cancel := context.WithTimeout(outCtx, c.timeout)
		defer cancel()
		body, gerr := c.api.GetLogo(callCtx, &brandv1.GetLogoRequest{Variant: variantProto(variant)})
		err = gerr
		if err == nil && len(body.GetData()) > 0 {
			logo := &Logo{LogoRef: ref, Data: body.GetData()}
			if ct := body.GetContentType(); ct != "" {
				logo.ContentType = ct
			}
			c.mu.Lock()
			c.logos[key] = logo
			c.mu.Unlock()
			return logo, nil
		}
	}
	if err != nil && status.Code(err) != codes.NotFound {
		c.report(ctx, "get logo", tenant, cached != nil, err)
	}
	// No se pudo bajar el nuevo: el anterior, si lo hay, sigue siendo mejor
	// que un documento sin logo. (NotFound sin anterior: lo quitaron entre la
	// marca cacheada y ahora.)
	if cached != nil && status.Code(err) != codes.NotFound {
		return cached, nil
	}
	return nil, nil
}

// report deja en el log por qué se cayó al fallback. Una negativa de
// autenticación o de permiso NO es una red lenta: es una configuración rota
// (típicamente, la audiencia de este core falta en CORE_BRAND_AUDIENCE) y va
// como error para que alguien la vea.
func (c *GRPCClient) report(ctx context.Context, op, tenant string, stale bool, err error) {
	fallback := "sin marca"
	if stale {
		fallback = "última conocida"
	}
	args := []any{"op", op, "tenant", tenant, "fallback", fallback, "error", err}
	switch status.Code(err) {
	case codes.Unauthenticated, codes.PermissionDenied:
		c.logger().ErrorContext(ctx, "brand: core-brand rechazó la llamada; revisar que la audiencia de este core esté en CORE_BRAND_AUDIENCE", args...)
	default:
		c.logger().WarnContext(ctx, "brand: core-brand no contestó", args...)
	}
}

func (c *GRPCClient) fetch(ctx context.Context) (*Brand, error) {
	outCtx, err := auth.Outgoing(ctx, c.tokens, Audience)
	if err != nil {
		return nil, err
	}
	callCtx, cancel := context.WithTimeout(outCtx, c.timeout)
	defer cancel()
	resp, err := c.api.GetBrand(callCtx, &brandv1.GetBrandRequest{})
	if err != nil {
		return nil, err
	}
	return fromProto(resp), nil
}

func fromProto(p *brandv1.Brand) *Brand {
	b := &Brand{
		Configured: p.GetConfigured(),
		Version:    p.GetVersion(),
		Profile: Profile{
			TradeName:   p.GetProfile().GetTradeName(),
			LegalName:   p.GetProfile().GetLegalName(),
			LegalIDType: enumName(p.GetProfile().GetLegalIdType().String(), "LEGAL_ID_TYPE_"),
			LegalID:     p.GetProfile().GetLegalId(),
			Tagline:     p.GetProfile().GetTagline(),
			Phone:       p.GetProfile().GetPhone(),
			Email:       p.GetProfile().GetEmail(),
			Website:     p.GetProfile().GetWebsite(),
			Address:     p.GetProfile().GetAddress(),
		},
		Typography: Typography{
			HeadingFont: p.GetTypography().GetHeadingFont(),
			BodyFont:    p.GetTypography().GetBodyFont(),
		},
	}
	if p.GetUpdatedAt() != nil {
		b.UpdatedAt = p.GetUpdatedAt().AsTime()
	}
	for _, c := range p.GetColors() {
		b.Colors = append(b.Colors, Color{
			Role: enumName(c.GetRole().String(), "COLOR_ROLE_"), Hex: c.GetHex(), OnColor: c.GetOnColor(),
			ContrastOnWhite: c.GetContrastOnWhite(), ContrastOnDark: c.GetContrastOnDark(),
		})
	}
	for _, l := range p.GetLogos() {
		ref := LogoRef{
			Variant: enumName(l.GetVariant().String(), "LOGO_VARIANT_"), ContentType: l.GetContentType(),
			Width: int(l.GetWidthPx()), Height: int(l.GetHeightPx()), ByteSize: int(l.GetByteSize()),
			SHA256: l.GetSha256(),
		}
		if l.GetUpdatedAt() != nil {
			ref.UpdatedAt = l.GetUpdatedAt().AsTime()
		}
		b.Logos = append(b.Logos, ref)
	}
	return b
}

// enumName quita el prefijo del enum; UNSPECIFIED (o un valor que este binario
// no conoce) sale vacío.
func enumName(s, prefix string) string {
	name, ok := strings.CutPrefix(s, prefix)
	if !ok || name == "UNSPECIFIED" {
		return ""
	}
	return name
}

func variantProto(v string) brandv1.LogoVariant {
	return brandv1.LogoVariant(brandv1.LogoVariant_value["LOGO_VARIANT_"+v])
}
