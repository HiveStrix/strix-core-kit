// Package parties es el cliente de plataforma para referencias a TERCEROS
// (core-clients): hoy, resolver un lote de proveedores AL ESCRIBIR una
// compra. Un core comprador (expenses, inventory) guarda `supplier_id` como
// id opaco y pregunta aquí si existe, si está activo y si emite recibo —
// nunca confía en el body para eso (shell-core §14.1): `issues_receipt`
// decide si el IVA de la compra es recuperable.
//
// Reglas, las mismas del paquete divisions salvo una:
//
//   - El token del CALLER viaja siempre; nunca una identidad de servicio.
//   - El Stub es fail-closed: sin backend configurado devuelve error.
//   - SIN CACHÉ. `active` e `issues_receipt` tienen que ser frescos en el
//     momento de escribir: un proveedor bloqueado esta mañana no puede
//     recibir compras hasta mediodía porque una foto lo dio por bueno. Es
//     un lookup por escritura, batch, y ese costo es el correcto.
//
// Elegir o buscar proveedores desde una pantalla NO pasa por aquí: la UI
// habla con core-clients por el proxy del Shell (catalogs-adoption-guide.md).
package parties

import (
	"context"
	"errors"
	"fmt"
	"github.com/hs-javierviquez/strix-core-kit/auth"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	clientsv1 "github.com/hs-javierviquez/strix-core-kit/gen/clients/v1"
)

// Errores centinela del cliente.
var (
	ErrNotConfigured = errors.New("parties: cliente no configurado")
)

// SupplierRef es la proyección mínima de un proveedor que un core comprador
// necesita al escribir. Un id ausente en el mapa = el proveedor no existe (o
// no es proveedor) en este tenant.
type SupplierRef struct {
	ID            int64
	Code          string
	Name          string
	Active        bool
	IssuesReceipt bool
	CreditDays    int32
}

// Client es la única operación que un core comprador necesita.
type Client interface {
	// LookupSuppliers resuelve un LOTE de ids en una pasada. Los que no
	// existen simplemente no vienen: el caller decide qué significa eso.
	LookupSuppliers(ctx context.Context, ids []int64) (map[int64]SupplierRef, error)
}

// Stub es el cliente sin backend: falla con ErrNotConfigured.
type Stub struct{}

func (Stub) LookupSuppliers(context.Context, []int64) (map[int64]SupplierRef, error) {
	return nil, ErrNotConfigured
}

// callTimeout acota cada lookup; sin WaitForReady: un core-clients caído
// debe fallar rápido y ruidoso, no encolar la compra.
const callTimeout = 10 * time.Second

// GRPCClient es el cliente real contra core-clients.
type GRPCClient struct {
	conn *grpc.ClientConn
	api  clientsv1.ClientsServiceClient
	// tokens acuña la identidad de ESTE core para las llamadas sin usuario
	// detrás (un consumidor de eventos); nil = solo reenviar el bearer.
	tokens *auth.ServiceTokens
}

// Audience es la audiencia que core-clients acepta en un token de servicio.
const Audience = "core-clients"

// Option configura Dial.
type Option func(*GRPCClient)

// WithServiceTokens permite que las llamadas sin bearer de usuario —un
// consumidor, un job— identifiquen a este core con un token client_credentials
// del tenant en contexto (security-contract §5.6). El bearer de una persona,
// cuando lo hay, sigue viajando tal cual: ver auth.Outgoing.
func WithServiceTokens(tokens *auth.ServiceTokens) Option {
	return func(c *GRPCClient) { c.tokens = tokens }
}

// Dial conecta con core-clients (gRPC plaintext intra-cluster, mismo perfil
// de keepalive que el resto de clientes del estate).
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
		return nil, fmt.Errorf("parties: dial %s: %w", addr, err)
	}
	c := &GRPCClient{conn: conn, api: clientsv1.NewClientsServiceClient(conn)}
	for _, o := range opts {
		o(c)
	}
	return c, nil
}

// Close libera la conexión.
func (c *GRPCClient) Close() error {
	return c.conn.Close()
}

func (c *GRPCClient) LookupSuppliers(ctx context.Context, ids []int64) (map[int64]SupplierRef, error) {
	out := make(map[int64]SupplierRef, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	req := &clientsv1.LookupSuppliersRequest{Ids: make([]uint32, 0, len(ids))}
	for _, id := range ids {
		// El contrato de clients usa uint32; un id negativo o desbordado no
		// puede existir y no se pregunta.
		if id > 0 && id <= 1<<32-1 {
			req.Ids = append(req.Ids, uint32(id))
		}
	}
	outCtx, err := auth.Outgoing(ctx, c.tokens, Audience)
	if err != nil {
		return nil, fmt.Errorf("parties: %w", err)
	}
	callCtx, cancel := context.WithTimeout(outCtx, callTimeout)
	defer cancel()
	resp, err := c.api.LookupSuppliers(callCtx, req)
	if err != nil {
		return nil, fmt.Errorf("parties: lookup suppliers: %w", err)
	}
	for _, s := range resp.GetSuppliers() {
		out[int64(s.GetId())] = SupplierRef{
			ID:            int64(s.GetId()),
			Code:          s.GetCode(),
			Name:          s.GetName(),
			Active:        s.GetActive(),
			IssuesReceipt: s.GetIssuesReceipt(),
			CreditDays:    s.GetCreditDays(),
		}
	}
	return out, nil
}
