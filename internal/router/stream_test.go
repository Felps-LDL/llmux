package router_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
)

// Verifica a regra central do streaming: até o primeiro pedaço, o router ainda
// pode trocar de provedor. Os dois casos são diferentes para o provedor: um
// recusa o pedido, o outro aceita (status 200) e falha na primeira leitura. Sem a
// leitura antecipada no router, o segundo caso chegaria ao cliente como erro.
func TestChatStreamFazFallbackAntesDoPrimeiroPedaco(t *testing.T) {
	tests := []struct {
		name string
		step fake.Step
		// Só a falha na leitura chega a abrir um stream, e ele precisa ser fechado.
		wantCloses int
	}{
		{name: "falha ao abrir", step: fake.Fail(provider.ErrUnavailable), wantCloses: 0},
		{name: "falha na primeira leitura", step: fake.StreamFailAfter(provider.ErrUnavailable), wantCloses: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", tt.step)
			b := fake.New("b", fake.StreamReply(provider.StreamChunk{Content: "olá"}))
			r := router.New(map[string][]router.Route{
				"fast": {
					{Provider: a, Model: "modelo-a"},
					{Provider: b, Model: "modelo-b"},
				},
			}, time.Second, testBreaker)

			res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
			if err != nil {
				t.Fatalf("erro inesperado: %v", err)
			}
			defer func() { _ = res.Stream.Close() }()

			if res.Provider != "b" {
				t.Errorf("Provider = %q, want %q", res.Provider, "b")
			}
			chunk, err := res.Stream.Recv()
			if err != nil || chunk.Content != "olá" {
				t.Errorf("Recv = (%+v, %v), want o pedaço de b", chunk, err)
			}
			// Um stream aberto e abandonado no fallback seria uma conexão vazada a
			// cada falha do provedor.
			if got := a.Closes(); got != tt.wantCloses {
				t.Errorf("Closes em A = %d, want %d", got, tt.wantCloses)
			}
		})
	}
}

// Verifica o outro lado da regra: depois do primeiro pedaço, nada de fallback. O
// cliente já recebeu "o" de A; se o router trocasse para B, a resposta final
// misturaria o começo de um modelo com uma resposta inteira de outro.
func TestChatStreamNaoFazFallbackDepoisDoPrimeiroPedaco(t *testing.T) {
	a := fake.New("a", fake.StreamFailAfter(provider.ErrUnavailable, provider.StreamChunk{Content: "o"}))
	b := fake.New("b", fake.StreamReply(provider.StreamChunk{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, testBreaker)

	res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	defer func() { _ = res.Stream.Close() }()

	if chunk, err := res.Stream.Recv(); err != nil || chunk.Content != "o" {
		t.Fatalf("primeiro Recv = (%+v, %v), want o pedaço de A", chunk, err)
	}
	if _, err := res.Stream.Recv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("segundo Recv: erro = %v, want %v", err, provider.ErrUnavailable)
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}

// streamFunc é um provedor de streaming definido pelo teste, para os casos que
// dependem do tempo e do contexto da tentativa, que o roteiro do falso não expressa.
type streamFunc func(ctx context.Context) (provider.StreamReader, error)

func (f streamFunc) Name() string { return "func" }

func (f streamFunc) Chat(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
	return nil, errors.New("streamFunc: Chat não usado neste teste")
}

func (f streamFunc) ChatStream(ctx context.Context, _ provider.ChatRequest) (provider.StreamReader, error) {
	return f(ctx)
}

// slowStream espera antes de cada pedaço e respeita o contexto, como um provedor
// real: se o contexto da tentativa for cancelado, a leitura falha.
type slowStream struct {
	ctx        context.Context
	firstDelay time.Duration
	delay      time.Duration
	chunks     []string
	sent       int
}

func (s *slowStream) Recv() (provider.StreamChunk, error) {
	if s.sent == len(s.chunks) {
		return provider.StreamChunk{}, io.EOF
	}
	wait := s.delay
	if s.sent == 0 {
		wait = s.firstDelay
	}
	select {
	case <-time.After(wait):
	case <-s.ctx.Done():
		return provider.StreamChunk{}, fmt.Errorf("slowStream: %w", s.ctx.Err())
	}
	s.sent++
	return provider.StreamChunk{Content: s.chunks[s.sent-1]}, nil
}

func (s *slowStream) Close() error { return nil }

// Verifica que o prazo da tentativa não vale para o stream inteiro. A resposta
// leva mais que o prazo no total, mas o primeiro pedaço chega dentro dele. Com um
// WithTimeout comum, ou com o defer cancel do Chat, o segundo pedaço falharia.
func TestChatStreamNaoCortaRespostaLongaDepoisDoPrimeiroPedaco(t *testing.T) {
	a := streamFunc(func(ctx context.Context) (provider.StreamReader, error) {
		return &slowStream{ctx: ctx, delay: 30 * time.Millisecond, chunks: []string{"o", "lá"}}, nil
	})
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-a"}},
	}, 10*time.Millisecond, testBreaker)

	res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	defer func() { _ = res.Stream.Close() }()

	var content string
	for {
		chunk, err := res.Stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: erro inesperado: %v", err)
		}
		content += chunk.Content
	}
	if content != "olá" {
		t.Errorf("conteúdo = %q, want %q", content, "olá")
	}
}

