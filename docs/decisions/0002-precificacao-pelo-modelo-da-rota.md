# ADR 0002 — Precificação pelo modelo da rota

- Status: aceito
- Data: 2026-11-03
- Fase: 1

## Contexto

Cada chamada a um provedor tem custo, calculado a partir dos tokens que o provedor
reporta e de uma tabela de preços. A chamada envolve dois nomes de modelo: o nome
físico que pedimos, definido na rota (`gpt-4o-mini`), e o nome que o provedor
reporta ter usado, que pode ser mais específico (`gpt-4o-mini-2024-07-18`).

A escolha do nome tem consequência em cobrança. Precificar pelo nome reportado exige
uma entrada na tabela para cada versão datada, e uma versão nova do provedor não
encontraria preço. Se a ausência virar custo zero, a contabilidade fica errada sem
nenhum erro.

## Decisão

1. **O custo é calculado pelo nome físico da rota.** O conjunto de nomes está na
   nossa config, então é conhecido antes de qualquer chamada.
2. **Toda rota precisa ter preço, ou o gateway não sobe.** A verificação acontece na
   carga da config. Preço zero é permitido, mas precisa estar escrito, como no
   Ollama local: "de graça" e "esquecemos de cadastrar" são coisas diferentes.
3. **Entrada e saída têm preços separados**, por milhão de tokens.
4. **O custo é um inteiro em picodólares (10⁻¹² USD).** Somar `float64` acumula erro
   de arredondamento, inaceitável em contabilidade. Um `int64` nessa unidade
   representa preços fracionários e comporta até cerca de 9 milhões de dólares por
   valor somado.
5. **Os tokens são os reportados pelo provedor.** O gateway não estima contagem.

## Alternativas descartadas

**Nome reportado pelo provedor, com correspondência exata.** É o que o provedor diz
ter usado, mas exige acompanhar cada versão datada. Uma versão nova zeraria o custo
em silêncio ou derrubaria pedidos que estavam funcionando.

**Nome reportado, com correspondência por prefixo.** Reduz a manutenção, mas
`gpt-4o` também é prefixo de `gpt-4o-mini`, então uma regra errada aplicaria o preço
de outro modelo sem nenhum erro.

**`float64` em dólares.** É simples e direto, mas o erro de arredondamento cresce a
cada soma.

## Consequências

- **O custo é uma estimativa quando o provedor muda de versão.** Se um nome sem data
  passar a apontar para uma versão com outro preço, calculamos com o preço antigo até
  a tabela ser atualizada. A fatura do provedor continua sendo a fonte da verdade.
- **O nome reportado continua disponível** em `ChatResponse.Model`, para investigar
  divergências com a fatura.
- **Adicionar uma rota exige cadastrar o preço.** É um atrito de propósito.
- **Conversões:** a métrica `llm_cost_usd_total`, da Fase 3, precisará converter o
  inteiro para dólares. Essa conversão é só para exibição; a soma confiável é a
  feita em inteiros.
