.PHONY: test generate sync-proto check-proto

test:
	go test ./...

generate:
	buf generate

# Copias sincronizadas del kit: "<ruta gh api del original>=<copia local>".
# El original es el dueño de cada contrato; la copia local solo difiere en su
# go_package y en la cabecera que explica por qué existe. La copia anterior
# —una por Core, sin nada que las midiera— divergió sin aviso (gitops#38), y
# la de authorization volvió a divergir dos semanas hasta que el guardián
# entró al CI de strix-auth (2026-08-24). Cada original vigila su copia desde
# SU CI (el repo privado puede leer este repo público; al revés no).
SYNCED := \
	repos/hs-javierviquez/strix-auth/contents/proto/authorization/v1/authorization.proto=proto/authorization/v1/authorization.proto \
	repos/HiveStrix/strix-divisions/contents/proto/divisions/v1/divisions.proto=proto/divisions/v1/divisions.proto

# El CONTRATO empieza en `syntax = `: lo anterior es prosa de cada repo (la
# cabecera local explica por qué la copia existe; la del original, sus
# decisiones de diseño) y no participa ni del sync ni del diff.

# Trae el cuerpo de cada original y repone el go_package y la cabecera locales.
sync-proto:
	@for pair in $(SYNCED); do \
		up=$${pair%%=*}; local=$${pair##*=}; \
		sed -n '1,/^syntax = /p' $$local | sed '$$d' > .proto-header.tmp; \
		grep '^option go_package' $$local > .proto-gopkg.tmp; \
		gh api $$up --jq '.content' | base64 -d | sed -n '/^syntax = /,$$p' > .proto-upstream.tmp; \
		sed "s#^option go_package = .*#$$(cat .proto-gopkg.tmp)#" .proto-upstream.tmp > .proto-body.tmp; \
		cat .proto-header.tmp .proto-body.tmp > $$local; \
		rm -f .proto-*.tmp; \
		echo "$$local resincronizado."; \
	done
	@$(MAKE) generate
	@echo "Revisá el diff antes de commitear."

# Falla si alguna copia divergió de su original en algo que no sea go_package.
check-proto:
	@fail=0; for pair in $(SYNCED); do \
		up=$${pair%%=*}; local=$${pair##*=}; \
		gh api $$up --jq '.content' | base64 -d | sed -n '/^syntax = /,$$p' | sed 's#^option go_package = .*##' > .proto-upstream.tmp; \
		sed -n '/^syntax = /,$$p' $$local | sed 's#^option go_package = .*##' > .proto-local.tmp; \
		if diff .proto-upstream.tmp .proto-local.tmp >/dev/null; then \
			echo "$$local coincide con su original."; \
		else \
			echo "$$local DIVERGIÓ de su original. Corré 'make sync-proto'."; fail=1; \
		fi; \
		rm -f .proto-*.tmp; \
	done; exit $$fail
