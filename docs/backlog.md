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

## CI

- **`actionlint` no CI.** Hoje um erro no `ci.yml` só aparece quando o
  workflow roda no GitHub. Origem: Fase 0, o workflow foi validado só como
  YAML antes do primeiro push.
