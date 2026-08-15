package tenancy

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"net/url"
	"sort"
	"strings"
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
	adminDSN, pattern, err := adminAndPattern(template)
	if err != nil {
		return nil, err
	}

	db, err := sql.Open("pgx", adminDSN)
	if err != nil {
		return nil, fmt.Errorf("tenancy: open admin connection: %w", err)
	}
	defer db.Close()

	rows, err := db.QueryContext(ctx,
		`SELECT datname FROM pg_database WHERE datname LIKE $1 AND datallowconn ORDER BY datname`,
		pattern.like)
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
			failures = append(failures, fmt.Sprintf("%s: %v", slug, err))
			continue
		}
		if !ok {
			continue
		}
		if err := Migrate(dsn, migrations); err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", slug, err))
			continue
		}
		migrated = append(migrated, slug)
	}
	if len(failures) > 0 {
		return migrated, fmt.Errorf("tenancy: migraciones fallidas en %d de %d tenants:\n  %s",
			len(failures), len(tenants), strings.Join(failures, "\n  "))
	}
	return migrated, nil
}

// hasSchema reports whether the Core's schema exists in a tenant database.
//
// Needs nothing but CONNECT: information_schema is readable by any role, so the
// check itself never becomes the reason a migration cannot run.
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
