# llmux

Gateway de LLMs em Go, grau de produção. API compatível com a da OpenAI na frente de
múltiplos provedores, com fallback, rate limiting, cache, contabilidade de custo e
observabilidade completa.

## Por que este projeto existe

Projeto de portfólio de Felipe Lages, desenvolvedor Go procurando vaga de back-end e
plataforma de IA. O objetivo **não é o software funcionar**. O objetivo é Felipe
conseguir defender cada decisão numa entrevista técnica.

Consequência direta e inegociável: **se você escrever código que o Felipe não entende,
o projeto falhou**, mesmo que passe em todos os testes.

Lacunas que o projeto precisa fechar, em ordem de prioridade:
testes automatizados, CI/CD, observabilidade, Kubernetes, mensageria.

## Estado atual

Fase: **2** (em andamento, branch `fase-2`; a seguir, adaptadores da OpenAI e da Anthropic)
Última coisa concluída: componente 1, streaming SSE (`ChatStream` no falso, no Ollama, no
router com fallback até o primeiro pedaço, e na API com custo em trailer). Provado com o
Ollama real e com o SDK oficial em Go. A pedido do Felipe, Claude escreve o código; ele
revisa e responde perguntas de cenário, que ficaram pendentes para o fim da fase.

> Mantenha estas duas linhas atualizadas ao fim de cada sessão.

## Decisões já tomadas

Não reabrir sem motivo forte. Cada uma já tem ou terá um ADR em `docs/decisions/`.

1. **API compatível com a OpenAI** (`POST /v1/chat/completions`). O cliente troca só a
   `base_url`. Adoção sem refatoração.
2. **Modelo lógico, não físico.** O cliente pede `"fast"` ou `"smart"`; a config mapeia
   para uma lista ordenada de provedores. Trocar de modelo é mudar config, não deploy.
3. **Fallback com circuit breaker** no router. 429, 5xx ou timeout cai para o próximo
   provedor; provedor morto não é martelado.
4. **Rate limit por requisição e por token**, token bucket no Redis. Token por minuto é
   o limite que importa em LLM; requisição por minuto sozinho é ingênuo.
5. **Cache exato** por hash de prompt mais parâmetros. Cache semântico está **fora de
   escopo** por custo e risco de correção.
6. **Assíncrono em dois lugares apenas**: eventos de uso e processamento em lote. Nunca
   no caminho crítico da chamada, que é síncrono e com streaming.
7. **`Provider` é uma interface**, com implementação falsa. Todo o núcleo é testável sem
   rede, sem chave de API e sem custo.

## Arquitetura

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

```
llmux/
├── cmd/{gateway,worker}/
├── internal/
│   ├── provider/      ← interface + openai/ anthropic/ ollama/ fake/
│   ├── router/        ← política, fallback, circuit breaker
│   ├── ratelimit/
│   ├── cache/
│   ├── usage/         ← tokens, custo, evento
│   ├── api/           ← handlers, middleware
│   └── telemetry/     ← métricas, tracing, logger
├── deploy/{docker-compose.yml,k8s/}
├── docs/{architecture.md,decisions/,backlog.md}
└── .github/workflows/ci.yml
```

Interface central, tudo é escrito contra ela:

```go
type Provider interface {
    Name() string
    Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
    ChatStream(ctx context.Context, req ChatRequest) (StreamReader, error)
}
```

`ChatStream` só entrou na Fase 2, junto com o streaming. Antes disso, toda
implementação teria um stub devolvendo "não implementado", e o tipo afirmaria que
todo provedor faz streaming quando nenhum faz.

## Fases

| Fase | Escopo |
|---|---|
| **0** | Repo, Makefile, golangci-lint, CI verde, Dockerfile, README com o alvo |
| **1** | Interface `Provider`, provedor falso, router com fallback e breaker, contagem de token e custo, carga de config, adaptador do Ollama, endpoint sem streaming, cobertura alta |
| **2** | Streaming SSE (`ChatStream` em `Provider`), adaptadores da OpenAI e da Anthropic, autenticação por chave de API, rate limit no Redis, cache exato, Postgres e migrations (chaves de API), testcontainers, docker-compose |
| **3** | slog com request_id, métricas Prometheus, tracing OTel, dashboard Grafana, prints no README |
| **4** | Fila de uso e worker, tabelas de uso no Postgres, `/v1/batches`, métrica de profundidade de fila |
| **5** | Kubernetes: deployments separados, HPA pela fila, kind documentado |

