# ADR 0001 — Imagem de runtime distroless

- Status: aceito
- Data: 2026-11-01
- Fase: 0

## Contexto

O gateway é um binário Go estático que faz chamadas HTTPS a provedores de LLM
(OpenAI, Anthropic) e vai rodar em Kubernetes na Fase 5. A imagem de runtime
precisa de três coisas: certificados raiz para validar TLS, nada que não seja
usado (superfície de ataque e tamanho) e execução sem root.

## Decisão

Build multi-stage: compilação em `golang:1.27.3` com `CGO_ENABLED=0`, runtime em
`gcr.io/distroless/static-debian12:nonroot`. Imagem final com 8,4 MB.

## Alternativas descartadas

**scratch.** Imagem vazia: não tem `/etc/ssl/certs`. A primeira chamada HTTPS a um
provedor falharia com `x509: certificate signed by unknown authority`, e isso é o
caminho crítico inteiro do gateway. Também não tem tzdata nem `/etc/passwd`, então
não há usuário nonroot nomeado.
Contra-argumento conhecido: dá para copiar os certificados do estágio de build
para o scratch. Funciona, mas passamos a manter à mão o que a distroless já
entrega e atualiza (certificados, tzdata, usuário). O ganho seria de ~2 MB.

**alpine.** Tem shell e `apk`, o que facilita depurar, mas cada pacote é
superfície de ataque e entra no scanner de vulnerabilidades. Com binário
estático não precisamos de nada do que ela traz.

**golang como runtime.** Centenas de MB com compilador e ferramentas que só
servem para o build.

## Consequências

**Sem shell para depurar.** `docker exec -it ... sh` e `kubectl exec ... sh`
não funcionam: não há `sh`, `curl`, `ls` nem `cat` no container. Esse é o custo
real da decisão.

Por que aceitamos:

- **A depuração deve acontecer de fora do container.** A Fase 3 traz logs
  estruturados com `request_id`, métricas Prometheus e tracing OTel. Num gateway, as
  perguntas de incidente são "qual provedor está falhando", "por que caiu no
  fallback" e "onde está a latência", e elas são respondidas por métricas e traces,
  não por um shell dentro de um pod. Um shell mostra um pod num instante, e a
  telemetria mostra todos os pods ao longo do tempo.
- **Ainda há saída quando for preciso olhar dentro.** No Kubernetes,
  `kubectl debug` injeta um container efêmero com ferramentas no pod. Localmente,
  existe a variante `:debug-nonroot` da distroless, que inclui o busybox, e pode
  ser usada temporariamente sem mudar a imagem de produção.

**Outros pontos:**

- O binário precisa ser estático (`CGO_ENABLED=0`), porque a imagem não tem libc.
  Qualquer dependência futura que exija cgo reabre esta decisão.
- A imagem base está fixada por tag, não por digest. Fixar por digest é mais
  reprodutível e fica no backlog.
