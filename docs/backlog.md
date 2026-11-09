# Backlog

Ideias que surgiram fora da fase em que faziam sentido. Cada item diz por que
existe; nenhum entra sem passar pela fase em andamento.

## Segurança da cadeia de suprimentos

- **Fixar as actions do CI por SHA de commit, não por tag.** Uma tag como `@v7`
  pode ser movida pelo dono da action para outro commit; um SHA não. Se a
  action for comprometida, a tag passa a apontar para o código malicioso.
  Origem: Fase 0, `ci.yml`.
- **Fixar a imagem base por digest.** Mesmo motivo: `static-debian12:nonroot`
  é uma tag móvel, então dois builds do mesmo commit podem gerar imagens
  diferentes. Origem: Fase 0, ADR 0001.

## API

- **Tratar os campos da OpenAI que hoje são ignorados.** `stop`, `top_p` e `seed`, entre
  outros, mudam a resposta, e o cliente não recebe nenhum aviso. Foi assim que o
  `max_completion_tokens` passou despercebido até o teste com um SDK real. Cada campo
  precisa de uma decisão: repassar ou recusar. Origem: Fase 1, teste com o SDK.
- **Cobrar streams interrompidos.** O uso só chega no último pedaço; se o stream cai
  antes, o gateway não sabe quanto o provedor cobrou e não registra custo. Opções: estimar
  pelos pedaços entregues ou contar os tokens no gateway. Importa na Fase 4, quando o custo
  vira registro por chave de API. Origem: Fase 2, streaming.

## Cache

- **Cache no streaming.** Hoje só pedidos sem streaming usam o cache. Um acerto
  precisaria ser devolvido como uma sequência de eventos SSE, e um stream só pode ser
  gravado depois de terminar. Origem: Fase 2, cache exato.
- **Pedidos iguais simultâneos.** Dois pedidos idênticos ao mesmo tempo dão dois miss
  e chamam o provedor duas vezes. Um singleflight resolveria dentro de uma réplica;
  entre réplicas, seria preciso uma trava no Redis. Origem: Fase 2, cache exato.

## CI

- **`actionlint` no CI.** Hoje um erro no `ci.yml` só aparece quando o
  workflow roda no GitHub. Origem: Fase 0, o workflow foi validado só como
  YAML antes do primeiro push.
