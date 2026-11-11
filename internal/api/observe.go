package api

import (
	"context"
	"crypto/rand"
	"log/slog"
	"net/http"
	"regexp"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Felps-LDL/llmux/internal/telemetry"
)

const requestIDHeader = "X-Request-Id"

// Um id do cliente só é aceito neste formato. Uma quebra de linha nele forjaria uma
// linha de log falsa, e um id de 1 MB inflaria cada linha do pedido.
var validRequestID = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// requestID dá um id a cada pedido e o devolve no cabeçalho da resposta. Aceitar o
// id do cliente deixa ele cruzar os próprios logs com os do gateway.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(requestIDHeader)
		if !validRequestID.MatchString(id) {
			id = rand.Text()
		}
		w.Header().Set(requestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(telemetry.WithRequestID(r.Context(), id)))
	})
}

// requestInfo junta o que só se sabe durante o pedido, para o access log escrever
// numa linha só no fim. Cada campo é preenchido por quem o descobre: a chave pela
// autenticação, o provedor pelo handler.
type requestInfo struct {
	// start é o início do pedido, para o tempo até o primeiro token contar desde a
	// chegada, e não desde a chamada ao provedor: é o tempo que o usuário espera.
	start    time.Time
	keyID    int64
	model    string
	provider string
	cache    string
}

type requestInfoKey struct{}

// infoFrom devolve o requestInfo do pedido. Fora do accessLog (num teste que chama
// um handler direto), devolve um descartável, para ninguém precisar checar nil.
func infoFrom(ctx context.Context) *requestInfo {
	if info, ok := ctx.Value(requestInfoKey{}).(*requestInfo); ok {
		return info
	}
	return &requestInfo{}
}

// Status que o nginx usa para "o cliente fechou a conexão": nenhum status HTTP
// padrão diz isso, e um 200 no log esconderia que ninguém recebeu a resposta.
const statusClientClosed = 499

// accessLog escreve uma linha por pedido, com o resultado e a duração, e mede o
// pedido nas métricas: é o único lugar que conhece tudo isso no fim. Nunca registra
// o conteúdo das mensagens nem a chave de API: o conteúdo pode ter dados pessoais,
// e da chave basta o id.
func (s *Server) accessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		info := &requestInfo{start: start}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}

		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), requestInfoKey{}, info)))

		status := sw.status
		if !sw.wroteHeader && r.Context().Err() != nil {
			status = statusClientClosed
		}
		elapsed := time.Since(start)
		// O modelo vem do cliente. Como rótulo de métrica, um nome inventado criaria
		// uma série nova no Prometheus, guardada para sempre: mil nomes, mil séries. Só
		// os modelos da config viram rótulo; o log fica com o nome que veio.
		metricModel := info.model
		if metricModel != "" && !s.router.HasModel(metricModel) {
			metricModel = "unknown"
		}
		s.metrics.ObserveRequest(info.provider, metricModel, status, elapsed)
		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("status", status),
			slog.Int64("duration_ms", elapsed.Milliseconds()),
		}
		if info.keyID != 0 {
			attrs = append(attrs, slog.Int64("key_id", info.keyID))
		}
		for _, a := range []struct{ k, v string }{{"model", info.model}, {"provider", info.provider}, {"cache", info.cache}} {
			if a.v != "" {
				attrs = append(attrs, slog.String(a.k, a.v))
			}
		}

		// Os mesmos campos no span do pedido, para o trace ser filtrável por chave,
		// modelo ou provedor no Jaeger, como o log é.
		trace.SpanFromContext(r.Context()).SetAttributes(
			attribute.Int64("llmux.key_id", info.keyID),
			attribute.String("llmux.model", info.model),
			attribute.String("llmux.provider", info.provider),
			attribute.String("llmux.cache", info.cache),
		)

		level := slog.LevelInfo
		if status >= http.StatusInternalServerError {
			level = slog.LevelError
		}
		slog.LogAttrs(r.Context(), level, "pedido", attrs...)
	})
}

// statusWriter guarda o status que o handler escreveu.
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	w.wroteHeader = true
	return w.ResponseWriter.Write(b)
}

// Unwrap é o que deixa o http.ResponseController alcançar o Flush do writer
// original. Sem ele, o SSE deixaria de entregar aos poucos, sem nenhum erro: o
// cliente receberia tudo só no fim.
func (w *statusWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

// setCache põe o resultado do cache no cabeçalho e no access log de uma vez, para
// os dois nunca divergirem.
func (info *requestInfo) setCache(w http.ResponseWriter, result string) {
	w.Header().Set(cacheHeader, result)
	info.cache = result
}
