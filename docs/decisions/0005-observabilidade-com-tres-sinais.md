# ADR 0005 — Observabilidade com os três sinais ligados pelo contexto

- Status: aceito
- Data: 2026-11-14
- Fase: 3

## Contexto

Até a Fase 2, o gateway funcionava, mas não se explicava. Um fallback acontecia sem
deixar rastro, um pedido lento não dizia onde gastou o tempo, e não havia como saber o
custo do dia ou se um provedor estava caindo, a não ser lendo o código.

Um sistema em produção precisa responder três tipos de pergunta, e cada um pede um
sinal diferente:

| Pergunta | Sinal |
|---|---|
| O que aconteceu **neste** pedido? | Log |
| Como está o sistema **agora**, somando todos os pedidos? | Métrica |
| **Onde** este pedido gastou o tempo? | Trace |

## Decisão

**Os três sinais, ligados por dois ids que viajam no `context.Context`.** O contexto já
passa por todas as camadas, do handler ao adaptador. Por isso, nenhuma função recebe um
logger ou um id como parâmetro, e nenhuma assinatura mudou para a observabilidade
entrar.

**Logs: `log/slog` em JSON, uma linha de access log por pedido.**
- Um `slog.Handler` próprio lê o `request_id` e o `trace_id` do contexto e os põe em
  toda linha.
- O `X-Request-Id` do cliente é aceito só se tiver até 64 caracteres de um conjunto
  seguro. Uma quebra de linha nele forjaria uma linha de log falsa.
- O access log tem status, duração, chave, modelo, provedor e cache, mas nunca o
  conteúdo da conversa nem a chave de API.

**Métricas: `prometheus/client_golang`, numa porta própria (2112).**
- O `/metrics` mostra custos e ids de chave, e não pode ficar ao lado da API pública.
- Os pedidos são medidos no access log, que é o único lugar que conhece status,
  provedor, modelo e duração no fim do pedido.
- O estado dos circuitos é lido na hora da coleta, por um método do router que devolve
  um mapa. O router não conhece o Prometheus.
- Os baldes de latência vão até 120 s. Os padrão param em 10 s e cegariam o p95 de um
  LLM.

**Traces: OpenTelemetry, exportados por OTLP/HTTP.**
- Um span por pedido, por dependência (Postgres, Redis) e por tentativa no router, com
  a chamada HTTP ao provedor dentro da tentativa.
- O tracing liga só com `OTEL_EXPORTER_OTLP_ENDPOINT`. Sem coletor, a API do
  OpenTelemetry fica no modo padrão, que não faz nada.
- O `traceparent` do cliente é respeitado, mas o nosso **não sai** para os provedores:
  eles são terceiros, e o id do nosso trace não serve para eles e revela ids internos.

**O `trace_id` em toda linha de log é a ponte**: de uma linha de erro se chega ao
cronograma do pedido, e do cronograma às linhas.

## Regras de cardinalidade

Cada combinação de valores de rótulos é uma série guardada pelo Prometheus. Um valor
que vem de fora e vira rótulo é uma porta para encher o Prometheus.

| Valor | Regra | Por quê |
|---|---|---|
| Modelo pedido pelo cliente | Só os da config viram rótulo; o resto vira `unknown` | Um cliente com chave válida criaria uma série por nome inventado. Achado na prova com o compose |
| Id da chave de API | Só no contador de custo, nunca num histograma | As chaves só crescem quando o operador cria uma; num histograma, cada chave custaria 14 séries |
| `request_id` | Nunca vira rótulo | Uma série por pedido |
| Nome do span do pedido | Fixo por rota; rota desconhecida tem nome próprio | As ferramentas de trace agrupam por nome, e uma URL inventada criaria um nome novo |

## Alternativas descartadas

**`zap` ou `zerolog` em vez de `slog`.** São mais rápidos, mas a velocidade de log não é
o gargalo de um gateway que espera segundos por um LLM. O `slog` é da biblioteca padrão
desde o Go 1.21.

**Passar o logger como parâmetro.** É explícito, mas mudaria a assinatura de cada
função que loga, e o router precisaria de um logger só para registrar o fallback.

**Métricas pelo SDK do OpenTelemetry**, com exportador para o Prometheus. Unificaria com
o tracing, mas traria mais dependências e uma camada a mais para um formato que o
Prometheus já fala direto.

**`/metrics` na porta da API, atrás da autenticação.** O Prometheus precisaria de uma
chave de API, e um erro na autenticação esconderia as métricas justamente quando elas
mais importam.

**Exportador de traces por gRPC.** É o mesmo protocolo, mas traria o gRPC inteiro como
dependência.

**Propagar o `traceparent` para os provedores**, que é o padrão do `otelhttp`.
Descartado pelo motivo acima: terceiros não usam o id e ganham uma informação interna.

**Montar o painel do Grafana na interface.** O painel ficaria fora do Git, e uma máquina
nova subiria sem ele. Fontes de dados e painel são arquivos em `deploy/grafana/`.

## Consequências

- **Estado global nos testes.** O logger padrão do `slog` e o provedor global do
  OpenTelemetry são trocados pelos testes que os leem, que por isso não rodam em
  paralelo.
- **O tracer é buscado na hora do uso**, e não guardado numa variável de pacote. Numa
  variável, ele ficava preso ao primeiro provedor global: um teste rodado duas vezes
  registrou zero spans na segunda.
- **Os logs não são agregados.** Ficam na saída do container (`docker logs`); um
  coletor como o Loki ficou fora da fase.
- **O Jaeger do compose guarda os traces na memória**, e eles somem num restart.
- **O custo por chave nas métricas tem limite.** Serve para centenas ou poucos milhares
  de chaves; acima disso, o custo por chave pertence ao Postgres, com a fila de uso da
  Fase 4.
- **O `rate()` precisa de tráfego contínuo.** Uma série nova não tem um valor anterior,
  e um painel com pouco tráfego mostra zero ou "sem dados".
