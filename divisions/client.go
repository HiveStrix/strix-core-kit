// Package divisions es el cliente de plataforma para referencias
// organizacionales: validar que un division_id existe y acepta registros,
// resolver el subárbol de un nodo para filtrar, y obtener el path que los
// hechos transaccionales guardan como foto histórica.
//
// POR QUÉ VIVE EN EL KIT y no en cada consumidor (la convención vigente para
// clientes core→core es la contraria — ver strix-costing/internal/expenses):
// divisiones es un contrato DE PLATAFORMA, transversal a todo core, como
// authorization.v1 — no un contrato de dominio entre dos vecinos. Todo core
// que clasifique información contra el árbol necesita exactamente estas tres
// operaciones con exactamente esta semántica; N copias divergirían igual que
// divergieron las copias del proto (gitops#38). El kit no acumula clientes de
// dominio; acumula capacidades de plataforma, y esta lo es.
//
// Reglas de diseño (blueprint de divisions, 2026-08-24):
//
//   - El token del CALLER viaja siempre; nunca una identidad de servicio.
//     core-divisions debe saber quién pregunta — confiar en la palabra del
//     core intermediario es la confusión que la audiencia por core previene.
//     Consecuencia: la caché se clava por tenant.
//   - La unidad de caché es el ÁRBOL COMPLETO del tenant (una GetTree):
//     Validate/Subtree/Path se computan localmente sobre esa foto. Un árbol
//     son decenas o cientos de nodos; el TTL corto (60s) hace que el peor
//     caso sea operar un minuto con un árbol viejo — aceptable para master
//     data organizacional. La invalidación inmediata por eventos
//     (divisions.*.v1) se añade cuando algún consumidor la necesite.
//   - stdlib puro: cero dependencias nuevas en el go.sum de los consumidores
//     (mismo criterio que la caché de JWKS del verifier).
//   - El Stub es fail-closed y ruidoso: sin backend configurado devuelve
//     error, jamás "asumo que la referencia existe". Qué implementación corre
//     lo decide la config al arrancar, nunca un sondeo en runtime.
package divisions

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"

	divisionsv1 "github.com/hs-javierviquez/strix-core-kit/gen/divisions/v1"
	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// Errores centinela del cliente.
var (
	ErrNotConfigured = errors.New("divisions: cliente no configurado")
	ErrNotFound      = errors.New("divisions: división no encontrada")
	ErrNoTenant      = errors.New("divisions: no hay tenant en el contexto")
)

// RefStatus reporta la validez de UNA referencia. Reason viene vacío cuando
// Valid es true; cuando es false explica por qué, en palabras aptas para el
// usuario del consumidor.
type RefStatus struct {
	Valid  bool
	Reason string
}

// Client son las tres operaciones que un core consumidor necesita. La
// interfaz es deliberadamente estrecha: cuanto más ancha, más difícil
// mantener los módulos separables.
type Client interface {
	// ValidateRefs valida un LOTE de referencias en una pasada — nunca un RPC
	// por nodo: la latencia de un consumidor no puede ser función de la
	// profundidad del árbol. Válida = existe, activa y toda su cadena de
	// ancestros activa.
	ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error)
	// Subtree devuelve los ids del subárbol de rootID, él incluido — el
	// conjunto para `WHERE division_id = ANY($ids)`.
	Subtree(ctx context.Context, rootID int64) ([]int64, error)
	// Path devuelve los codes desde la raíz hasta id unidos por "/" — lo que
	// un hecho transaccional guarda como snapshot.
	Path(ctx context.Context, id int64) (string, error)
}

// Stub es el cliente sin backend: TODO falla con ErrNotConfigured. Existe
// para que un core arranque sin divisions configurado con una rama visible,
// nunca con un "todo es válido" silencioso.
type Stub struct{}

func (Stub) ValidateRefs(context.Context, []int64) (map[int64]RefStatus, error) {
	return nil, ErrNotConfigured
}
func (Stub) Subtree(context.Context, int64) ([]int64, error) { return nil, ErrNotConfigured }
func (Stub) Path(context.Context, int64) (string, error)     { return "", ErrNotConfigured }

// DefaultTTL es la vida de la foto del árbol en caché.
const DefaultTTL = time.Minute

// callTimeout acota cada GetTree; sin WaitForReady: un core-divisions caído
// debe fallar rápido y ruidoso, no encolar.
const callTimeout = 10 * time.Second

// GRPCClient es el cliente real contra core-divisions.
type GRPCClient struct {
	conn        *grpc.ClientConn
	api         divisionsv1.DivisionsServiceClient
	costCenters divisionsv1.CostCentersServiceClient
	assetTypes  divisionsv1.AssetTypesServiceClient
	ttl         time.Duration
	now         func() time.Time // seam de test

	mu      sync.Mutex
	cache   map[string]snapshot // por tenant — el token del caller viaja, la caché no se comparte entre tenants
	ccCache map[string]ccSnapshot
	atCache map[string]atSnapshot
}

type snapshot struct {
	fetched time.Time
	tree    *tree
}

