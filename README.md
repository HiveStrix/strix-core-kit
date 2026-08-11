# strix-core-kit

La mecánica que todo Core de Hivestrix repite: validar el token que el Shell
intermedia, delegar la decisión de permiso al PDP, resolver la base del tenant y
sacar los eventos del outbox. Vive aquí para que exista **una** copia y no una
por repo.

## Por qué existe

Cuatro Cores —`strix-expenses`, `strix-costing`, `strix-clients` y `core-tasks`—
llevaban cada uno su copia de `auth` y `pdp`, en dos generaciones. El riesgo no
era la repetición sino la divergencia silenciosa: ambas copias compilan, ambas
pasan sus pruebas, y la que se queda atrás sigue aceptando lo que la otra ya
rechaza sin que nada lo señale.

Cuando se midió, ya había pasado. La copia sin timeout dejaba que un PDP colgado
retuviera la petición en lugar de denegarla, y —lo más caro— el propio `.proto`
del contrato estaba duplicado: cada Core había editado el comentario del campo
`action` para describir lo que ya hacía, hasta que uno nombró sus acciones de
forma que un `forbid` de la política dejó de coincidir por nombre y cualquier
`member` podía borrar clientes.

El detalle completo está en
[gitops#38](https://github.com/hs-javierviquez/Hivestrix-gitops/issues/38).

## Qué hay

| Paquete | Qué es |
|---|---|
| `auth` | El PEP: verificación local del access token (EdDSA fijado en código, `typ=at+jwt`, `iss`, `aud` any-of, `tenant_id` obligatorio) y el interceptor gRPC que siembra la identidad en el contexto |
| `pdp` | Cliente de `CheckPermission`, fail-closed y con deadline |
| `authz` | El gate deny-by-default: entitlement local y decisión delegada al PDP |
| `tenantctx` | El tenant y el subject verificados, a través del contexto |
| `textnorm` | Normalización de nombres para búsqueda sin `unaccent` |
| `decimals` | Límites de magnitud y precisión para los números que manda un usuario, antes de que lleguen al cálculo o a la base |
| `gen/authorization/v1` | Stubs del contrato PEP↔PDP, generados de una sola copia del proto |

Pendientes de fases posteriores: la tenancy (pools por tenant, resolución de
DSN), el relay del outbox, `sanitize` y los helpers de `config`.

## Cómo se nombran las acciones

`<module>[.<grupo>...].<verbo>`. El PDP toma el módulo del **primer** segmento y
el verbo del **último**; lo de en medio es libre.

El verbo se deriva, nunca se declara: el kit no envía `context` al PDP y el PDP
ignora las claves que él mismo deriva. **Nombrar la acción es elegir el nivel al
que se la juzga.** Una acción que requiere `tenant-admin` termina en un verbo que
lo diga —`expenses.taxrate.admin`— en vez de tomar prestado el nombre de una
escritura y pedir un trato distinto por otro canal.

El gate además rechaza una acción cuyo primer segmento no sea el módulo del
core: es un error de programación que, sin la comprobación, se manifiesta como
la petición evaluada contra las reglas de otro módulo.

## Uso

```go
verifier := auth.NewVerifier(cfg.JWKSURL, cfg.Issuer, cfg.Audiences)
srv := grpc.NewServer(grpc.UnaryInterceptor(auth.UnaryServerInterceptor(verifier)))

pdpClient, err := pdp.Dial(cfg.AuthzGRPCAddr)
```

Dentro de un handler, la identidad se lee del contexto y **nunca** del cuerpo de
la petición: algunos mensajes proto traen un `tenant_id` que es heredado e
informativo.

```go
claims, _ := auth.ClaimsFrom(ctx)
tenant := tenantctx.Tenant(ctx)
```

Todo número que venga de un usuario pasa por `decimals` **en la frontera**, antes
del cálculo y antes de la base:

```go
monto, err := decimals.Parse(req.GetAmount(), "monto")   // límites Money
factor, err := decimals.ParseWith(s, "factor", decimals.Factor)
```

No es una validación de formulario que el front pueda cubrir. Un `decimal` cuesta
casi nada en memoria y **un byte por dígito al renderizarlo**, y un Core renderiza
todo lo que persiste: un `1e9` que entre sin filtro se convierte en ~1 GB de
asignación al escribirlo de vuelta. Así murió `core-expenses` dos veces
(`OOMKilled`, sin dejar log, porque `SIGKILL` no deja escribir). El chequeo es
barato precisamente porque nunca renderiza.

Ojo con los números que van a la base **como string** sin parsearse en Go: ahí no
hay nada que los rechace, y `numeric` de Postgres acepta magnitudes que después
nadie puede leer de vuelta.

## El proto

`proto/authorization/v1/authorization.proto` es una copia sincronizada; el
original vive en `strix-auth`, que es quien implementa el servicio. `make
check-proto` falla si divergió en algo que no sea el `go_package`, y `make
sync-proto` la repone. Ese check es justamente la red que no existía antes.

## Desarrollo

```
make test          # go test ./...
make generate      # buf generate
make check-proto   # verifica la copia contra strix-auth (necesita gh)
```
