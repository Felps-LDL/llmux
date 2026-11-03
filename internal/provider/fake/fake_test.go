package fake_test

import (
	"context"
	"errors"
	"testing"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
)

// Verifica que o roteiro é seguido na ordem. Os testes de fallback do router vão
// depender disso ("o primeiro falha, o segundo responde"): se a ordem não fosse
// garantida, eles poderiam passar sem exercitar o fallback.
func TestChatSegueOrdemDoRoteiro(t *testing.T) {
	p := fake.New("a",
		fake.Fail(provider.ErrRateLimited),
		fake.Reply(provider.ChatResponse{Content: "olá"}),
	)

	// errors.Is, e não ==, porque é assim que o router vai perguntar. O nil na
	// resposta também importa: erro e resposta juntos seriam um estado ambíguo.
	resp, err := p.Chat(t.Context(), provider.ChatRequest{})
	if !errors.Is(err, provider.ErrRateLimited) {
		t.Fatalf("primeira chamada: erro = %v, want %v", err, provider.ErrRateLimited)
	}
	if resp != nil {
		t.Fatalf("primeira chamada: resposta = %+v, want nil", resp)
	}

	resp, err = p.Chat(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segunda chamada: erro inesperado: %v", err)
	}
	if resp.Content != "olá" {
		t.Errorf("segunda chamada: Content = %q, want %q", resp.Content, "olá")
	}
}

// Verifica que, com o cliente já desconectado, o falso devolve context.Canceled
// e não entrega a resposta do roteiro. Se entregasse, um router que ignora o
// cancelamento pareceria correto nos testes.
func TestChatRespeitaContextoCancelado(t *testing.T) {
	p := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, err := p.Chat(ctx, provider.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erro = %v, want %v", err, context.Canceled)
	}
	if resp != nil {
		t.Errorf("resposta = %+v, want nil", resp)
	}

	// A chamada conta mesmo cancelada: é assim que um teste do router vai provar
	// que ele nem tentou chamar o provedor depois do cancelamento.
	if got := len(p.Calls()); got != 1 {
		t.Errorf("Calls() = %d chamadas, want 1", got)
	}
}

func TestChatFalhaComRoteiroEsgotado(t *testing.T) {
	p := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))

	_, err := p.Chat(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("primeira chamada: erro inesperado: %v", err)
	}

	resp, err := p.Chat(t.Context(), provider.ChatRequest{})
	if !errors.Is(err, fake.ErrScriptExhausted) {
		t.Fatalf("erro = %v, want %v", err, fake.ErrScriptExhausted)
	}
	for _, class := range []error{provider.ErrRateLimited, provider.ErrUnavailable, provider.ErrTimeout} {
		if errors.Is(err, class) {
			t.Errorf("erro %q não deveria ser da classe %q", err, class)
		}
	}
	if resp != nil {
		t.Errorf("resposta = %+v, want nil", resp)
	}
}
