// Los catálogos de plataforma de core-divisions (divisions v2): centros de
// costo y tipos de activo. Mismas reglas que el árbol organizacional — token
// del caller, caché del catálogo COMPLETO por tenant con TTL corto, cómputo
// local, stub fail-closed — expuestas como sub-clientes del mismo Dial, de
// modo que un core que ya adoptó el árbol adopta los otros dos catálogos
// sin una conexión ni una config nuevas.
//
// Las interfaces existentes no cambian: Client sigue siendo el árbol. Los
// catálogos se piden por Catalogs, que *GRPCClient y Stub satisfacen; un
// consumidor que solo quiere el árbol no se entera de que existen.
package divisions

import (
	"context"
	"fmt"
	"strings"
	"time"

	divisionsv1 "github.com/hs-javierviquez/strix-core-kit/gen/divisions/v1"
	"github.com/hs-javierviquez/strix-core-kit/tenantctx"
)

// Catalogs agrupa el árbol y los dos catálogos. Es lo que un core que costea
// o clasifica artículos declara en su config en lugar de Client.
type Catalogs interface {
	Client
	CostCenters() CostCentersClient
	AssetTypes() AssetTypesClient
}

// CostCentersClient son las tres operaciones sobre centros de costo, con la
// misma semántica que las del árbol. Válido = existe, activo, cadena de
// ancestros activa y ninguna ventana temporal de la cadena cerrada HOY.
type CostCentersClient interface {
	ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error)
	Subtree(ctx context.Context, rootID int64) ([]int64, error)
	Path(ctx context.Context, id int64) (string, error)
}

// AssetType es la proyección del catálogo que un consumidor necesita para
// derivar su comportamiento (catalogs-adoption-guide.md). Los tres ejes son
// inmutables en el core, así que un consumidor puede decidir con ellos sin
// volver a preguntar.
type AssetType struct {
	ID          int64
	Code        string
	Name        string
	Recognition string
	// Outflows es el conjunto ORDENADO de salidas del valor (sale | consumption |
	// usage | rental): sin repetidos, con al menos una, y la primera es la
	// principal — la que un consumidor propone como default. Inmutable en el
	// core, como los otros dos ejes.
	Outflows  []string
	StockMode string
	Active    bool
}

// AssetTypesClient son las operaciones sobre tipos de activo. Válido =
// existe y activo (no hay jerarquía ni ventana).
type AssetTypesClient interface {
	ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error)
	// Get devuelve un tipo por id, activo o no: un artículo viejo puede
	// referenciar un tipo ya desactivado y su comportamiento no cambia.
	Get(ctx context.Context, id int64) (AssetType, error)
	// List devuelve el catálogo completo, en el orden del core (el id 1 es el
	// tipo por defecto).
	List(ctx context.Context) ([]AssetType, error)
}

// --- Stub ---

// CostCenters del Stub: fail-closed como el resto.
func (Stub) CostCenters() CostCentersClient { return stubCostCenters{} }

// AssetTypes del Stub: fail-closed como el resto.
func (Stub) AssetTypes() AssetTypesClient { return stubAssetTypes{} }

type stubCostCenters struct{}

func (stubCostCenters) ValidateRefs(context.Context, []int64) (map[int64]RefStatus, error) {
	return nil, ErrNotConfigured
}
func (stubCostCenters) Subtree(context.Context, int64) ([]int64, error) { return nil, ErrNotConfigured }
func (stubCostCenters) Path(context.Context, int64) (string, error)     { return "", ErrNotConfigured }

type stubAssetTypes struct{}

func (stubAssetTypes) ValidateRefs(context.Context, []int64) (map[int64]RefStatus, error) {
	return nil, ErrNotConfigured
}
func (stubAssetTypes) Get(context.Context, int64) (AssetType, error) {
	return AssetType{}, ErrNotConfigured
}
func (stubAssetTypes) List(context.Context) ([]AssetType, error) { return nil, ErrNotConfigured }

// --- GRPCClient ---

// CostCenters devuelve el sub-cliente de centros de costo sobre la misma
// conexión.
func (c *GRPCClient) CostCenters() CostCentersClient { return &costCentersClient{c: c} }

