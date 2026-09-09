# Aprovisionamiento y eco — vincular master data entre cores

Patrón reutilizable para el caso: **una persona da de alta una entidad en un
core (el _solicitante_) que en realidad es dueño de otro core (el _dueño_), y
los dos lados quedan vinculados sin doble captura ni una llamada síncrona.**

El caso que lo estrenó: en `core-expenses` se crea un insumo ("Carne"); ese
insumo ES un artículo de `core-inventory`, que es quien lleva su existencia. Sin
este patrón el usuario tenía que registrarlo dos veces —una en cada módulo— y
después vincularlos a mano. Con él, **un solo registro** en Gastos alcanza: el
artículo nace solo del otro lado y el `inventory_item_id` vuelve por el bus.

No inventa transporte: reusa el outbox transaccional (§9 del SCC) que todo core
ya tiene. Lo que estandariza es **cómo** se establece un id compartido entre dos
cores por eventos, para que el próximo par (billing↔algo, projects↔algo) lo copie
en vez de improvisar un mecanismo distinto.

## Por qué por eventos y no una llamada síncrona

El estate ya tenía dos formas de vincular ids entre cores, y ninguna servía
completa para este caso:

- **Lookup + guardar id** (así liga `core-maintenance` un activo a su artículo):
  una persona busca el id del otro core y se guarda al escribir. Sirve cuando la
  entidad del dueño **ya existe**; no cuando hay que crearla.
- **Clave natural compartida** (así descuenta `core-billing` por `código`): no se
  guarda id, el dueño resuelve por un código común. Frágil y no deja el vínculo
  explícito.

Aprovisionamiento y eco cubre el hueco —**crear** la entidad del dueño y quedarse
con su id— sin acoplar el alta del solicitante a que el dueño esté arriba en ese
instante: si lo está caído, el evento espera en el stream y el vínculo se
completa solo cuando vuelve. Es la misma resiliencia con la que ya viajan los
hechos (una compra, un consumo).

## El handshake

Tres mensajes, los tres por el outbox transaccional (nunca *publish-and-pray*):

```
  solicitante                         bus                          dueño
      │  alta local (misma tx) ─────────────────────────────────────┐
      │  <solicitante>.<entidad>.created.v1  ──────────────────────► │
      │       { source_ref, ...campos que el dueño necesita }        │
      │                                              crea idempotente │
      │                                              por source_ref   │
      │ ◄──────────────────────  <dueño>.<entidad>.linked.v1         │
      │       { source_ref, <dueño>_id, code }                       │
      │  guarda <dueño>_id en la fila de source_ref                  │
```

1. **Solicitud de aprovisionamiento** — `<solicitante>.<entidad>.created.v1`,
   escrito al outbox **en la misma transacción** que el alta local. Payload:

   | campo | qué es |
   |---|---|
   | `source_ref` | clave opaca y determinista `"<core>:<entidad>:<id_local>"`. Es a la vez la correlación del eco y la clave de idempotencia del dueño. |
   | *(campos del dominio)* | lo mínimo que el dueño necesita para crear la entidad. |
   | `actor` | quién lo originó, para la traza. |

   El envelope del outbox ya aporta `event_id`, `tenant_id` y `subject`.

2. **Creación idempotente por el dueño** — un consumer del dueño recibe el
   evento y crea la entidad **con la misma validación que su alta normal**,
   guardando `source_ref` en una columna con índice único por tenant. La
   idempotencia es doble: `event_id` (tabla `processed_events`) descarta el
   redelivery; el índice único de `source_ref` garantiza **una** entidad aunque
   el solicitante reenvíe con otro `event_id`. Un reenvío que encuentra la
   entidad ya creada **no crea otra: reusa la existente y vuelve a emitir el
   eco** (así el solicitante siempre recupera el id, y ese es el camino de
   recuperación para filas viejas sin vincular).

3. **Eco de identidad** — `<dueño>.<entidad>.linked.v1`, escrito al outbox **en
   la misma transacción** que la creación. Payload:

   | campo | qué es |
   |---|---|
   | `source_ref` | el mismo que llegó, verbatim. |
   | `<dueño>_id` | el id canónico de la entidad en el dueño. |
   | `code` | el código legible del dueño (útil para mostrar; opcional guardarlo). |

4. **El solicitante guarda el id** — un consumer del solicitante recibe el eco,
   parsea el id local desde `source_ref` y escribe `<dueño>_id` en su fila.
   Idempotente por `event_id` (y la escritura es idempotente de por sí).

## Reglas que hacen que sea reusable

- **`source_ref` lo define el solicitante y es opaco para el dueño.** El dueño no
  lo interpreta: solo lo usa como clave única y lo devuelve tal cual. Formato
  `"<core>:<entidad>:<id_local>"`, la misma convención que ya usa `external_ref`
  en los hechos (una compra lleva `external_ref = "<document_id>"`, una orden
  `"maintenance:wo:<id>"`).
- **El nombre de cada evento es de quien lo publica**; la _forma_ del payload es
  lo compartido. El dueño define `<dueño>.<entidad>.linked.v1`; el solicitante
  define `<solicitante>.<entidad>.created.v1`. Ambos con los campos de las tablas
  de arriba.
- **La creación en el dueño pasa por su alta normal**, con todas sus
  validaciones y defaults (referencias a otros cores, unidades, numeración). Si
  una validación falla porque un tercer core está caído, el consumer reintenta:
  el redelivery del bus ES el reintento.
- **Nada de auth nueva para el solicitante.** No llama a ninguna escritura del
  dueño: publica un evento. La creación ocurre dentro del consumer del dueño,
  bajo su propia identidad, igual que cualquier otro hecho que le entra por el
  stream.
- **La ventana de carrera es conocida y se acepta.** Entre el alta y el eco
  (segundos) la fila del solicitante no tiene el id todavía; cualquier hecho que
  dependa del vínculo en ese instante se omite y se reconcilia después (el mismo
  reenvío del punto 2 sirve de backfill). No se bloquea el alta para cerrarla.

## Instancia vigente

| Solicitante | Dueño | Solicitud | Eco | id compartido |
|---|---|---|---|---|
| core-expenses (`items`) | core-inventory (`items`) | `expenses.item.created.v1` | `inventory.item.linked.v1` | `expenses.items.inventory_item_id` |

Detalle de cada payload en el `events/catalog.yaml` de cada repo.
