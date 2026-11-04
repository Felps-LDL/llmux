package usage_test

import (
	"errors"
	"testing"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/usage"
)

func TestCost(t *testing.T) {
	table := usage.PriceTable{
		{Provider: "pago", Model: "m"}:  {InputPerMillion: 150_000, OutputPerMillion: 600_000},
		{Provider: "local", Model: "m"}: {},
	}

	tests := []struct {
		name     string
		provider string
		usage    provider.Usage
		want     usage.Cost
	}{
		{name: "só entrada", provider: "pago", usage: provider.Usage{PromptTokens: 1000}, want: 150_000_000},
		// LACUNA 1: só saída, com 1000 tokens de CompletionTokens
		{name: "só saída", provider: "pago", usage: provider.Usage{CompletionTokens: 1000}, want: 600_000_000},
		// LACUNA 2: entrada e saída juntas, com 1000 e 500 tokens
		{name: "entrada e saída", provider: "pago", usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 500}, want: 450_000_000},
		// LACUNA 3: provedor "local", com tokens, e custo 0
		{name: "provedor local", provider: "local", usage: provider.Usage{PromptTokens: 1000, CompletionTokens: 500}, want: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := table.Cost(tt.provider, "m", tt.usage)
			// LACUNA 4: err tem que ser nil (t.Fatalf)
			if err != nil {
				t.Fatalf("Cost() returned error: %v", err)
			}
			// LACUNA 5: got tem que ser tt.want, com a mensagem "Cost = %d, want %d"
			if got != tt.want {
				t.Fatalf("Cost = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestCostComPrecoDesconhecido(t *testing.T) {
	// LACUNA 6: chame Cost numa tabela vazia e verifique usage.ErrUnknownPrice com errors.Is
	_, err := usage.PriceTable{}.Cost("pago", "m", provider.Usage{PromptTokens: 1000})
	if !errors.Is(err, usage.ErrUnknownPrice) {
		t.Fatalf("Cost() returned error: %v", err)
	}
}
