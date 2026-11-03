package router_test

import (
	"context"
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

func TestChatEnviaNomeFisicoDeCadaRota(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second)

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{Model: "fast"})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	callsA := a.Calls()
	if len(callsA) != 1 {
		t.Fatalf("chamadas em A = %d, want 1", len(callsA))
	}
	if callsA[0].Model != "modelo-a" {
		t.Errorf("Model em A = %q, want %q", callsA[0].Model, "modelo-a")
	}

	callsB := b.Calls()
	if len(callsB) != 1 {
		t.Fatalf("chamadas em B = %d, want 1", len(callsB))
	}
	if callsB[0].Model != "modelo-b" {
		t.Errorf("Model em B = %q, want %q", callsB[0].Model, "modelo-b")
	}
}

func TestChatNaoChamaProvedorComContextoCancelado(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := r.Chat(ctx, "fast", provider.ChatRequest{})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("erro = %v, want %v", err, context.Canceled)
	}
	if got := len(a.Calls()); got != 0 {
		t.Errorf("chamadas em A = %d, want 0", got)
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}