// AssetTypes devuelve el sub-cliente de tipos de activo sobre la misma
// conexión.
func (c *GRPCClient) AssetTypes() AssetTypesClient { return &assetTypesClient{c: c} }

type costCentersClient struct{ c *GRPCClient }

func (cc *costCentersClient) ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error) {
	t, err := cc.c.costCentersFor(ctx)
	if err != nil {
		return nil, err
	}
	today := civil(cc.c.now())
	out := make(map[int64]RefStatus, len(ids))
	for _, id := range ids {
		out[id] = t.validate(id, today)
	}
	return out, nil
}

func (cc *costCentersClient) Subtree(ctx context.Context, rootID int64) ([]int64, error) {
	t, err := cc.c.costCentersFor(ctx)
	if err != nil {
		return nil, err
	}
	if _, ok := t.nodes[rootID]; !ok {
		return nil, fmt.Errorf("%w (centro de costo %d)", ErrNotFound, rootID)
	}
	out := []int64{rootID}
	for i := 0; i < len(out); i++ {
		out = append(out, t.children[out[i]]...)
	}
	return out, nil
}

func (cc *costCentersClient) Path(ctx context.Context, id int64) (string, error) {
	t, err := cc.c.costCentersFor(ctx)
	if err != nil {
		return "", err
	}
	n, ok := t.nodes[id]
	if !ok {
		return "", fmt.Errorf("%w (centro de costo %d)", ErrNotFound, id)
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
	return "", fmt.Errorf("divisions: cadena de ancestros rota para el centro de costo %d", id)
}

type assetTypesClient struct{ c *GRPCClient }

func (at *assetTypesClient) ValidateRefs(ctx context.Context, ids []int64) (map[int64]RefStatus, error) {
	cat, err := at.c.assetTypesFor(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]RefStatus, len(ids))
	for _, id := range ids {
		a, ok := cat.byID[id]
		switch {
		case !ok:
			out[id] = RefStatus{Reason: "no existe"}
		case !a.Active:
			out[id] = RefStatus{Reason: "tipo de activo inactivo"}
		default:
			out[id] = RefStatus{Valid: true}
		}
	}
	return out, nil
}

func (at *assetTypesClient) Get(ctx context.Context, id int64) (AssetType, error) {
	cat, err := at.c.assetTypesFor(ctx)
	if err != nil {
		return AssetType{}, err
	}
	a, ok := cat.byID[id]
	if !ok {
		return AssetType{}, fmt.Errorf("%w (tipo de activo %d)", ErrNotFound, id)
	}
	return *a, nil
}

func (at *assetTypesClient) List(ctx context.Context) ([]AssetType, error) {
	cat, err := at.c.assetTypesFor(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]AssetType, len(cat.items))
	copy(out, cat.items)
	return out, nil
}

// --- fotos en caché, por tenant ---

type ccSnapshot struct {
	fetched time.Time
	tree    *ccTree
}

type atSnapshot struct {
	fetched time.Time
	catalog *atCatalog
}

