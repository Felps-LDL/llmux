package router_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
)

// O limite alto mantém o circuito fechado: estes testes são sobre o fallback, e os
// sobre o breaker definem a própria configuração.
var testBreaker = router.BreakerConfig{Threshold: 100, Cooldown: time.Minute}

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
			}, time.Second, testBreaker)

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
	}, time.Second, testBreaker)

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
	}, time.Second, testBreaker)

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{Model: "fast"})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if res.Model != "modelo-b" {
		t.Errorf("Model = %q, want %q", res.Model, "modelo-b")
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
	}, time.Second, testBreaker)

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

// providerFunc permite que um teste defina o que acontece durante a chamada, o
// que o roteiro do falso não consegue expressar.
type providerFunc func(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error)

func (f providerFunc) Name() string { return "func" }

func (f providerFunc) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	return f(ctx, req)
}

// Os testes que usam providerFunc são de Chat; um ChatStream chamado aqui é um
// teste errado, e o erro sem classe faz o router parar em vez de mascará-lo.
func (f providerFunc) ChatStream(context.Context, provider.ChatRequest) (provider.StreamReader, error) {
	return nil, errors.New("providerFunc: ChatStream não usado neste teste")
}

// Verifica o caso que o teste de contexto cancelado não alcança: o cliente
// desiste enquanto o A está em andamento. Importa porque a próxima tentativa
// poderia custar dinheiro por uma resposta que ninguém vai ler.
func TestChatNaoFazFallbackSeClienteDesisteDuranteTentativa(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	// O A falha com uma classe que aciona fallback. Com um erro sem classe, o
	// router pararia de qualquer forma, e o teste não provaria nada sobre o
	// contexto.
	a := providerFunc(func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		cancel()
		return nil, provider.ErrUnavailable
	})
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, testBreaker)

	_, err := r.Chat(ctx, "fast", provider.ChatRequest{})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("erro = %v, want %v", err, context.Canceled)
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}

// Verifica o motivo de o timeout ser por tentativa: um provedor travado não pode
// impedir o fallback. O sucesso do B também prova que o prazo é próprio de cada
// tentativa, porque um prazo compartilhado já teria expirado quando ele fosse
// chamado.
func TestChatFazFallbackQuandoProvedorTrava(t *testing.T) {
	a := providerFunc(func(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
		select {
		case <-ctx.Done():
			return nil, provider.ErrTimeout
		// Sem este caso, um router sem timeout travaria o teste até o limite do
		// go test, em vez de reprovar com uma mensagem clara.
		case <-time.After(5 * time.Second):
			return nil, errors.New("o router não deu prazo à tentativa")
		}
	})
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, 10*time.Millisecond, testBreaker)

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if res.Provider != "b" {
		t.Errorf("Provider = %q, want %q", res.Provider, "b")
	}
}

func TestChatDevolveErroComModeloDesconhecido(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
		},
	}, time.Second, testBreaker)

	_, err := r.Chat(t.Context(), "modelo-desconhecido", provider.ChatRequest{})
	if !errors.Is(err, router.ErrUnknownModel) {
		t.Errorf("erro = %v, want %v", err, router.ErrUnknownModel)
	}
	if got := len(a.Calls()); got != 0 {
		t.Errorf("chamadas em A = %d, want 0", got)
	}
}

func TestChatNaoChamaSegundoProvedorQuandoPrimeiroResponde(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "resposta de a"}))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "resposta de b"}))

	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, testBreaker)

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want %q", res.Provider, "a")
	}
	if res.Response == nil {
		t.Fatal("Response = nil, want resposta de a")
	}
	if res.Response.Content != "resposta de a" {
		t.Errorf("Content = %q, want %q", res.Response.Content, "resposta de a")
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}

func TestChatGuardaFalhaDeCadaProvedorQuandoTodosFalham(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrTimeout))
	b := fake.New("b", fake.Fail(provider.ErrUnavailable))

	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, testBreaker)

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})

	if !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("erro = %v, want %v", err, provider.ErrTimeout)
	}
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("erro = %v, want %v", err, provider.ErrUnavailable)
	}
}

func TestChatPulaProvedorComCircuitoAberto(t *testing.T) {
	tests := []struct {
		name string
		err  error
	}{
		{name: "indisponível", err: provider.ErrUnavailable},
		{name: "tempo esgotado", err: provider.ErrTimeout},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Fail(tt.err))
			b := fake.New("b",
				fake.Reply(provider.ChatResponse{Content: "primeira"}),
				fake.Reply(provider.ChatResponse{Content: "segunda"}),
			)
			r := router.New(map[string][]router.Route{
				"fast": {
					{Provider: a, Model: "modelo-a"},
					{Provider: b, Model: "modelo-b"},
				},
			}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

			_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
			if err != nil {
				t.Fatalf("primeiro pedido: erro inesperado: %v", err)
			}

			res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
			if err != nil {
				t.Fatalf("segundo pedido: erro inesperado: %v", err)
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

func TestChatNaoAbreCircuitoComLimiteExcedido(t *testing.T) {
	a := fake.New("a",
		fake.Fail(provider.ErrRateLimited),
		fake.Reply(provider.ChatResponse{Content: "resposta de a"}),
	)
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "resposta de b"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("primeiro pedido: erro inesperado: %v", err)
	}

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want %q", res.Provider, "a")
	}

	if got := len(a.Calls()); got != 2 {
		t.Errorf("chamadas em A = %d, want 2", got)
	}
}

func TestChatNaoAbreCircuitoComErroSemClasse(t *testing.T) {
	errSemClasse := errors.New("status 401")
	a := fake.New("a",
		fake.Fail(errSemClasse),
		fake.Reply(provider.ChatResponse{Content: "resposta de a"}),
	)
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "resposta de b"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if !errors.Is(err, errSemClasse) {
		t.Fatalf("primeiro pedido: erro = %v, want %v", err, errSemClasse)
	}

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want %q", res.Provider, "a")
	}

	if got := len(a.Calls()); got != 2 {
		t.Errorf("chamadas em A = %d, want 2", got)
	}

	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}

