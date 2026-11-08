package api_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/ratelimit"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/store"
)

type reserveCall struct {
	keyID  int64
	limits ratelimit.Limits
	tokens int
}

type settleCall struct {
	reserved, used int
	// O estado do contexto no momento do acerto: ele não pode estar cancelado.
	ctxErr error
}

// fakeLimiter devolve uma decisão fixa e registra as chamadas.
type fakeLimiter struct {
	decision ratelimit.Decision
	err      error

	mu       sync.Mutex
	reserves []reserveCall
	settles  []settleCall
}

func (f *fakeLimiter) Reserve(_ context.Context, keyID int64, limits ratelimit.Limits, tokens int) (ratelimit.Decision, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reserves = append(f.reserves, reserveCall{keyID, limits, tokens})
	d := f.decision
	d.Reserved = tokens
	return d, f.err
}

func (f *fakeLimiter) Settle(ctx context.Context, _ int64, _ ratelimit.Limits, reserved, used int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.settles = append(f.settles, settleCall{reserved, used, ctx.Err()})
	return nil
}

var defaultLimits = ratelimit.Limits{RequestsPerMinute: 60, TokensPerMinute: 100000}

func newLimitedHandler(p provider.Provider, key store.APIKey, l api.Limiter) http.Handler {
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: p, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	return api.New(api.Options{
		Router:        r,
		Keys:          keys{testKey: key},
		Limiter:       l,
		DefaultLimits: defaultLimits,
	}).Handler()
}

func post(ctx context.Context, t *testing.T, h http.Handler, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// Verifica a resposta de limite excedido, que é o que o SDK do cliente lê para
// decidir quando tentar de novo. O Retry-After arredonda para cima (1,5 s vira 2):
// arredondar para baixo faria o cliente voltar antes da hora e ser recusado de novo.
// E um pedido recusado não pode chegar ao provedor nem ser acertado depois.
func TestRateLimitRecusaComRetryAfter(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	l := &fakeLimiter{decision: ratelimit.Decision{RetryAfter: 1500 * time.Millisecond, RemainingRequests: 0, RemainingTokens: 42}}

	rec := post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 7}, l),
		`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429 (corpo: %s)", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After = %q, want %q", got, "2")
	}
	if !strings.Contains(rec.Body.String(), `"rate_limit_error"`) {
		t.Errorf("corpo = %s, want o tipo rate_limit_error", rec.Body)
	}
	if got := rec.Header().Get("X-Ratelimit-Remaining-Tokens"); got != "42" {
		t.Errorf("X-Ratelimit-Remaining-Tokens = %q, want %q", got, "42")
	}
	if n := len(a.Calls()); n != 0 {
		t.Errorf("chamadas ao provedor = %d, want 0", n)
	}
	if n := len(l.settles); n != 0 {
		t.Errorf("acertos = %d, want 0 (nada foi reservado)", n)
	}
}

// Verifica o ciclo reservar e acertar, que é o que faz o limite de tokens
// funcionar sem saber a saída de antemão. A reserva é a estimativa ("oi" são 2
// bytes: 0 tokens de conteúdo + 4 de formatação, mais os 100 de max_tokens), e o
// acerto recebe o uso real que o provedor informou.
func TestRateLimitReservaEstimativaEAcertaComUsoReal(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá", Usage: provider.Usage{PromptTokens: 30, CompletionTokens: 50}}))
	l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

	rec := post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 7}, l),
		`{"model": "fast", "max_tokens": 100, "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corpo: %s)", rec.Code, rec.Body)
	}
	if len(l.reserves) != 1 || l.reserves[0].tokens != 104 || l.reserves[0].keyID != 7 {
		t.Fatalf("reservas = %+v, want uma de 104 tokens para a chave 7", l.reserves)
	}
	if len(l.settles) != 1 || l.settles[0] != (settleCall{reserved: 104, used: 80}) {
		t.Errorf("acertos = %+v, want um com 104 reservados e 80 usados", l.settles)
	}
}

