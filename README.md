# llmux

[![ci](https://github.com/Felps-LDL/llmux/actions/workflows/ci.yml/badge.svg)](https://github.com/Felps-LDL/llmux/actions/workflows/ci.yml)

Gateway de LLMs em Go. API compatível com a da OpenAI na frente de múltiplos
provedores, com fallback, rate limiting, cache, contabilidade de custo e
observabilidade.

> **Estado:** Fase 1 de 5 concluída. O gateway já atende
> `POST /v1/chat/completions` sem streaming, com fallback, circuit breaker e cálculo
> de custo, usando o Ollama como provedor. Redis, Postgres, filas e os demais
> provedores da arquitetura abaixo são o alvo, construído fase a fase.

## O alvo

O cliente usa o SDK da OpenAI que já tem e troca só a `base_url`. Em vez de um
modelo físico, pede um modelo lógico (`"fast"`, `"smart"`), e a configuração
decide quais provedores atendem, em que ordem.

```
  cliente  ────►  API (Go)  ──┬──►  rate limit  (Redis)
 (SDK OpenAI)                 ├──►  cache       (Redis)
                              └──►  router + circuit breaker
                                         │
                            ┌────────────┼────────────┐
                            ▼            ▼            ▼
                         OpenAI      Anthropic      Ollama

  API ──► fila de uso    ──► worker ──► Postgres (agregação de custo)
  API ──► fila de batch  ──► worker ──► processa lote
```

- **Fallback com circuit breaker:** 429, 5xx ou timeout cai para o próximo
  provedor; provedor fora do ar não recebe tráfego até se recuperar.
- **Rate limit por requisição e por token:** em LLM, tokens por minuto é o
  limite que importa.
- **Cache exato** por hash do prompt e dos parâmetros.
- **Caminho crítico síncrono**, com streaming. Assíncrono só para eventos de
  uso e processamento em lote.

## Fases

| Fase | Escopo | Estado |
|---|---|---|
| 0 | Repo, Makefile, golangci-lint, CI, Dockerfile | concluída |
| 1 | Interface `Provider`, provedor falso, router com fallback e breaker, custo, config, adaptador do Ollama, endpoint sem streaming | concluída |
| 2 | Streaming SSE, adaptadores da OpenAI e da Anthropic, autenticação por chave de API, rate limit e cache no Redis, Postgres, testcontainers | — |
| 3 | Logs estruturados, métricas Prometheus, tracing OTel, Grafana | — |
| 4 | Fila de uso e worker, `/v1/batches` | — |
| 5 | Kubernetes com HPA pela fila | — |

## Rodando

O caminho mais curto precisa só do Docker:

```
make up    # gateway, Ollama com llama3.2, Postgres e Redis
make down  # derruba; os dados do Postgres e o modelo ficam nos volumes
```

Na primeira vez, `make up` baixa a imagem do Ollama e o modelo (alguns GB) e só
termina depois que o modelo estiver pronto. As portas publicadas (gateway em 8080,
Postgres em 5432, Redis em 6379) ficam presas em `127.0.0.1`. A config usada é
[`deploy/config.compose.json`](deploy/config.compose.json).

Todo pedido precisa de uma chave de API. Crie uma com o próprio binário (a chave só
aparece nesta saída; o banco guarda só o hash):

```
docker compose -f deploy/docker-compose.yml exec gateway /gateway keys create -name eu
docker compose -f deploy/docker-compose.yml exec gateway /gateway keys revoke -id 1
```

E use-a no cabeçalho `Authorization`:

```
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer llmux_..." -d '{
  "model": "fast",
  "messages": [{"role": "user", "content": "Olá!"}]
}'
```

Com um SDK da OpenAI, troque só a `base_url` para `http://localhost:8080/v1` e a
`api_key` para a chave criada. A resposta traz o provedor e o custo em picodólares nos
cabeçalhos `X-Llmux-Provider` e `X-Llmux-Cost-Picodollars`.

Para desenvolver sem o gateway em container, com Go 1.27, golangci-lint 2.x e o
[Ollama](https://ollama.com/download) na máquina:

```
ollama pull llama3.2
cp config.example.json config.json
docker compose -f deploy/docker-compose.yml up -d postgres
go run ./cmd/gateway keys create -name eu   # com LLMUX_DATABASE_URL, como em make run
make run
```

Outros comandos:

```
make              # lint, test e build: o mesmo que o CI roda
make docker-build # gera a imagem llmux-gateway:dev
```

Para rodar a imagem fora do compose, monte a config e aponte o banco. No Linux, com o
Ollama na própria máquina:

```
docker run --rm --network host \
  -v "$PWD/config.json:/config.json:ro" -e LLMUX_CONFIG=/config.json \
  -e LLMUX_DATABASE_URL='postgres://llmux:llmux@localhost:5432/llmux?sslmode=disable' \
  llmux-gateway:dev
```

Variáveis de ambiente:

| Variável | Default | Uso |
|---|---|---|
| `LLMUX_ADDR` | `:8080` | endereço do servidor |
| `LLMUX_CONFIG` | `config.json` | caminho do arquivo de configuração |
| `LLMUX_DATABASE_URL` | — | obrigatória; o Postgres das chaves de API (`make run` usa o do compose) |
| `OPENAI_API_KEY` | — | chave da OpenAI; obrigatória só se `openai` estiver em `providers` |
| `ANTHROPIC_API_KEY` | — | chave da Anthropic; obrigatória só se `anthropic` estiver em `providers` |

O formato do arquivo está em [`config.example.json`](config.example.json). O gateway
não sobe se uma rota não tiver preço ou provedor configurado.

## Limitações atuais

- **Chaves só pela linha de comando:** não há interface de administração (fora de
  escopo). Cada pedido consulta o banco, sem cache, para uma revogação valer na hora.
- **Stream interrompido não tem custo:** o uso só chega no fim do stream. No streaming,
  o custo vai como trailer HTTP, que muitos clientes ignoram.
- **OpenAI imitada pelo Ollama:** o adaptador da OpenAI funciona contra o `/v1` do Ollama,
  mas o Ollama ignora `max_completion_tokens`, então o limite de tokens não vale ali.
- **Anthropic sem `temperature`:** a versão `2026-10-21` da API recusa o campo com `400`,
  então a temperatura do cliente não é repassada. Sem `max_tokens`, o gateway manda 16000,
  porque a Anthropic exige o campo.
- **Só mensagens de texto:** `content` como lista de partes e o papel `tool` são
  recusados, então os recursos de ferramentas dos SDKs não funcionam.
- **Campos ignorados:** para os SDKs funcionarem, campos desconhecidos são aceitos e
  ignorados. Isso inclui `stop`, `top_p` e `seed`, que mudam a resposta.
- **O custo não é guardado:** ele sai só no cabeçalho (ou trailer) da resposta. A contabilidade
  entra com o Postgres e a fila de uso.
- **Compatibilidade:** testada com `curl` e com o SDK oficial em Go.

## Decisões

Cada decisão relevante tem um ADR em [`docs/decisions/`](docs/decisions/).

- [0001 — Imagem de runtime distroless](docs/decisions/0001-imagem-distroless.md)
- [0002 — Precificação pelo modelo da rota](docs/decisions/0002-precificacao-pelo-modelo-da-rota.md)
- [0003 — Fallback e circuit breaker](docs/decisions/0003-fallback-e-circuit-breaker.md)

## Custo

O Ollama local é o provedor padrão de desenvolvimento. Uma chave paga com limite
baixo serve só para provar que o gateway funciona com provedor real. Os testes
usam um provedor falso: o núcleo é testável sem rede, sem chave e sem custo.

## O que ficou de fora e por quê

- **Cache semântico.** Exige embeddings em toda requisição (custo) e pode
  devolver a resposta de uma pergunta parecida, mas diferente (correção). O
  cache exato não tem esse risco.
- **Multi-tenancy completo.** Isolamento real entre clientes (quotas, dados,
  chaves por tenant) é um produto por si só. Aqui a chave de API identifica o
  consumidor para rate limit e custo, e para por aí.
- **Interface de administração.** Configuração é arquivo versionado; métricas
  e dashboards vêm do Grafana.
- **Entrada de imagem e áudio.** Multiplica os formatos a normalizar entre
  provedores sem mudar nenhuma das decisões de arquitetura que o projeto quer
  demonstrar.
- **Fine-tuning.** É outro problema: treinamento, não roteamento.
