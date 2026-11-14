package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/Felps-LDL/llmux/internal/telemetry"
)

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := bytes.Split(bytes.TrimSpace(buf.Bytes()), []byte("\n"))
	var got map[string]any
	if err := json.Unmarshal(lines[len(lines)-1], &got); err != nil {
		t.Fatalf("a linha não é JSON: %v (%s)", err, buf)
	}
	return got
}

// Verifica o mecanismo do componente: o request_id do contexto aparece na linha
// sem ninguém passá-lo como argumento. O caso do logger.With importa porque é o
// jeito comum de criar um logger com campos fixos; sem o WithAttrs próprio, ele
// devolveria o handler interno, e o request_id sumiria sem nenhum erro.
func TestLoggerPoeORequestIDDoContexto(t *testing.T) {
	var buf bytes.Buffer
	logger := telemetry.NewLogger(&buf, slog.LevelInfo)
	ctx := telemetry.WithRequestID(context.Background(), "abc-123")

	logger.InfoContext(ctx, "direto")
	if got := lastLine(t, &buf)["request_id"]; got != "abc-123" {
		t.Errorf("request_id = %v, want abc-123", got)
	}

	logger.With("component", "router").InfoContext(ctx, "com campos fixos")
	line := lastLine(t, &buf)
	if line["request_id"] != "abc-123" || line["component"] != "router" {
		t.Errorf("linha = %v, want request_id e component", line)
	}

	logger.WithGroup("g").InfoContext(ctx, "num grupo")
	if _, ok := lastLine(t, &buf)["g"]; !ok {
		t.Errorf("linha sem o grupo: %s", buf.String())
	}
}

// Fora de um pedido, como na subida do gateway, não há id, e a linha não pode
// ganhar um request_id vazio que confundiria uma busca por id.
func TestLoggerSemPedidoNaoPoeRequestID(t *testing.T) {
	var buf bytes.Buffer
	telemetry.NewLogger(&buf, slog.LevelInfo).InfoContext(context.Background(), "subida")

	if _, ok := lastLine(t, &buf)["request_id"]; ok {
		t.Errorf("linha com request_id fora de um pedido: %s", buf.String())
	}
}

// O trace_id é a ponte do log para o trace: uma linha escrita dentro de um span
// precisa levar o id do trace, no mesmo formato que o Jaeger mostra.
func TestLoggerPoeOTraceIDDoSpan(t *testing.T) {
	var buf bytes.Buffer
	traceID, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	spanID, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	ctx := trace.ContextWithSpanContext(context.Background(), trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled,
	}))

	telemetry.NewLogger(&buf, slog.LevelInfo).InfoContext(ctx, "dentro de um span")

	if got := lastLine(t, &buf)["trace_id"]; got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_id = %v, want o do span", got)
	}
}

// Sem OTEL_EXPORTER_OTLP_ENDPOINT, o tracing fica desligado e o fim é um nada:
// subir o gateway sem coletor não pode falhar.
func TestSetupTracingSemEndpointNaoFazNada(t *testing.T) {
	shutdown, err := telemetry.SetupTracing(t.Context(), func(string) string { return "" })
	if err != nil {
		t.Fatalf("SetupTracing: %v", err)
	}
	if err := shutdown(t.Context()); err != nil {
		t.Errorf("shutdown: %v", err)
	}
}
