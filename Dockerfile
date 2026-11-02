FROM golang:1.27.3 AS build
WORKDIR /src

# go.mod copiado antes do código para a camada de download ser reaproveitada
# enquanto as dependências não mudarem. go.sum* não falha enquanto não existir.
COPY go.mod go.sum* ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 porque a imagem final não tem libc; -trimpath tira caminhos
# locais do binário e -s -w remove tabela de símbolos e DWARF.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

# static em vez de scratch: traz certificados raiz (HTTPS aos provedores),
# tzdata e o usuário nonroot. Trade-offs no ADR 0001.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /gateway
USER nonroot:nonroot
ENTRYPOINT ["/gateway"]