// Sem max_tokens, a saída ainda precisa ser reservada. Com reserva zero, dez
// pedidos simultâneos da mesma chave passariam todos pelo balde e só depois, no
// acerto, virariam dívida: o limite não seguraria nada.
func TestRateLimitReservaSaidaPadraoSemMaxTokens(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

	post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 1}, l),
		`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

	// 4 de formatação da mensagem, mais a saída padrão de 1024.
	if len(l.reserves) != 1 || l.reserves[0].tokens != 1028 {
		t.Errorf("reservas = %+v, want uma de 1028 tokens", l.reserves)
	}
}

func TestRateLimitUsaLimitesDaChaveOuOPadrao(t *testing.T) {
	tests := []struct {
		name string
		key  store.Limits
		want ratelimit.Limits
	}{
		{name: "sem limites próprios", key: store.Limits{}, want: defaultLimits},
		{name: "os dois próprios", key: store.Limits{RequestsPerMinute: 5, TokensPerMinute: 500}, want: ratelimit.Limits{RequestsPerMinute: 5, TokensPerMinute: 500}},
		{name: "só tokens próprio", key: store.Limits{TokensPerMinute: 500}, want: ratelimit.Limits{RequestsPerMinute: 60, TokensPerMinute: 500}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
			l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

			post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 1, Limits: tt.key}, l),
				`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

			if len(l.reserves) != 1 || l.reserves[0].limits != tt.want {
				t.Errorf("reservas = %+v, want limites %+v", l.reserves, tt.want)
			}
		})
	}
}

func TestRateLimitDevolveReservaQuandoProvedorFalha(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

	rec := post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 1}, l),
		`{"model": "fast", "max_tokens": 100, "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if len(l.settles) != 1 || l.settles[0].used != 0 {
		t.Errorf("acertos = %+v, want um com used 0 (devolve tudo)", l.settles)
	}
}

// Fail open: o Redis fora do ar não pode derrubar o produto. O pedido passa, e sem
// reserva não há o que acertar.
func TestRateLimitDeixaPassarQuandoRedisFalha(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	l := &fakeLimiter{err: errors.New("redis: conexão recusada")}

	rec := post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 1}, l),
		`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corpo: %s)", rec.Code, rec.Body)
	}
	if n := len(a.Calls()); n != 1 {
		t.Errorf("chamadas ao provedor = %d, want 1", n)
	}
	if n := len(l.settles); n != 0 {
		t.Errorf("acertos = %d, want 0", n)
	}
}

func TestRateLimitNoStreamingAcertaNoFimOuMantemReserva(t *testing.T) {
	tests := []struct {
		name        string
		step        fake.Step
		wantSettles []settleCall
	}{
		{
			name: "stream completo acerta com o uso",
			step: fake.StreamReply(
				provider.StreamChunk{Content: "olá"},
				provider.StreamChunk{FinishReason: "stop", Usage: &provider.Usage{PromptTokens: 10, CompletionTokens: 20}},
			),
			wantSettles: []settleCall{{reserved: 104, used: 30}},
		},
		{
			name:        "falha antes do primeiro pedaço devolve tudo",
			step:        fake.Fail(provider.ErrUnavailable),
			wantSettles: []settleCall{{reserved: 104, used: 0}},
		},
		// Sem o uso real, o mais seguro é cobrar o que foi reservado.
		{
			name:        "stream interrompido mantém a reserva",
			step:        fake.StreamFailAfter(provider.ErrUnavailable, provider.StreamChunk{Content: "o"}),
			wantSettles: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", tt.step)
			l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

			post(t.Context(), t, newLimitedHandler(a, store.APIKey{ID: 1}, l),
				`{"model": "fast", "stream": true, "max_tokens": 100, "messages": [{"role": "user", "content": "oi"}]}`)

			if len(l.settles) != len(tt.wantSettles) || (len(tt.wantSettles) == 1 && l.settles[0] != tt.wantSettles[0]) {
				t.Errorf("acertos = %+v, want %+v", l.settles, tt.wantSettles)
			}
		})
	}
}

// cancelingProvider simula o cliente desistindo durante a chamada ao provedor.
type cancelingProvider struct{ cancel context.CancelFunc }

func (p cancelingProvider) Name() string { return "a" }

func (p cancelingProvider) Chat(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	p.cancel()
	return nil, ctx.Err()
}

func (p cancelingProvider) ChatStream(context.Context, provider.ChatRequest) (provider.StreamReader, error) {
	return nil, errors.New("não usado")
}

// O cliente desistiu no meio da chamada, e o contexto do pedido foi cancelado. O
// acerto ainda precisa chegar ao Redis com um contexto vivo: com o do pedido, ele
// falharia, e a reserva ficaria presa no balde até a recarga.
func TestRateLimitAcertaMesmoComClienteQueDesistiu(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}

	post(ctx, t, newLimitedHandler(cancelingProvider{cancel}, store.APIKey{ID: 1}, l),
		`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

	if len(l.settles) != 1 {
		t.Fatalf("acertos = %+v, want 1", l.settles)
	}
	if l.settles[0].ctxErr != nil {
		t.Errorf("contexto do acerto: %v, want vivo", l.settles[0].ctxErr)
	}
}
