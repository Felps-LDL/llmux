package router_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
)

func TestChatFazFallback(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "limite excedido", err: provider.ErrRateLimited},
		{name: "indisponível", err: provider.ErrUnavailable},
		{name: "tempo esgotado", err: provider.ErrTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Fail(tt.err))
			b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
			r := router.New(map[string][]router.Route{
				"fast": {
					{Provider: a, Model: "modelo-a"},
					{Provider: b, Model: "modelo-b"},
				},
			}, time.Second)

			res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})

			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			if res.Provider != "b" {
				t.Errorf("Provider = %q, want %q", res.Provider, "b")
			}
			if got := len(a.Calls()); got != 1 {
				t.Errorf("chamadas em A = %d, want 1", got)
			}
		})
	}
}

func TestChatNaoFazFallbackComErroSemClasse(t *testing.T) {
	errSemClasse := errors.New("status 401")
	a := fake.New("a", fake.Fail(errSemClasse))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second)

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})

	if !errors.Is(err, errSemClasse) {
		t.Errorf("erro = %v, want %v", err, errSemClasse)
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}
