package tenancy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Tenants lists the tenant databases that exist for a DSN template.
//
// It answers the question the migration fan-out needs — which tenants are
// there — by asking POSTGRES, not Kubernetes. That is the whole point of this
// function existing.
//
// The fan-out used to discover tenants by listing the `db-tenant-*` Secrets of
// the namespace, which required granting its ServiceAccount `list` on secrets.
// In Kubernetes `list` returns the objects WITH their data: there is no verb
// that enumerates names without exposing values, and RBAC has no label
// selectors. So a Core that declared `migrate: fanout` ended up able to read
// every secret in its space — including the other Cores' (gitops#52).
//
// Discovering the same thing from the database needs no cluster permission at
// all, so the ServiceAccount, the Role and the RoleBinding stop existing rather
// than being narrowed.
func Tenants(ctx context.Context, template string) ([]string, error) {
	return enumerateTenants(ctx, template, false)
}

// enumerateTenants lista los slugs de tenant que existen para una plantilla de
// DSN. Con onlyConnectable, la consulta descarta además las bases donde
// current_user no tiene CONNECT —el privilegio que el provisioner reserva a los
// módulos activados—, para que quien solo quiere los tenants activos no abra ni
// una conexión contra los que no lo están (su 42501 no llega al log del motor).
func enumerateTenants(ctx context.Context, template string, onlyConnectable bool) ([]string, error) {
	adminDSN, pattern, err := adminAndPattern(template)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return nil, fmt.Errorf("tenancy: open admin connection: %w", err)
	}
	defer db.Close()

	query := `SELECT datname FROM pg_database WHERE datname LIKE $1 AND datallowconn`
	if onlyConnectable {
		query += ` AND has_database_privilege(current_user, datname, 'CONNECT')`
	}
	query += ` ORDER BY datname`

	rows, err := db.QueryContext(ctx, query, pattern.like)
	if err != nil {
		return nil, fmt.Errorf("tenancy: list tenant databases: %w", err)
	}
	defer rows.Close()

	var tenants []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("tenancy: scan database name: %w", err)
		}
		slug := strings.TrimSuffix(strings.TrimPrefix(name, pattern.prefix), pattern.suffix)
		if slug == "" || !validSlug.MatchString(slug) {
			// A database that matches the shape but whose slug is not one we
			// would ever have created. Skipping it beats building a DSN from a
			// name nobody vouched for.
			continue
		}
		tenants = append(tenants, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("tenancy: read database list: %w", err)
	}
	sort.Strings(tenants)
	return tenants, nil
}

// TenantsWithSchema lista los tenants donde ESTE Core está activado. Criba en
// DOS pasos, para no tocar siquiera las bases donde el Core no entra:
//
//  1. La enumeración descarta las bases sin CONNECT para current_user
//     (has_database_privilege) —el privilegio que el provisioner concede solo a
//     los módulos activados—, así que una base sin el módulo ni recibe una
//     conexión y su 42501 no llega al log del motor.
//  2. Sobre las que quedan, hasSchema confirma que el schema existe: cubre la
//     ventana entre conceder CONNECT y migrar el schema (activación en curso) y,
//     como solo corre sobre tenants ya conectables, no vuelve a martillar nada.
//
// La usa un worker de fondo para barrer el mismo conjunto que el fan-out de
// migraciones trata como activo. Un tenant no activado se excluye EN SILENCIO
// —es el estado normal de un descubrimiento en bucle—; un error REAL se loguea y
// no oculta a los demás: esa base se cae de esta pasada, no el barrido entero.
func TenantsWithSchema(ctx context.Context, template, schema string) ([]string, error) {
	tenants, err := enumerateTenants(ctx, template, true)
	if err != nil {
		return nil, err
	}

	resolver := TemplateResolver{Template: template, Schema: schema}
	var active []string
	for _, slug := range tenants {
		dsn, err := resolver.Resolve(ctx, slug)
		if err != nil {
			slog.ErrorContext(ctx, "tenancy: resolver DSN de tenant", "tenant", slug, "error", err)
			continue
		}
		ok, err := hasSchema(ctx, dsn, schema)
		if err != nil {
			if notActivated(err) != "" {
				continue
			}
			slog.ErrorContext(ctx, "tenancy: verificar activación de tenant", "tenant", slug, "error", err)
			continue
		}
		if ok {
			active = append(active, slug)
		}
	}
	return active, nil
}