func TestChatDevolveErroQuandoTodosOsCircuitosEstaoAbertos(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	b := fake.New("b", fake.Fail(provider.ErrUnavailable))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("primeiro pedido: erro = %v, want %v", err, provider.ErrUnavailable)
	}

	_, err = r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if !errors.Is(err, router.ErrCircuitOpen) {
		t.Fatalf("segundo pedido: erro = %v, want %v", err, router.ErrCircuitOpen)
	}

	if got := len(a.Calls()); got != 1 {
		t.Errorf("chamadas em A = %d, want 1", got)
	}

	if got := len(b.Calls()); got != 1 {
		t.Errorf("chamadas em B = %d, want 1", got)
	}
}

func TestChatNaoAbreCircuitoQuandoClienteDesiste(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	calls := 0
	a := providerFunc(func(context.Context, provider.ChatRequest) (*provider.ChatResponse, error) {
		calls++
		if calls == 1 {
			cancel()
			return nil, provider.ErrTimeout
		}
		return &provider.ChatResponse{Content: "resposta de a"}, nil
	})
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "resposta de b"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	_, err := r.Chat(ctx, "fast", provider.ChatRequest{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("primeiro pedido: erro = %v, want %v", err, context.Canceled)
	}

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	if res.Provider != "func" {
		t.Errorf("Provider = %q, want %q", res.Provider, "func")
	}
	if got := len(b.Calls()); got != 0 {
		t.Errorf("chamadas em B = %d, want 0", got)
	}
}

func TestChatDivideCircuitoEntreModelosDoMesmoProvedor(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	r := router.New(map[string][]router.Route{
		"fast":  {{Provider: a, Model: "modelo-rapido"}},
		"smart": {{Provider: a, Model: "modelo-esperto"}},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})

	_, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if !errors.Is(err, provider.ErrUnavailable) {
		t.Fatalf("primeiro pedido: erro = %v, want %v", err, provider.ErrUnavailable)
	}
	_, err = r.Chat(t.Context(), "smart", provider.ChatRequest{})
	if !errors.Is(err, router.ErrCircuitOpen) {
		t.Fatalf("segundo pedido: erro = %v, want %v", err, router.ErrCircuitOpen)
	}
	if got := len(a.Calls()); got != 1 {
		t.Errorf("chamadas em A = %d, want 1", got)
	}
}

func TestChatVoltaAUsarProvedorRecuperado(t *testing.T) {
	a := fake.New("a",
		fake.Fail(provider.ErrUnavailable),
		fake.Reply(provider.ChatResponse{Content: "sonda"}),
		fake.Reply(provider.ChatResponse{Content: "normal"}),
	)
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "resposta de b"}))
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: 0})

	res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("primeiro pedido: erro inesperado: %v", err)
	}
	if res.Provider != "b" {
		t.Errorf("Provider = %q, want %q", res.Provider, "b")
	}
	res, err = r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("segundo pedido: erro inesperado: %v", err)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want %q", res.Provider, "a")
	}
	res, err = r.Chat(t.Context(), "fast", provider.ChatRequest{})
	if err != nil {
		t.Fatalf("terceiro pedido: erro inesperado: %v", err)
	}
	if res.Provider != "a" {
		t.Errorf("Provider = %q, want %q", res.Provider, "a")
	}
	if got := len(b.Calls()); got != 1 {
		t.Errorf("chamadas em B = %d, want 1", got)
	}
}

// Verifica o caminho inteiro sob carga: pedidos simultâneos dividem o mesmo router
// e os mesmos breakers. Cada um precisa fazer o fallback sem perder resposta, e o
// -race acusa qualquer estado compartilhado sem trava nesse caminho.
func TestChatAtendePedidosConcorrentes(t *testing.T) {
	// Abaixo do limite de testBreaker: o circuito não pode abrir no meio do teste,
	// senão a contagem de chamadas em A dependeria da ordem das goroutines.
	const requests = 50

	aSteps := make([]fake.Step, requests)
	bSteps := make([]fake.Step, requests)
	for i := range requests {
		aSteps[i] = fake.Fail(provider.ErrUnavailable)
		bSteps[i] = fake.Reply(provider.ChatResponse{Content: "olá"})
	}
	a := fake.New("a", aSteps...)
	b := fake.New("b", bSteps...)
	r := router.New(map[string][]router.Route{
		"fast": {
			{Provider: a, Model: "modelo-a"},
			{Provider: b, Model: "modelo-b"},
		},
	}, time.Second, testBreaker)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			res, err := r.Chat(t.Context(), "fast", provider.ChatRequest{})
			// Errorf, e não Fatalf: Fatalf só pode ser chamado na goroutine do
			// teste, porque ele encerra a goroutine que o chamou.
			if err != nil {
				t.Errorf("erro inesperado: %v", err)
				return
			}
			if res.Provider != "b" {
				t.Errorf("Provider = %q, want %q", res.Provider, "b")
			}
		})
	}
	close(start)
	wg.Wait()

	if got := len(a.Calls()); got != requests {
		t.Errorf("chamadas em A = %d, want %d", got, requests)
	}
	if got := len(b.Calls()); got != requests {
		t.Errorf("chamadas em B = %d, want %d", got, requests)
	}
}
