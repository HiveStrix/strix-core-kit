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
[gitops#38](https://github.com/HiveStrix/Hivestrix-gitops/issues/38).

## Qué hay

| Paquete | Qué es |
|---|---|
| `auth` | El PEP: verificación local del access token (EdDSA fijado en código, `typ=at+jwt`, `iss`, `aud` any-of, `tenant_id` obligatorio) y el interceptor gRPC que siembra la identidad en el contexto. Desde v0.12.0, también la identidad DE SERVICIO del core (`ServiceTokens`: `client_credentials` contra strix-auth, un token por tenant y audiencia, security-contract §5.6) y `Outgoing(ctx, tokens, aud)`, que reenvía el bearer del usuario cuando lo hay y acuña el de servicio solo cuando no hay nadie detrás de la llamada (un consumidor de eventos, un job) Desde v0.20.0 (contrato AI ready §1.1), `Claims` lee `principal_type`, `via`, `installation_id`, `agent_id`, `agent_version`, `provider_id`, `origin`, `tier` y `act.sub`; `PrincipalType()` decide por el claim y, si falta, por la heurística de siempre (`sub == client_id` → servicio). `IsAgent()` y `Via()` son nuevos, e `IsService()` sigue a `PrincipalType()`: un token de agente tiene `sub == client_id` y **no** es un servicio |
| `pdp` | Cliente de `CheckPermission`, fail-closed y con deadline Desde v0.20.0, `Request` suma `SubjectType`, `PrincipalAttributes`, `Via` y `ConsistencyToken`; `Check` devuelve la `Decision` entera (con `DecidedAt`) y `BatchAllowed` verifica hasta `MaxBatch` (100) ítems del mismo tenant y sujeto en un viaje, todo o nada: un error, un PDP sin la RPC o una cantidad de resultados distinta no devuelve ninguna decisión |
| `authz` | El gate deny-by-default: entitlement local y decisión delegada al PDP; a un principal de servicio (`Claims.IsService`) lo decide el scope (`core.read` / `core.write` por el verbo de la acción, `ScopeFor`), nunca el PDP. Desde v0.18.0, `authz.New(pdp, módulo, authz.MachineAllow(acción, clientIDs...)...)` suma una **allowlist de `client_id` por acción exacta**, opt-in: con al menos una entrada, una máquina pasa solo si está listada para ESA acción **y** su scope alcanza, y toda acción que no aparece le queda cerrada a cualquier máquina; sin ninguna, el gate se comporta igual que antes. Las personas no cambian. Una acción de otro módulo, sin verbo, o un `client_id` vacío o con espacios hacen fallar `New` al arrancar (panic, error de programación). Con allowlist, toda denegación a una máquina —también la de scope insuficiente— es la misma `authz: denied` del PDP: no dice qué clientes existen, si la acción tiene entrada ni si el propio llamante está listado (el motivo queda en el log) Desde v0.20.0 el gate mira **primero** `PrincipalType()` (§1.3): un **agente** pasa por entitlement y PDP como `subject_type: agent`, sujeto = `installation_id`, con `origin`/`tier`/`provider`/`installation_id` del token como atributos, nunca por scope ni por la allowlist; un token con **`via`** (el asistente) recibe `PermissionDenied` en toda acción que no sea de lectura **antes** del PDP, y después va como la persona con `Via` al PDP; un **servicio** también pasa con un scope igual al nombre exacto de la acción (§6.1), siempre bajo `MachineAllow`; un `principal_type` desconocido se niega. Qué es lectura lo dice `authz.Effects(cat.Effects())`; sin catálogo, solo los verbos `read`, `list` y `get` (más estrecho que `ScopeFor` a propósito), y con catálogo una acción que no figura **no** es lectura |
| `capabilities` | Desde v0.20.0, el lector del catálogo de operaciones de cada core (`capabilities/catalog.yaml`, contrato AI ready §1.4): `Load`/`Parse`/`MustParse` validan campos y enums (campo desconocido, `id` distinto de `action`, `rpc`/`id`/`action` repetidos: todo error, todos juntos) y devuelven `Effects()` (acción → efecto, para `authz.Effects`), `MethodEffects()` (método gRPC → efecto, para `idempotency`), `Risks()` y `Lookup`. `IsRead(effects, acción)` es LA clasificación de lectura de la plataforma. El chequeo de CI es `cmd/capcheck` (ver «Catálogo de operaciones») |
| `tenantctx` | El tenant y el subject verificados, a través del contexto |
| `tenancy` | Un pool por tenant (LRU, apertura perezosa), resolución de DSN por plantilla, `Base` para repositorios (Conn/InTx/InTxFor), migraciones goose y descubrimiento de tenants por Postgres |
| `outbox` | El outbox transaccional (`outbox`, `processed_events`) y el relay que lo drena a JetStream. Desde v0.15.0 el relay tolera un broker caído o sin DNS al arrancar: `Connect` ya no falla por eso y el stream se crea en cuanto NATS responde. Desde v0.16.0, también el lado del consumidor: `Dial` conecta como el relay (el primer dial reintenta) y `Subscribe(ctx, js, Subscription)` engancha un durable al stream de otro core y no se rinde nunca — broker caído, stream que todavía no existe o durable que no se pudo crear son el mismo "todavía no", y reintenta (5 s doblando hasta `MaxRetry`, 5 min por defecto) hasta engancharse. El ack, el nak con espera y la idempotencia (`MarkProcessed`) siguen siendo del core Desde v0.20.0, el sobre suma `traceparent` y `causation` (`{event_id, principal_type, principal_id, chain}`), opcionales y compatibles: sin ellos el sobre es byte a byte el de antes. La tabla `outbox` es de cada core, así que el kit trae la migración (`outbox.TraceColumnsMigration`, goose, columnas nullable) y el core, una vez aplicada en todos los tenants, escribe con `InsertWith(..., Meta{Traceparent, Causation})` y enciende `Config.TraceColumns` en el relay (sin él, el relay lee lo de siempre; con él y sin columnas, falla en voz alta). El consumidor usa `ParseEnvelope` y `ContextWithEnvelope`; `NextCausation` arma la causa de lo que emite un handler y **se niega** (`ErrCausationCycle`, `ErrCausationTooDeep`, 8 saltos por defecto) cuando el principal ya está en la cadena: el corte del bucle de un agente que reacciona a lo que él mismo causó |
| `divisions` | Cliente de plataforma para el árbol organizacional (`ValidateRefs` batch, `Subtree`, `Path`) y, desde v0.10.0, para los catálogos de plataforma por el mismo `Dial`: `CostCenters()` y `AssetTypes()` (interfaz `Catalogs`), cada uno con su caché por tenant y stub fail-closed. Desde v0.14.0, `Client` expone `Ancestors` (ids raíz..nodo) y `CostCentersClient` `Anchor` (el `division_id` del centro de costo) y el paquete agrega `CheckCoherence(ctx, cats, divisionID, costCenterID)` — función libre, no método de la interfaz — para validar división↔centro de costo (coherente sii el ancla de C es D o un ancestro de D). Cómo lo adopta un core: `Hivestrix-gitops/docs/catalogs-adoption-guide.md` |
| `parties` | Cliente de plataforma para terceros (`core-clients`): `LookupSuppliers` batch, con el token del caller y SIN caché — `active` e `issues_receipt` deben ser frescos al escribir una compra. `Dial(addr, WithServiceTokens(ts))` para las llamadas sin usuario; ídem en `divisions` |
| `hcmrules` | Desde v0.17.0, el evaluador del motor de reglas HCM (ADR `Hivestrix-gitops/docs/decisions/hcm-cores.md` §2), funciones puras sin I/O: `EvalFormula`/`CheckEligibility` resuelven un `kind` registrado (nunca un DSL), `EvalBracket`, `ResolveCalendar` y `DecodeValues`, que convierte los coeficientes JSON de `GetRuleset` a decimales exactos sin pasar por float. Los datos viven en `core-hcmrules`; el consumidor pide la foto una vez y evalúa local. Vino de `strix-hcm-rules/pkg/rules` al llegar su segundo consumidor (`core-leave`), y agrega los kinds `vacation_proportional` y `vacation_period_vesting`. Desde v0.18.0, para `core-payroll`: `cesantia_cr_art29` (CT art. 29 como lo aplica el MTSS: la tasa de la fila de los años **completos** × los años contados —la fracción suma un año pero no sube de fila— con tope, y días fijos bajo el año; los bordes de 6 y 12 meses son coeficientes, no código), `fixed_term_indemnity_cr` (art. 31: un día por cada 7 trabajados o fracción, con mínimos de 3 y 22), `absence_employer_share` (la parte patronal de un día de ausencia subsidiada por bandas, p. ej. enfermedad CCSS 50 % días 1-3), el coeficiente opcional `band<i>_exclusive` en `preaviso_cr` (art. 28 dice «exceda de» seis meses y «después de» un año; sin el flag sigue `>=`, así que las filas sembradas no cambian; `band_count` negativo o fraccionario pasa a ser error) y `EvalBracketIncremental(tiers, antes, después)` para el ISR acumulado del mes. Todo conteo de bandas o filas (`band_count`, `row_count`, `sub_year_band_count`) tiene que ser un entero en [mínimo, 100], comprobado sobre el decimal antes de convertirlo: `core-hcmrules` evalúa en su preview los coeficientes que manda el llamante, y un conteo enorme no puede dimensionar una reserva de memoria. `cesantia_cr` queda registrado solo para que sus filas sigan evaluando hasta cerrarse: su forma acumulativa no es la del art. 29 |
| `fx/bccr` | Desde v0.18.0, el cliente del API REST SDDE del BCCR para el tipo de cambio de referencia (317 compra, 318 venta), el primero de la plataforma. Token Bearer por config (secreto de plataforma), timeout por intento, reintentos con backoff que respeta `Retry-After` **solo** en 429/5xx, valores `decimal` leídos del literal JSON (nunca float), y cada `Rate` lleva el cuerpo crudo y su SHA-256 como evidencia. Una fecha sin valor es `ErrNotPublished`, nunca el día anterior (el BCCR publica todos los días); 401/403 casan con `ErrTokenRejected` para avisar que hay que regenerar el token, y 404 con `ErrEndpointNotFound`, porque el estándar también da un token vencido como causa de un 404 (el job alerta por ambos). Nunca sigue un redirect: el token solo va a la URL base configurada. Sin caché: guardar lo leído (el `fx_rates` inmutable de payroll) y no volver a pedirlo en un recálculo es del core |
| `textnorm` | Normalización de nombres para búsqueda sin `unaccent` |
| `decimals` | Límites de magnitud y precisión para los números que manda un usuario, antes de que lleguen al cálculo o a la base |
| `gen/authorization/v1` | Stubs del contrato PEP↔PDP, generados de una copia sincronizada del proto |
| `gen/divisions/v1` | Stubs del contrato de `core-divisions` (árbol, centros de costo, tipos de activo), ídem |
| `gen/clients/v1` | Stubs del contrato de `core-clients`, ídem |

Pendientes de fases posteriores: `sanitize` y los helpers de `config`.

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

Un core que sirve datos sensibles a otros cores cierra su gate a las máquinas
que no nombra (desde v0.18.0):

```go
gate := authz.New(pdpClient, "people",
	authz.MachineAllow("people.employment_records.read", "core-time-m2m", "core-leave-m2m"),
	authz.MachineAllow("people.compensation.read", "core-payroll-m2m"),
)
```

La allowlist es **por acción, no por RPC**: dos RPC que comparten nombre de
acción comparten entrada. Si a una la necesita una máquina y a la otra no,
dales acciones distintas (los segmentos del medio son libres).

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

## Catálogo de operaciones

Cada core declara en `capabilities/catalog.yaml` una entrada por RPC pública
(esquema en el contrato AI ready §1.4) y lo embebe:

```go
//go:embed capabilities/catalog.yaml
var catalogYAML []byte

cat := capabilities.MustParse(catalogYAML)
gate := authz.New(pdpClient, "billing", authz.Effects(cat.Effects()))
```

En su CI corre el chequeo, que falla si una RPC del core no está catalogada,
si una entrada nombra una RPC que no existe o con otro request/response, si
hay `id`/`action`/`rpc` repetidos o si la acción no es de los módulos dados:

```
go run github.com/hs-javierviquez/strix-core-kit/cmd/capcheck@v0.20.0 \
    --catalog capabilities/catalog.yaml --proto proto \
    --package billing.v1 --module billing
```

`--proto`, `--package` y `--module` se repiten. `--package` limita el chequeo
a los servicios de esos paquetes: las copias sincronizadas de contratos ajenos
que un core guarda bajo `proto/` no son suyas para catalogar. Los protos se
parsean sin compilar (no hace falta buf ni googleapis). Salida 0 limpio, 1
problemas del catálogo (todos impresos), 2 uso o E/S.

## Los protos

Los `.proto` bajo `proto/` son **copias sincronizadas**; el original de cada
uno vive en el repo que implementa el servicio (`strix-auth` para
`authorization.v1`, `strix-divisions` para los tres archivos de `divisions.v1`,
`strix-clients` para `clients.v1` — la lista es `SYNCED` en el Makefile). El contrato empieza en `syntax = `: la prosa
anterior es la cabecera de cada repo y no participa del diff. `make
check-proto` falla si alguna copia divergió en algo que no sea el
`go_package`, y `make sync-proto` las repone.

El guardián de verdad vive en el CI del repo DUEÑO de cada contrato (los
repos privados pueden leer esta copia pública; al revés no): strix-auth,
strix-divisions y strix-clients fallan su CI si esta copia queda atrás. La de authorization
divergió dos semanas sin ese guardián (2026-08); no volvió a pasar.

## Desarrollo

```
make test          # go test ./...
make generate      # buf generate
make check-proto   # verifica la copia contra strix-auth (necesita gh)
```
