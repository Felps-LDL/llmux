package fake_test

import (
	"context"
	"errors"
	"io"
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

// Verifica o contrato que o router e a API vão seguir ao ler um stream: os
// pedaços chegam na ordem, o uso só no último, e o fim é io.EOF. Um falso que
// devolvesse nil no fim faria um laço de leitura nunca terminar.
func TestChatStreamEntregaPedacosETerminaComEOF(t *testing.T) {
	usage := provider.Usage{PromptTokens: 3, CompletionTokens: 2}
	p := fake.New("a", fake.StreamReply(
		provider.StreamChunk{Content: "o"},
		provider.StreamChunk{Content: "lá", FinishReason: "stop", Usage: &usage},
	))

	stream, err := p.ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}

	var got []provider.StreamChunk
	for {
		chunk, err := stream.Recv()
		// errors.Is, e não ==, porque um provedor real pode embrulhar o io.EOF.
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: erro inesperado: %v", err)
		}
		got = append(got, chunk)
	}

	if len(got) != 2 || got[0].Content != "o" || got[1].Content != "lá" {
		t.Fatalf("pedaços = %+v, want \"o\" e \"lá\" nessa ordem", got)
	}
	if got[0].Usage != nil {
		t.Errorf("primeiro pedaço: Usage = %+v, want nil", got[0].Usage)
	}
	if got[1].Usage == nil || *got[1].Usage != usage {
		t.Errorf("último pedaço: Usage = %+v, want %+v", got[1].Usage, usage)
	}
}

// Verifica a falha que mais importa no streaming: ChatStream funciona, o cliente
// já recebeu um pedaço, e só depois a conexão cai. É o caso em que o router não
// pode mais fazer fallback, então o falso precisa reproduzi-lo com precisão.
func TestChatStreamFalhaDepoisDosPedacos(t *testing.T) {
	p := fake.New("a", fake.StreamFailAfter(provider.ErrUnavailable,
		provider.StreamChunk{Content: "o"},
	))

	// Se o erro viesse já aqui, seria uma falha antes do stream, que ainda
	// permite fallback: outro cenário, que fake.Fail cobre.
	stream, err := p.ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()

	chunk, err := stream.Recv()
	if err != nil || chunk.Content != "o" {
		t.Fatalf("primeiro Recv = (%+v, %v), want o pedaço \"o\" sem erro", chunk, err)
	}

	_, err = stream.Recv()
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("segundo Recv: erro = %v, want %v", err, provider.ErrUnavailable)
	}
}

// Verifica que o cancelamento chega no meio do stream, não só no começo. É por
// isso que Recv checa o contexto de novo: o cliente que fecha a aba depois do
// primeiro pedaço só é percebido na leitura seguinte.
func TestChatStreamRespeitaCancelamentoNoMeio(t *testing.T) {
	p := fake.New("a", fake.StreamReply(
		provider.StreamChunk{Content: "o"},
		provider.StreamChunk{Content: "lá"},
	))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	stream, err := p.ChatStream(ctx, provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	if _, err := stream.Recv(); err != nil {
		t.Fatalf("primeiro Recv: erro inesperado: %v", err)
	}

	cancel()

	if _, err := stream.Recv(); !errors.Is(err, context.Canceled) {
		t.Errorf("Recv após cancelamento: erro = %v, want %v", err, context.Canceled)
	}

	// Close continua obrigatório depois de um erro: é ele que solta a conexão.
	_ = stream.Close()
	if got := p.Closes(); got != 1 {
		t.Errorf("Closes() = %d, want 1", got)
	}
}
