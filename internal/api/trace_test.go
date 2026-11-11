package api_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
)

// Verifica o trace de um pedido pelo caminho real. O traceparent do cliente precisa
// ser respeitado: o trace do gateway continua o dele, com o mesmo id, e é assim que
// um pedido aparece inteiro no Jaeger, do cliente ao provedor. O span do router
// precisa ser filho do span do pedido; solto, o cronograma não mostraria de quem é
// cada etapa. E a linha do log leva o mesmo trace_id.
func TestPedidoContinuaOTraceDoClienteEMostraAsEtapas(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prevTP, prevProp := otel.GetTracerProvider(), otel.GetTextMapPropagator()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	otel.SetTextMapPropagator(propagation.TraceContext{})
	t.Cleanup(func() { otel.SetTracerProvider(prevTP); otel.SetTextMapPropagator(prevProp) })
	logs := captureLogs(t)

	const clientTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Traceparent", "00-"+clientTrace+"-00f067aa0ba902b7-01")

	newHandler(a).ServeHTTP(httptest.NewRecorder(), req)

	spans := rec.Ended()
	var server, attempt sdktrace.ReadOnlySpan
	for _, s := range spans {
		switch s.Name() {
		case "POST /v1/chat/completions":
			server = s
		case "router.attempt":
			attempt = s
		}
	}
	if server == nil || attempt == nil {
		t.Fatalf("spans = %d, want o do pedido e o do router", len(spans))
	}
	if got := server.SpanContext().TraceID().String(); got != clientTrace {
		t.Errorf("trace do pedido = %s, want o do cliente (%s)", got, clientTrace)
	}
	if attempt.Parent().SpanID() != server.SpanContext().SpanID() {
		t.Error("o span do router não é filho do span do pedido")
	}
	var model string
	for _, kv := range server.Attributes() {
		if kv.Key == "llmux.model" {
			model = kv.Value.AsString()
		}
	}
	if model != "fast" {
		t.Errorf("llmux.model no span do pedido = %q, want fast", model)
	}

	lines := logLines(t, logs, "pedido")
	if len(lines) != 1 || lines[0]["trace_id"] != clientTrace {
		t.Errorf("access log = %v, want o trace_id do cliente", lines)
	}
}

// As ferramentas de trace agrupam spans pelo nome. Com o caminho cru no nome, cada
// URL inventada criaria um nome novo, o mesmo problema de cardinalidade das métricas.
func TestSpanDeRotaDesconhecidaTemNomeFixo(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	for _, path := range []string{"/admin", "/v1/qualquer/coisa-123"} {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		newHandler(fake.New("a")).ServeHTTP(httptest.NewRecorder(), req)
	}

	for _, s := range rec.Ended() {
		if s.Name() != "GET rota desconhecida" {
			t.Errorf("span = %q, want %q", s.Name(), "GET rota desconhecida")
		}
	}
	if n := len(rec.Ended()); n != 2 {
		t.Errorf("spans = %d, want 2", n)
	}
}
