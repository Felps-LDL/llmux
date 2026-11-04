// Package usage calcula o custo das chamadas aos provedores.
package usage

import (
	"errors"
	"fmt"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// ErrUnknownPrice indica um modelo sem preço cadastrado. É um erro, e não custo
// zero, porque um custo zerado em silêncio só apareceria na fatura do provedor.
var ErrUnknownPrice = errors.New("usage: preço desconhecido")

// Cost é um valor em picodólares (10⁻¹² USD). É inteiro porque somar float64
// acumula erro de arredondamento; ver ADR 0002.
type Cost int64

// Price é o preço de um modelo, em microdólares por milhão de tokens. Nessa
// unidade, tokens × preço já resulta em picodólares, sem divisão nem arredondamento.
type Price struct {
	InputPerMillion  int64
	OutputPerMillion int64
}

// ModelKey identifica um modelo em um provedor. O provedor faz parte da chave
// porque o mesmo nome de modelo pode ter preços diferentes em cada um.
type ModelKey struct {
	Provider string
	Model    string
}

// PriceTable guarda os preços por modelo. Pode ser lida por pedidos concorrentes,
// desde que não mude depois de criada.
type PriceTable map[ModelKey]Price

// Cost calcula o custo de uma chamada. O model é o nome físico da rota, não o
// reportado pelo provedor; ver ADR 0002.
func (t PriceTable) Cost(providerName, model string, u provider.Usage) (Cost, error) {
	price, ok := t[ModelKey{Provider: providerName, Model: model}]
	if !ok {
		return 0, fmt.Errorf("%w: provedor %q, modelo %q", ErrUnknownPrice, providerName, model)
	}

	input := int64(u.PromptTokens) * price.InputPerMillion
	output := int64(u.CompletionTokens) * price.OutputPerMillion
	return Cost(input + output), nil
}
