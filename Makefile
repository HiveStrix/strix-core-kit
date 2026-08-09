PROTO := proto/authorization/v1/authorization.proto
GO_PACKAGE := option go_package = "github.com/hs-javierviquez/strix-core-kit/gen/authorization/v1;authorizationv1";

.PHONY: test generate sync-proto check-proto

test:
	go test ./...

generate:
	buf generate

# Trae el proto canónico desde strix-auth y repone el go_package del kit,
# conservando la cabecera que explica por qué esta copia existe.
sync-proto:
	@sed -n '1,/^syntax = /p' $(PROTO) | sed '$$d' > .proto-header.tmp
	@gh api repos/hs-javierviquez/strix-auth/contents/proto/authorization/v1/authorization.proto \
		--jq '.content' | base64 -d > .proto-upstream.tmp
	@sed 's#^option go_package = .*#$(GO_PACKAGE)#' .proto-upstream.tmp > .proto-body.tmp
	@cat .proto-header.tmp .proto-body.tmp > $(PROTO)
	@rm -f .proto-header.tmp .proto-upstream.tmp .proto-body.tmp
	@$(MAKE) generate
	@echo "Proto resincronizado. Revisá el diff antes de commitear."

# Falla si la copia divergió del original en algo que no sea el go_package.
# Es la red que faltaba: la copia anterior —una por Core— divergió sin que
# nada lo midiera (gitops#38).
check-proto:
	@gh api repos/hs-javierviquez/strix-auth/contents/proto/authorization/v1/authorization.proto \
		--jq '.content' | base64 -d | sed 's#^option go_package = .*##' > .proto-upstream.tmp
	@sed -n '/^syntax = /,$$p' $(PROTO) | sed 's#^option go_package = .*##' > .proto-local.tmp
	@diff .proto-upstream.tmp .proto-local.tmp \
		&& echo "El proto coincide con el de strix-auth." \
		|| (echo "DIVERGIÓ del proto de strix-auth. Corré 'make sync-proto'."; rm -f .proto-*.tmp; exit 1)
	@rm -f .proto-upstream.tmp .proto-local.tmp
