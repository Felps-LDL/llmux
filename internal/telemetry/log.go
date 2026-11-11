// Package telemetry reúne o que o gateway expõe para ser operado: logs, métricas e
// tracing.
package telemetry

import (
	"context"
	"io"
	"log/slog"
)

type requestIDKey struct{}

// WithRequestID guarda o id do pedido no contexto. O contexto já passa por todas as
// camadas, então nenhuma função precisa receber o id, nem um logger, como parâmetro.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestID devolve o id guardado por WithRequestID, ou "" fora de um pedido.
func RequestID(ctx context.Context) string {
	id, _ := ctx.Value(requestIDKey{}).(string)
	return id
}

// contextHandler põe o request_id do contexto em toda linha de log. Só funciona com
// as funções que recebem o contexto (slog.InfoContext e afins); por isso o código do
// gateway usa sempre essas.
type contextHandler struct {
	slog.Handler
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := RequestID(ctx); id != "" {
		r.AddAttrs(slog.String("request_id", id))
	}
	return h.Handler.Handle(ctx, r)
}

// WithAttrs e WithGroup precisam devolver um contextHandler: herdados do Handler
// embutido, devolveriam o handler interno, e um logger.With(...) perderia o
// request_id sem nenhum erro.
func (h contextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return contextHandler{h.Handler.WithAttrs(attrs)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.Handler.WithGroup(name)}
}

// NewLogger cria o logger do gateway: JSON, uma linha por evento, para um coletor
// (Loki, CloudWatch, o docker logs) conseguir filtrar por campo sem expressão
// regular.
func NewLogger(w io.Writer, level slog.Level) *slog.Logger {
	return slog.New(contextHandler{slog.NewJSONHandler(w, &slog.HandlerOptions{Level: level})})
}
