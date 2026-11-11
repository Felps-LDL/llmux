package telemetry_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"testing"

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
