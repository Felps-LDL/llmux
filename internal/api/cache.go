package api

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/Felps-LDL/llmux/internal/cache"
	"github.com/Felps-LDL/llmux/internal/provider"
)

const (
	cacheHeader = "X-Llmux-Cache"
	// Quanto a gravação no cache pode demorar depois de a resposta ter saído.
	cacheSetTimeout = 5 * time.Second
)

// ResponseCache é o que a API usa do cache, pelo mesmo motivo de KeyLookup.
type ResponseCache interface {
	Get(ctx context.Context, key string) (cache.Entry, bool, error)
	Set(ctx context.Context, key string, entry cache.Entry) error
}

// cacheKey diz se o pedido pode usar o cache e qual a chave dele. Só com
// temperature 0: acima disso o cliente pede variedade, e devolver sempre a mesma
// resposta mudaria o comportamento sem aviso.
func (s *Server) cacheKey(ctx context.Context, model string, req provider.ChatRequest) (string, bool) {
	if s.cache == nil || req.Temperature == nil || *req.Temperature != 0 {
		return "", false
	}
	key, err := cache.Key(keyFrom(ctx).ID, model, req)
	if err != nil {
		slog.WarnContext(ctx, "cache desligado para o pedido", "error", err)
		return "", false
	}
	return key, true
}

// cacheGet trata um erro do Redis como miss: o cache é otimização, não
// autorização, e uma queda dele não pode derrubar o pedido.
func (s *Server) cacheGet(ctx context.Context, key string) (cache.Entry, bool) {
	ctx, span := startSpan(ctx, "cache.get")
	entry, ok, err := s.cache.Get(ctx, key)
	span.SetAttributes(attribute.Bool("llmux.cache.hit", ok))
	endSpan(span, err)
	if err != nil {
		slog.WarnContext(ctx, "lendo cache", "error", err)
		return cache.Entry{}, false
	}
	return entry, ok
}

// cacheSet usa um contexto que o fim do pedido não cancela: a resposta já saiu, e
// o cliente ir embora não é motivo para perder a gravação.
func (s *Server) cacheSet(ctx context.Context, key string, entry cache.Entry) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cacheSetTimeout)
	defer cancel()
	if err := s.cache.Set(ctx, key, entry); err != nil {
		slog.WarnContext(ctx, "gravando cache", "error", err)
	}
}
