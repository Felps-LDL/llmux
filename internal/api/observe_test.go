package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/telemetry"
)

// captureLogs troca o logger padrão por um que escreve num buffer, e o devolve no
// fim do teste. Por mexer num estado global, os testes deste pacote não podem
// rodar em paralelo.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(telemetry.NewLogger(&buf, slog.LevelDebug))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// logLines devolve as linhas de log com a mensagem msg.
func logLines(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(buf.String()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("linha de log não é JSON: %v (%s)", err, line)
		}
		if m["msg"] == msg {
			out = append(out, m)
		}
	}
	return out
}

// Verifica a entrada do request_id, que é a porta para o log: um id do cliente no
// formato certo é aceito e devolvido, para ele cruzar os próprios logs com os
// nossos. Os inválidos são trocados, porque uma quebra de linha forjaria uma linha
// de log falsa e um id enorme incharia todas as linhas do pedido.
func TestRequestIDAceitaSoIdsValidos(t *testing.T) {
	tests := []struct {
		name     string
		header   string
		wantSame bool
	}{
		{name: "válido", header: "pedido-42_a.b", wantSame: true},
		{name: "ausente", header: "", wantSame: false},
		{name: "com quebra de linha", header: "abc\nlevel=ERROR", wantSame: false},
		{name: "longo demais", header: strings.Repeat("a", 65), wantSame: false},
		{name: "com espaço", header: "abc def", wantSame: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
				strings.NewReader(`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`))
			req.Header.Set("Authorization", "Bearer "+testKey)
			if tt.header != "" {
				req.Header["X-Request-Id"] = []string{tt.header}
			}
			rec := httptest.NewRecorder()

			newHandler(a).ServeHTTP(rec, req)

			got := rec.Header().Get("X-Request-Id")
			if tt.wantSame && got != tt.header {
				t.Errorf("X-Request-Id = %q, want o do cliente (%q)", got, tt.header)
			}
			if !tt.wantSame && (got == tt.header || got == "") {
				t.Errorf("X-Request-Id = %q, want um id novo", got)
			}
		})
	}
}

// Verifica a linha do access log, que é o que um operador lê para entender um
// pedido: o resultado, quem pediu e quem atendeu, todos ligados pelo mesmo
// request_id. E o que não pode estar nela: a chave de API e o conteúdo da
// conversa, que pode ter dados pessoais.
func TestAccessLogRegistraOPedidoSemSegredos(t *testing.T) {
	buf := captureLogs(t)
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "resposta secreta"}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model": "fast", "messages": [{"role": "user", "content": "pergunta secreta"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("X-Request-Id", "req-1")
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	lines := logLines(t, buf, "pedido")
	if len(lines) != 1 {
		t.Fatalf("linhas do access log = %d, want 1 (log: %s)", len(lines), buf)
	}
	line := lines[0]
	want := map[string]any{
		"request_id": "req-1",
		"method":     "POST",
		"path":       "/v1/chat/completions",
		"status":     float64(200),
		"key_id":     float64(1),
		"model":      "fast",
		"provider":   "a",
		"cache":      "bypass",
	}
	for k, v := range want {
		if line[k] != v {
			t.Errorf("%s = %v, want %v", k, line[k], v)
		}
	}
	for _, secret := range []string{testKey, "pergunta secreta", "resposta secreta"} {
		if strings.Contains(buf.String(), secret) {
			t.Errorf("o log contém %q: %s", secret, buf)
		}
	}
}

func TestAccessLogRegistraFalhaDeAutenticacaoComRequestID(t *testing.T) {
	buf := captureLogs(t)
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`))
	req.Header.Set("Authorization", "Bearer llmux_errada")
	req.Header.Set("X-Request-Id", "req-401")

	newHandler(a).ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf, "pedido")
	if len(lines) != 1 || lines[0]["status"] != float64(401) || lines[0]["request_id"] != "req-401" {
		t.Fatalf("access log = %v, want uma linha com 401 e o request_id", lines)
	}
	if _, ok := lines[0]["key_id"]; ok {
		t.Errorf("key_id num pedido sem chave válida: %v", lines[0])
	}
}

// Um cliente que desiste antes da resposta não recebe nada, e um 200 no log
// esconderia isso. O 499 é a convenção do nginx para "o cliente fechou".
func TestAccessLogMarcaClienteQueDesistiuCom499(t *testing.T) {
	buf := captureLogs(t)
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`))
	req.Header.Set("Authorization", "Bearer "+testKey)

	newHandler(a).ServeHTTP(httptest.NewRecorder(), req)

	lines := logLines(t, buf, "pedido")
	if len(lines) != 1 || lines[0]["status"] != float64(499) {
		t.Errorf("access log = %v, want status 499", lines)
	}
}

// Verifica que o access log mede o pedido com os mesmos dados que escreve: o
// provedor que atendeu e o modelo lógico. É o caminho real, do handler até o texto
// que o Prometheus lê.
func TestAccessLogMedePedidoNasMetricas(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	m := telemetry.NewMetrics(nil)
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	h := api.New(api.Options{Router: r, Keys: keys{testKey: {ID: 1}}, Metrics: m}).Handler()

	post(t.Context(), t, h, `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`)

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	if want := `llm_requests_total{model="fast",provider="a",status="200"} 1`; !strings.Contains(rec.Body.String(), want) {
		t.Errorf("/metrics sem %q", want)
	}
}

// Um nome de modelo inventado pelo cliente não pode virar rótulo: cada nome seria uma
// série nova no Prometheus, e um cliente com uma chave válida poderia criar milhões.
// Todos os desconhecidos dividem a série "unknown".
func TestMetricasNaoUsamModeloDesconhecidoComoRotulo(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	m := telemetry.NewMetrics(nil)
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	h := api.New(api.Options{Router: r, Keys: keys{testKey: {ID: 1}}, Metrics: m}).Handler()

	for _, model := range []string{"inventado-1", "inventado-2"} {
		post(t.Context(), t, h, `{"model": "`+model+`", "messages": [{"role": "user", "content": "oi"}]}`)
	}

	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if strings.Contains(body, "inventado") {
		t.Errorf("/metrics tem um nome do cliente como rótulo:\n%s", body)
	}
	if want := `llm_requests_total{model="unknown",provider="none",status="404"} 2`; !strings.Contains(body, want) {
		t.Errorf("/metrics sem %q", want)
	}
}