**Não implemente nada de uma fase futura.** Se você sugerir algo fora da fase atual e
fizer sentido, anote em `docs/backlog.md` e siga.

Métricas da Fase 3, para referência:

```
llm_requests_total{provider,model,status}
llm_request_duration_seconds{provider,model}
llm_time_to_first_token_seconds{provider}
llm_tokens_total{provider,direction}
llm_cost_usd_total{provider,model,api_key}
llm_cache_hits_total
llm_circuit_state{provider}
llm_queue_depth{queue}
```

## Como trabalhamos

### O suporte diminui

- **Fases 0 e 1** — você escreve, com a explicação vindo antes do código. O Felipe revisa
  linha por linha e pergunta o que não entendeu.
- **Fases 2 e 3** — você propõe a abordagem e as armadilhas; o Felipe escreve; você revisa.
  Transição gradual, por decisão do Felipe: nos primeiros componentes da Fase 2, você
  também passa a estrutura com lacunas, cada vez maiores, até ele escrever sozinho. Ao
  fim de cada componente, ele explica com as próprias palavras o que escreveu.
- **Fases 4 e 5** — o Felipe escreve; você só revisa e questiona. Não escreva a solução
  antes de ele tentar.

### Testes, especificamente

O Felipe está aprendendo testes agora. O modo é exemplo primeiro, autonomia depois:

- Para cada **tipo novo** de teste (tabela, com provedor falso, concorrência com `-race`,
  integração com testcontainers), você escreve os **dois primeiros** como exemplo
  comentado, explicando **o que está sendo verificado e por que aquele caso importa**.
- A partir do terceiro, **o Felipe escreve e você revisa**, apontando os casos que
  faltaram e por quê.
- Nunca entregue uma suíte pronta. Um teste que o Felipe não sabe explicar não existe
  para o propósito deste projeto.

### Regras que valem em qualquer fase

- Um componente por vez. Nunca despeje vários arquivos numa resposta só.
- Explique a decisão **antes** do código, em no máximo cinco linhas.
- Toda dependência nova exige justificativa e a alternativa descartada. Nenhuma
  biblioteca escolhida em silêncio.
- Comentário de código só para explicar **por quê**. Nunca o quê.
- Se o Felipe estiver errado, diga. Concordar por educação não serve para nada aqui.
- Se você não souber, diga que não sabe.

### Ao fim de cada fase

1. Escreva ou atualize o ADR da decisão principal da fase.
2. Atualize o README e a seção "Estado atual" deste arquivo.
3. Faça ao Felipe **cinco perguntas de entrevista** sobre o que foi construído, do tipo
   que um entrevistador sênior faria. Se ele não souber responder alguma, revisem esse
   ponto antes de avançar de fase.

## Convenções

- Go idiomático, `gofmt`, `golangci-lint` limpo.
- Erros embrulhados com `fmt.Errorf("contexto: %w", err)`.
- `context.Context` sempre como primeiro parâmetro.
- Sem framework web. `net/http` com `chi` para roteamento.
- Nada de `any` sem justificativa escrita.
- Commits convencionais. **O Felipe escreve as mensagens de commit**, você no máximo
  revisa.
- Uma branch por fase, PR para a `main`, CI obrigatório para merge.

## Fora de escopo

Cache semântico, multi-tenancy completo, interface de administração, entrada de imagem
e áudio, fine-tuning. Tudo isso vai para a seção "o que ficou de fora e por quê" do
README, que vale mais do que implementar mal.

## Custo

Ollama local é o provedor padrão de desenvolvimento. Uma chave paga com limite baixo
serve só para provar que funciona com provedor real. O provedor falso cobre os testes.
Isso é parte do discurso do projeto, não uma limitação a esconder.