// Dial conecta con core-divisions (gRPC plaintext intra-cluster, mismo perfil
// de keepalive que el resto de clientes del estate).
func Dial(addr string) (*GRPCClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{
			Time:                30 * time.Second,
			Timeout:             10 * time.Second,
			PermitWithoutStream: true,
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("divisions: dial %s: %w", addr, err)
	}
	return &GRPCClient{
		conn:        conn,
		api:         divisionsv1.NewDivisionsServiceClient(conn),
		costCenters: divisionsv1.NewCostCentersServiceClient(conn),
		assetTypes:  divisionsv1.NewAssetTypesServiceClient(conn),
		ttl:         DefaultTTL,
		now:         time.Now,
		cache:       make(map[string]snapshot),
		ccCache:     make(map[string]ccSnapshot),
		atCache:     make(map[string]atSnapshot),
	}, nil
}

// Close libera la conexión.
func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

func (c *GRPCClient) ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error) {
	t, err := c.treeFor(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]RefStatus, len(ids))
	for _, id := range ids {
		out[id] = t.validate(id)
	}
	return out, nil
}

func (c *GRPCClient) Subtree(ctx context.Context, rootID int64) ([]int64, error) {
	t, err := c.treeFor(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := t.nodes[rootID]; !ok {
		return nil, fmt.Errorf("%w (id %d)", ErrNotFound, rootID)
	}
	out := []int64{rootID}
	for i := 0; i < len(out); i++ {
		out = append(out, t.children[out[i]]...)
	}
	return out, nil
}

func (c *GRPCClient) Path(ctx context.Context, id int64) (string, error) {
	t, err := c.treeFor(ctx)
	if err != nil {
		return "", err
	}
	n, ok := t.nodes[id]
	if !ok {
		return "", fmt.Errorf("%w (id %d)", ErrNotFound, id)
	}
	parts := []string{}
	for steps := 0; steps <= len(t.nodes); steps++ {
		parts = append(parts, n.code)
		if n.parent == 0 {
			for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
				parts[i], parts[j] = parts[j], parts[i]
			}
			return strings.Join(parts, "/"), nil
		}
		if n, ok = t.nodes[n.parent]; !ok {
			break
		}
	}
	return "", fmt.Errorf("divisions: cadena de ancestros rota para %d", id)
}

// treeFor devuelve la foto del árbol del tenant del contexto, de caché si
// está fresca. Dos goroutines del mismo tenant pueden traerla a la vez en el
// borde del TTL; el árbol es pequeño y la duplicación ocasional es más barata
// que un singleflight.
func (c *GRPCClient) treeFor(ctx context.Context) (*tree, error) {
	tenant := tenantctx.Tenant(ctx)
	if tenant == "" {
		return nil, ErrNoTenant
	}

	c.mu.Lock()
	if s, ok := c.cache[tenant]; ok && c.now().Sub(s.fetched) < c.ttl {
		c.mu.Unlock()
		return s.tree, nil
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(forward(ctx), callTimeout)
	defer cancel()
	resp, err := c.api.GetTree(callCtx, &divisionsv1.GetTreeRequest{})
	if err != nil {
		return nil, fmt.Errorf("divisions: get tree: %w", err)
	}
	t := buildTree(resp)

	c.mu.Lock()
	c.cache[tenant] = snapshot{fetched: c.now(), tree: t}
	c.mu.Unlock()
	return t, nil
}

// forward reenvía el bearer del CALLER como metadata saliente. Llamar con
// identidad de servicio haría que core-divisions confíe en la palabra de ESTE
// core sobre quién pregunta — exactamente la confusión que la audiencia por
// core existe para prevenir.
func forward(ctx context.Context) context.Context {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ctx
	}
	if auth := md.Get("authorization"); len(auth) > 0 {
		return metadata.AppendToOutgoingContext(ctx, "authorization", auth[0])
	}
	return ctx
}

// --- la foto local del árbol ---

type node struct {
	parent int64
	code   string
	active bool
}

type tree struct {
	nodes    map[int64]*node
	children map[int64][]int64
}

func buildTree(resp *divisionsv1.Tree) *tree {
	t := &tree{
		nodes:    make(map[int64]*node, len(resp.GetDivisions())),
		children: make(map[int64][]int64),
	}
	for _, d := range resp.GetDivisions() {
		t.nodes[d.GetId()] = &node{parent: d.GetParentId(), code: d.GetCode(), active: d.GetActive()}
		if p := d.GetParentId(); p != 0 {
			t.children[p] = append(t.children[p], d.GetId())
		}
	}
	return t
}

// validate replica la semántica del core (la fuente normativa es
// internal/tree de strix-divisions): válida = existe, activa Y toda su
// cadena de ancestros activa — desactivar un nodo cierra su subárbol.
func (t *tree) validate(id int64) RefStatus {
	n, ok := t.nodes[id]
	if !ok {
		return RefStatus{Reason: "no existe"}
	}
	if !n.active {
		return RefStatus{Reason: "división inactiva"}
	}
	for cur := n; cur.parent != 0; {
		cur, ok = t.nodes[cur.parent]
		if !ok {
			return RefStatus{Reason: "cadena de ancestros rota"}
		}
		if !cur.active {
			return RefStatus{Reason: fmt.Sprintf("ancestro inactivo: %s", cur.code)}
		}
	}
	return RefStatus{Valid: true}
}
