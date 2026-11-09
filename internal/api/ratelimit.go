package api

import (
	"context"
	"log"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/ratelimit"
	"github.com/Felps-LDL/llmux/internal/store"
)

const (
	// Saída estimada quando o pedido não tem max_tokens. Com zero, dez pedidos
	// simultâneos da mesma chave passariam todos e só depois virariam dívida. O
	// acerto corrige o valor com o uso real.
	defaultOutputReservation = 1024
	// Quanto o acerto pode demorar depois de o pedido terminar; ver settle.
	settleTimeout = 5 * time.Second
)

// Limiter é o que a API usa do rate limit, pelo mesmo motivo de KeyLookup: os
// testes usam um falso, sem Redis.
type Limiter interface {
	Reserve(ctx context.Context, keyID int64, limits ratelimit.Limits, tokens int) (ratelimit.Decision, error)
	Settle(ctx context.Context, keyID int64, limits ratelimit.Limits, reserved, used int) error
}

// limitsFor aplica os limites próprios da chave e completa o que faltar com o
// padrão da config; zero na chave significa "use o padrão".
func (s *Server) limitsFor(key store.APIKey) ratelimit.Limits {
	l := s.defaultLimits
	if key.Limits.RequestsPerMinute > 0 {
		l.RequestsPerMinute = key.Limits.RequestsPerMinute
	}
	if key.Limits.TokensPerMinute > 0 {
		l.TokensPerMinute = key.Limits.TokensPerMinute
	}
	return l
}

// estimateTokens estima a entrada em ~4 bytes por token, a regra prática dos
// tokenizadores de inglês. Em português, os acentos ocupam 2 bytes e a estimativa
// sai alta, o que só reserva a mais: o acerto devolve a diferença.
func estimateTokens(req provider.ChatRequest) int {
	input := 0
	for _, m := range req.Messages {
		// ~4 tokens de formatação por mensagem (papel e separadores).
		input += len(m.Content)/4 + 4
	}
	output := req.MaxTokens
	if output == 0 {
		output = defaultOutputReservation
	}
	return input + output
}

// reservation é uma reserva feita, a acertar quando o uso real for conhecido.
// A zero value é "nada reservado", e settle não faz nada.
type reservation struct {
	limiter  Limiter
	keyID    int64
	limits   ratelimit.Limits
	reserved int
}

// reserve reserva o pedido e os tokens estimados. Devolve false se o limite foi
// atingido, e nesse caso já respondeu 429.
func (s *Server) reserve(w http.ResponseWriter, r *http.Request, req provider.ChatRequest) (reservation, bool) {
	if s.limiter == nil {
		return reservation{}, true
	}
	key := keyFrom(r.Context())
	limits := s.limitsFor(key)

	d, err := s.limiter.Reserve(r.Context(), key.ID, limits, estimateTokens(req))
	if err != nil {
		// Fail open: o rate limit protege o sistema, mas não autoriza ninguém (a
		// autenticação já passou). Uma queda do Redis não pode derrubar o produto.
		log.Printf("api: rate limit indisponível, pedido liberado: %v", err)
		return reservation{}, true
	}

	// Os nomes da OpenAI, que os SDKs e clientes já sabem ler.
	h := w.Header()
	h.Set("X-Ratelimit-Limit-Requests", strconv.Itoa(limits.RequestsPerMinute))
	h.Set("X-Ratelimit-Remaining-Requests", strconv.Itoa(d.RemainingRequests))
	h.Set("X-Ratelimit-Limit-Tokens", strconv.Itoa(limits.TokensPerMinute))
	h.Set("X-Ratelimit-Remaining-Tokens", strconv.Itoa(d.RemainingTokens))

	if !d.Allowed {
		// Retry-After é em segundos inteiros; arredondar para baixo faria o cliente
		// voltar um pouco antes da hora e ser recusado de novo.
		h.Set("Retry-After", strconv.Itoa(int(math.Ceil(d.RetryAfter.Seconds()))))
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "limite de pedidos ou tokens da chave excedido")
		return reservation{}, false
	}
	return reservation{limiter: s.limiter, keyID: key.ID, limits: limits, reserved: d.Reserved}, true
}

// settle acerta a reserva com o uso real; used zero devolve a reserva inteira.
// Usa um contexto que o fim do pedido não cancela: se o cliente desistir, o
// acerto ainda precisa acontecer, senão a reserva ficaria presa no balde.
func (res reservation) settle(ctx context.Context, used int) {
	if res.limiter == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), settleTimeout)
	defer cancel()
	if err := res.limiter.Settle(ctx, res.keyID, res.limits, res.reserved, used); err != nil {
		log.Printf("api: acertando rate limit: %v", err)
	}
}