// MigrateAll applies a Core's migrations to every tenant database it finds.
//
// This is what the PostSync hook runs: one process, one image, no kubectl and
// no Job per tenant. Errors are collected rather than returned on the first
// failure, because a single broken tenant should not hide the state of the
// rest — the operator wants to know whether it is one database or all of them.
func MigrateAll(ctx context.Context, template, schema string, migrations fs.FS) ([]string, error) {
	tenants, err := Tenants(ctx, template)
	if err != nil {
		return nil, err
	}
	if len(tenants) == 0 {
		return nil, nil
	}

	resolver := TemplateResolver{Template: template, Schema: schema}
	var failures []string
	var migrated []string
	var skipped []string
	skip := func(slug, reason string) {
		// Un tenant saltado se DICE. Callarlo deja al operador con un fan-out
		// que reporta éxito sin explicar sobre cuántas bases actuó, y ese es el
		// peor resultado posible: indistinguible de uno que no migró nada.
		slog.InfoContext(ctx, "tenancy: tenant saltado",
			"tenant", slug, "schema", schema, "motivo", reason)
		skipped = append(skipped, slug)
	}
	for _, slug := range tenants {
		dsn, err := resolver.Resolve(ctx, slug)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", slug, err))
			continue
		}
		// Saltar los tenants donde este Core no está activado.
		//
		// Crear el schema es ACTIVAR EL MÓDULO, y eso lo decide el provisioner
		// del Shell, no un hook de despliegue. La versión anterior lo creaba en
		// toda base de tenant, lo que ponía el schema de un Core en tenants que
		// nunca lo pidieron — y además exigía privilegios de DDL sobre la base
		// entera que la credencial del Core, correctamente, no tiene.
		ok, err := hasSchema(ctx, dsn, schema)
		if err != nil {
			if reason := notActivated(err); reason != "" {
				skip(slug, reason)
				continue
			}
			failures = append(failures, fmt.Sprintf("%s: %v", slug, err))
			continue
		}
		if !ok {
			skip(slug, "el schema no existe: el módulo no está activado en este tenant")
			continue
		}
		if err := Migrate(dsn, migrations); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", slug, err))
			continue
		}
		migrated = append(migrated, slug)
	}
	slog.InfoContext(ctx, "tenancy: fan-out",
		"schema", schema, "tenants", len(tenants),
		"migrados", len(migrated), "saltados", len(skipped), "fallidos", len(failures))
	if len(failures) > 0 {
		return migrated, fmt.Errorf("tenancy: migraciones fallidas en %d de %d tenants:\n  %s",
			len(failures), len(tenants), strings.Join(failures, "\n  "))
	}
	return migrated, nil
}

// SQLSTATEs con los que Postgres contesta a una conexión que no debía ocurrir.
// Se comparan por código y nunca por el texto del mensaje: el texto lo localiza
// lc_messages y no es contrato.
const (
	sqlStateInsufficientPrivilege = "42501"
	sqlStateInvalidCatalogName    = "3D000"
)

// notActivated traduce un error de CONEXIÓN a la razón por la que este Core no
// tiene nada que migrar en ese tenant. Devuelve cadena vacía cuando el error es
// un fallo de verdad.
//
// El permiso de conexión es PRECISAMENTE lo que distingue un tenant que activó
// el módulo de uno que no: el provisioner concede CONNECT sobre db_tenant_<slug>
// solo a los roles de los módulos activados, con CONNECT revocado a PUBLIC. Así
// que el 42501 al conectar es la respuesta de Postgres a "este Core no está en
// este tenant", con la misma semántica que hasSchema devolviendo false — y sin
// esta traducción el salto que hasSchema implementa era inalcanzable justo en el
// caso que existe para cubrir (core-kit#4).
//
// Se exige que el PgError venga dentro de un ConnectError: un 42501 nacido de la
// consulta en sí, y no del handshake, sigue siendo un fallo que merece verse.
func notActivated(err error) string {
	var connErr *pgconn.ConnectError
	if !errors.As(err, &connErr) {
		return ""
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return ""
	}
	switch pgErr.Code {
	case sqlStateInsufficientPrivilege:
		return "sin CONNECT sobre la base: el módulo no está activado en este tenant"
	case sqlStateInvalidCatalogName:
		// La base pudo borrarse entre el SELECT datname y la conexión.
		return "la base del tenant ya no existe"
	default:
		return ""
	}
}

// hasSchema reports whether the Core's schema exists in a tenant database.
//
// La consulta en sí no necesita más que CONNECT — information_schema lo lee
// cualquier rol —, pero CONNECT no está dado: es justo el privilegio que el
// provisioner reserva a los módulos activados. Por eso el error de conexión se
// clasifica con notActivated en vez de contarse como fallo.
func hasSchema(ctx context.Context, dsn, schema string) (bool, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return false, fmt.Errorf("tenancy: open for schema check: %w", err)
	}
	defer db.Close()

	var exists bool
	err = db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		schema).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("tenancy: check schema %q: %w", schema, err)
	}
	return exists, nil
}

// namePattern describes how a tenant slug becomes a database name.
type namePattern struct {
	prefix string
	suffix string
	like   string
}

// adminAndPattern derives, from the tenant DSN template, both a connection that
// can enumerate databases and the shape of a tenant database name.
//
// The pattern comes from the template rather than being hardcoded as
// `db_tenant_%`: if the naming convention ever changes, the query follows it
// instead of silently returning nothing. A fan-out that quietly migrates zero
// tenants is the worst possible failure — it reports success.
func adminAndPattern(template string) (string, namePattern, error) {
	if !strings.Contains(template, "{slug}") {
		return "", namePattern{}, fmt.Errorf("tenancy: DSN template must contain {slug}")
	}
	u, err := url.Parse(template)
	if err != nil {
		return "", namePattern{}, fmt.Errorf("tenancy: invalid DSN template: %w", err)
	}

	dbName := strings.TrimPrefix(u.Path, "/")
	prefix, suffix, found := strings.Cut(dbName, "{slug}")
	if !found {
		return "", namePattern{}, fmt.Errorf("tenancy: the {slug} of the template is not in the database name")
	}

	// `postgres` is the maintenance database every cluster has; connecting
	// there is how you ask about the others without holding one of them open.
	u.Path = "/postgres"
	// The Core's search_path has no meaning on the admin connection, and
	// pointing at a schema that does not exist there would fail the connect.
	q := u.Query()
	q.Del("options")
	u.RawQuery = q.Encode()

	return u.String(), namePattern{
		prefix: prefix,
		suffix: suffix,
		like:   escapeLike(prefix) + "%" + escapeLike(suffix),
	}, nil
}

// escapeLike neutralizes the wildcards of LIKE inside a literal fragment, so a
// prefix containing `_` — which `db_tenant_` does — matches itself instead of
// any character.
func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}
