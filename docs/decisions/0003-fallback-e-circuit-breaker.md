# ADR 0003 — Fallback e circuit breaker

- Status: aceito
- Data: 2026-11-05
- Fase: 1

## Contexto

O gateway fica na frente de vários provedores de LLM, e cada um pode cair, limitar
pedidos ou travar. O cliente pede um modelo lógico (`"fast"`), que corresponde a uma
lista ordenada de rotas. Precisamos decidir quando tentar o próximo provedor e quando
parar de chamar um provedor com problema.

As duas decisões têm custo: tentar de novo sem critério cobra por pedidos inválidos, e
abrir o circuito sem critério tira de uso um provedor saudável para todos os clientes.

## Decisão

**O adaptador classifica, e o router decide.** Cada adaptador traduz as falhas em três
erros sentinela. O router só conhece essas classes, nunca status HTTP.

| Falha | Fallback? | Conta para abrir o circuito? |
|---|---|---|
| `ErrUnavailable` (5xx, falha de conexão, resposta malformada) | sim | sim |
| `ErrTimeout` | sim | sim |
| `ErrRateLimited` (429) | sim | **não** |
| Erro sem classe (400, 401, 404) | **não** | **não** |
| Cancelamento ou prazo do cliente | **não** | **não** |

Os motivos das exceções:

- **429 não abre o circuito.** O provedor está vivo e limitando este cliente, e o limite
  costuma passar logo. Abrir o circuito o tiraria de uso durante todo o cooldown.
- **Erro sem classe não faz fallback.** Um pedido inválido seria enviado aos demais
  provedores, e o que o aceitasse cobraria por ele. Em um 401, o fallback esconderia a
  chave errada. Ele também não abre o circuito: um único cliente tiraria o provedor de
  uso para todos.
- **A desistência do cliente não diz nada sobre o provedor.** Sem essa regra, clientes
  com prazo curto abririam o circuito de um provedor saudável. Por isso o resultado é
  avaliado com o contexto do pedido, e não com o da tentativa.

**Timeout por tentativa, não só por pedido.** Sem ele, um provedor travado consumiria
todo o prazo do cliente, e o fallback por timeout nunca aconteceria.

**O circuito é por provedor e conta falhas consecutivas.** Tem três estados:

- **Fechado:** passa tudo. Um sucesso zera a contagem, e atingir o limite abre o circuito.
- **Aberto:** recusa tudo até o cooldown passar. O router registra `ErrCircuitOpen` e
  segue para a próxima rota.
- **Meio-aberto:** libera uma única sonda. Sucesso fecha o circuito, e falha o reabre.
  Se a sonda terminar sem veredito, como um cancelamento, ela só libera a vaga
  (`release`) para a próxima tentativa.

## Alternativas descartadas

**`sony/gobreaker` em vez de código próprio.** É uma biblioteca madura, mas são poucas
linhas de máquina de estados, e o terceiro resultado (`release`) é central aqui. Este é um
projeto de portfólio, e a máquina de estados precisa ser defensável linha por linha. Em
um time, a biblioteca seria uma escolha razoável.

**Circuito por rota.** Uma queda atinge o provedor inteiro. Com um circuito por rota, um
segundo modelo lógico continuaria chamando um provedor morto.

**Janela de taxa de falhas em vez de falhas consecutivas.** Detecta melhor um provedor
que falha de forma intermitente, mas exige guardar uma janela de tempo. Começamos pelo
mais simples, e a escolha pode ser revista com as métricas da Fase 3.

**Status HTTP no router.** Nem toda falha tem status, como um timeout ou uma conexão
recusada, e o router teria que conhecer os detalhes de cada provedor.

## Consequências

- **Falhas intermitentes podem não abrir o circuito.** Um único sucesso zera a contagem,
  então um provedor que alterna sucesso e falha continua recebendo tráfego.
- **Provedor lento conta como falha.** Um `attempt_timeout` curto demais para o modelo
  pode abrir o circuito de um provedor que estava só lento. O valor precisa ser ajustado
  por config.
- **O estado de cada circuito é local à instância.** Com várias réplicas (Fase 5), cada
  uma aprende sozinha que um provedor caiu, e o provedor recebe mais tentativas do que
  o limite sugere.
- **Respostas atrasadas têm efeito limitado.** Uma resposta de antes de o circuito abrir é
  ignorada. Durante o meio-aberto, ela ainda conta como se fosse a sonda, porque
  distinguir os dois casos exigiria identificar cada chamada.
- **Latência no fallback:** no pior caso, um pedido espera `attempt_timeout` por rota,
  limitado pelo prazo do cliente.
