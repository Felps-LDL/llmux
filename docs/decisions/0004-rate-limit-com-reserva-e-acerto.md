# ADR 0004 — Rate limit com reserva e acerto

- Status: aceito
- Data: 2026-11-09
- Fase: 2

## Contexto

Cada chave de API tem dois limites por minuto: de pedidos e de tokens. O limite de
tokens é o que importa num gateway de LLM, porque é por token que os provedores cobram.
Um limite só de pedidos deixaria passar um pedido de 100.000 tokens com o mesmo peso de
um de 10.

O problema é o momento. O limite precisa ser aplicado **antes** de chamar o provedor,
mas os tokens de saída só são conhecidos **depois**, quando a resposta chega. E o
gateway roda em várias réplicas (Fase 5), que precisam enxergar o mesmo saldo.

## Decisão

**Token bucket no Redis, com reserva antes e acerto depois.**

1. **Reserva.** Antes de chamar o router, o gateway estima o custo do pedido e o
   desconta do balde:
   - **Entrada:** cerca de 4 bytes por token, mais 4 tokens de formatação por mensagem.
   - **Saída:** o `max_tokens` do pedido, ou 1.024 se ele não vier.

   Se o balde não tem saldo, a resposta é `429` com `Retry-After`.
2. **Acerto.** Quando o uso real chega, a diferença volta ao balde, ou sai dele se a
   estimativa ficou baixa. Se o provedor falha sem gerar nada, a reserva volta inteira.
3. **Dívida.** O saldo pode ficar negativo. Se o uso real passou da reserva, a resposta
   já foi dada; os próximos pedidos da chave esperam a dívida ser paga pela recarga.

**Os dois baldes são decididos num único script Lua.** O Redis roda um script inteiro
sem nenhum outro comando no meio. Por isso, ler o saldo, decidir e descontar viram uma
operação só, mesmo com réplicas concorrentes. O script também garante que uma recusa
por tokens não gaste a ficha de pedido.

**O relógio é o do Redis**, e não o do gateway. Réplicas com relógios diferentes
calculariam recargas diferentes para o mesmo balde.

**Falha aberto.** Com o Redis fora do ar, os pedidos passam sem limite, e o log avisa. A
autenticação faz o oposto: falha fechada, porque é ela que autoriza. Com o Redis fora,
o pior caso é um cliente **já autenticado** usar mais que a cota por alguns minutos. Com
o Postgres fora e a porta aberta, o pior caso seria **qualquer um** usar o sistema, e
derrubar o banco viraria um jeito de entrar.

**Limites por chave, com padrão na config.** A coluna `NULL` significa "use o padrão": ao
mudar a config, o limite efetivo de todas as chaves sem limite próprio muda junto, sem
migrar dados.

## Regras que fecham os casos de borda

| Situação | O que acontece | Por quê |
|---|---|---|
| Pedido sem `max_tokens` | Reserva 1.024 de saída | Com reserva zero, pedidos simultâneos passariam todos e só depois virariam dívida |
| Reserva maior que o limite por minuto | É reduzida ao limite | Senão o pedido nunca caberia, nem com o balde cheio |
| Cliente desiste no meio | O acerto usa um contexto que não é cancelado | Senão a reserva ficaria presa no balde |
| Stream interrompido | A reserva fica inteira | Sem o uso real, o mais seguro é cobrar o reservado |
| Acerto do cache | A reserva de tokens volta inteira; a ficha de pedido não | O provedor não foi chamado, mas o gateway trabalhou |
| Balde cheio | A chave expira no Redis | Um balde cheio é igual a um que não existe, e chaves ociosas não ocupam memória |

## Alternativas descartadas

**Contar os tokens só depois da resposta.** Sem reserva, o limite só é verificado
depois de o dinheiro ser gasto: dez pedidos simultâneos passariam todos, por maiores
que fossem.

**Janela fixa por minuto** (um contador zerado a cada minuto). É mais simples, mas
permite o dobro do limite na virada da janela, com uma rajada no último segundo de um
minuto e outra no primeiro do seguinte. O token bucket recarrega de forma contínua.

**Ler e gravar o saldo pelo Go, sem script.** Entre a leitura e a gravação, outra réplica
leria o mesmo saldo, e as duas passariam. Uma transação com `WATCH` resolveria, mas
falha e repete sob disputa; o script nunca falha por disputa.

**Tokenizador de verdade (tiktoken) na estimativa.** Cada provedor tem o seu, e seria uma
dependência pesada para um número que o acerto corrige logo depois.

**Falhar fechado no Redis.** Uma queda do Redis derrubaria o produto inteiro para
proteger contra um excesso de cota de clientes conhecidos.

## Consequências

- **A estimativa erra.** Em português, os acentos ocupam 2 bytes e a entrada sai
  superestimada, o que só reserva a mais. Um pedido que gera mais que o estimado vira
  dívida para a chave.
- **Um pedido sem `max_tokens` que gera mais de 1.024 tokens** passa pelo limite e só
  pesa depois, como dívida.
- **O limite é aproximado durante uma queda do Redis**, e inexistente enquanto ela dura.
- **Estado no Redis não sobrevive a um restart**, de propósito: o compose roda o Redis
  sem persistência. Um restart zera os baldes, o que só devolve cota aos clientes.
- **Testar o tempo é limitado.** O relógio é o do Redis, então os testes de recarga
  esperam tempo real, com taxas altas para a espera ser curta.