func (c *GRPCClient) costCentersFor(ctx context.Context) (*ccTree, error) {
	tenant := tenantctx.Tenant(ctx)
	if tenant == "" {
		return nil, ErrNoTenant
	}
	c.mu.Lock()
	if s, ok := c.ccCache[tenant]; ok && c.now().Sub(s.fetched) < c.ttl {
		c.mu.Unlock()
		return s.tree, nil
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(forward(ctx), callTimeout)
	defer cancel()
	resp, err := c.costCenters.GetCostCenterTree(callCtx, &divisionsv1.GetCostCenterTreeRequest{})
	if err != nil {
		return nil, fmt.Errorf("divisions: get cost center tree: %w", err)
	}
	t := buildCostCenters(resp)

	c.mu.Lock()
	c.ccCache[tenant] = ccSnapshot{fetched: c.now(), tree: t}
	c.mu.Unlock()
	return t, nil
}

func (c *GRPCClient) assetTypesFor(ctx context.Context) (*atCatalog, error) {
	tenant := tenantctx.Tenant(ctx)
	if tenant == "" {
		return nil, ErrNoTenant
	}
	c.mu.Lock()
	if s, ok := c.atCache[tenant]; ok && c.now().Sub(s.fetched) < c.ttl {
		c.mu.Unlock()
		return s.catalog, nil
	}
	c.mu.Unlock()

	callCtx, cancel := context.WithTimeout(forward(ctx), callTimeout)
	defer cancel()
	resp, err := c.assetTypes.ListAssetTypes(callCtx, &divisionsv1.ListAssetTypesRequest{IncludeInactive: true})
	if err != nil {
		return nil, fmt.Errorf("divisions: list asset types: %w", err)
	}
	cat := buildAssetTypes(resp)

	c.mu.Lock()
	c.atCache[tenant] = atSnapshot{fetched: c.now(), catalog: cat}
	c.mu.Unlock()
	return cat, nil
}

// --- la foto local de centros de costo ---

type ccNode struct {
	parent int64
	code   string
	active bool
	starts time.Time // cero = sin límite
	ends   time.Time
}

type ccTree struct {
	nodes    map[int64]*ccNode
	children map[int64][]int64
}

func buildCostCenters(resp *divisionsv1.CostCenterTree) *ccTree {
	t := &ccTree{
		nodes:    make(map[int64]*ccNode, len(resp.GetCostCenters())),
		children: make(map[int64][]int64),
	}
	for _, cc := range resp.GetCostCenters() {
		t.nodes[cc.GetId()] = &ccNode{
			parent: cc.GetParentId(),
			code:   cc.GetCode(),
			active: cc.GetActive(),
			starts: parseDate(cc.GetStartsOn()),
			ends:   parseDate(cc.GetEndsOn()),
		}
		if p := cc.GetParentId(); p != 0 {
			t.children[p] = append(t.children[p], cc.GetId())
		}
	}
	return t
}

// validate replica la semántica del core (fuente normativa:
// internal/costcenters de strix-divisions): estado, cadena de ancestros y
// ventana temporal heredada por la cadena.
func (t *ccTree) validate(id int64, today time.Time) RefStatus {
	n, ok := t.nodes[id]
	if !ok {
		return RefStatus{Reason: "no existe"}
	}
	if !n.active {
		return RefStatus{Reason: "centro de costo inactivo"}
	}
	if r := n.windowReason(today); r != "" {
		return RefStatus{Reason: r}
	}
	for cur := n; cur.parent != 0; {
		cur, ok = t.nodes[cur.parent]
		if !ok {
			return RefStatus{Reason: "cadena de ancestros rota"}
		}
		if !cur.active {
			return RefStatus{Reason: fmt.Sprintf("ancestro inactivo: %s", cur.code)}
		}
		if r := cur.windowReason(today); r != "" {
			return RefStatus{Reason: fmt.Sprintf("ancestro %s: %s", cur.code, r)}
		}
	}
	return RefStatus{Valid: true}
}

func (n *ccNode) windowReason(today time.Time) string {
	if !n.starts.IsZero() && today.Before(n.starts) {
		return "abre el " + n.starts.Format("2006-01-02")
	}
	if !n.ends.IsZero() && today.After(n.ends) {
		return "cerrado desde el " + n.ends.Format("2006-01-02")
	}
	return ""
}

// parseDate lee "YYYY-MM-DD"; vacío o ilegible = sin límite (el core ya
// validó el formato al escribir).
func parseDate(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return time.Time{}
	}
	return t
}

func civil(t time.Time) time.Time {
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// --- la foto local de tipos de activo ---

type atCatalog struct {
	items []AssetType
	byID  map[int64]*AssetType
}

func buildAssetTypes(resp *divisionsv1.ListAssetTypesResponse) *atCatalog {
	cat := &atCatalog{
		items: make([]AssetType, 0, len(resp.GetAssetTypes())),
		byID:  make(map[int64]*AssetType, len(resp.GetAssetTypes())),
	}
	for _, a := range resp.GetAssetTypes() {
		cat.items = append(cat.items, AssetType{
			ID: a.GetId(), Code: a.GetCode(), Name: a.GetName(),
			Recognition: a.GetRecognition(), Outflows: a.GetOutflows(), StockMode: a.GetStockMode(),
			Active: a.GetActive(),
		})
	}
	for i := range cat.items {
		cat.byID[cat.items[i].ID] = &cat.items[i]
	}
	return cat
}