// Verifica o prazo até o primeiro pedaço e a classificação dele. O provedor A vê
// o cancelamento como context.Canceled, igual a uma desistência do cliente; só a
// causa mostra que foi o prazo. Sem isso, A travado não faria fallback e nunca
// abriria o circuito: o segundo pedido o chamaria de novo.
func TestChatStreamTrataDemoraNoPrimeiroPedacoComoTimeout(t *testing.T) {
	calls := 0
	a := streamFunc(func(ctx context.Context) (provider.StreamReader, error) {
		calls++
		return &slowStream{ctx: ctx, firstDelay: 5 * time.Second, chunks: []string{"tarde"}}, nil
	})
	b := fake.New("b",
		fake.StreamReply(provider.StreamChunk{Content: "primeira"}),
		fake.StreamReply(provider.StreamChunk{Content: "segunda"}),
	)
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, 10*time.Millisecond, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	for i := range 2 {
		res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
		if err != nil {
			t.Fatalf("pedido %d: erro inesperado: %v", i+1, err)
		}
		_ = res.Stream.Close()
		if res.Provider != "b" {
			t.Errorf("pedido %d: Provider = %q, want %q", i+1, res.Provider, "b")
		}
	}
	if calls != 1 {
		t.Errorf("chamadas em A = %d, want 1 (o circuito deveria ter aberto)", calls)
	}
}

// Verifica que uma falha no meio do stream conta para o breaker, mesmo sem
// fallback. Um provedor que sempre cai no meio da resposta precisa sair de
// circulação como qualquer outro.
func TestChatStreamContaFalhaNoMeioDoStream(t *testing.T) {
	a := fake.New("a", fake.StreamFailAfter(provider.ErrUnavailable, provider.StreamChunk{Content: "o"}))
	b := fake.New("b", fake.StreamReply(provider.StreamChunk{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("primeiro pedido: erro inesperado: %v", err)
	}
	for {
		if _, err := res.Stream.Recv(); err != nil {
			break
		}
	}
	_ = res.Stream.Close()

	res, err = r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	_ = res.Stream.Close()
	if res.Provider != "b" {
		t.Errorf("segundo pedido: Provider = %q, want %q", res.Provider, "b")
	}
}

// Verifica o caso oposto: o cliente fecha o stream no meio. Isso é desistência,
// não falha do provedor; se contasse, clientes impacientes abririam o circuito de
// um provedor saudável.
func TestChatStreamNaoContaDesistenciaDoCliente(t *testing.T) {
	a := fake.New("a",
		fake.StreamReply(provider.StreamChunk{Content: "o"}, provider.StreamChunk{Content: "lá"}),
		fake.StreamReply(provider.StreamChunk{Content: "de novo"}),
	)
	b := fake.New("b", fake.StreamReply(provider.StreamChunk{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	res, err := r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("primeiro pedido: erro inesperado: %v", err)
	}
	if _, err := res.Stream.Recv(); err != nil {
		t.Fatalf("Recv: erro inesperado: %v", err)
	}
	_ = res.Stream.Close()

	res, err = r.ChatStream(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	_ = res.Stream.Close()
	if res.Provider != "a" {
		t.Errorf("segundo pedido: Provider = %q, want %q", res.Provider, "a")
	}
	if got := a.Closes(); got != 2 {
		t.Errorf("Closes em A = %d, want 2", got)
	}
}
