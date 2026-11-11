package router_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/telemetry"
)

// Verifica o rastro do fallback, que era a lacuna que motivou a Fase 3: um pedido
// atendido pela segunda rota parecia normal, e um provedor caindo o tempo todo
// passava despercebido. Cada tentativa que falha e cada rota pulada precisam virar
// uma linha, com o request_id do pedido para ligar a linha ao access log.
func TestFallbackDeixaRastroNoLog(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&buf, slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(prev) })

	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}), fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-a"}, {Provider: b, Model: "modelo-b"}},
	}, time.Second, router.BreakerConfig{Threshold: 1, Cooldown: time.Minute})
	ctx := telemetry.WithRequestID(t.Context(), "req-9")

	// O primeiro pedido falha em A e abre o circuito; o segundo pula A.
	for range 2 {
		if _, err := r.Chat(ctx, "fast", provider.ChatRequest{}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}

	var msgs []string
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("linha não é JSON: %s", line)
		}
		if m["provider"] != "a" || m["request_id"] != "req-9" {
			t.Errorf("linha sem o provedor a ou sem o request_id: %v", m)
		}
		msgs = append(msgs, m["msg"].(string))
	}
	want := []string{"tentativa falhou, indo para a próxima rota", "circuito aberto, rota pulada"}
	if strings.Join(msgs, "|") != strings.Join(want, "|") {
		t.Errorf("mensagens = %q, want %q", msgs, want)
	}
}
