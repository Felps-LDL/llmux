package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/usage"
)

// newHandler monta o caminho real (API, router e breaker) só com o provedor trocado
// pelo falso. Assim o teste exercita a integração entre eles, sem rede.
func newHandler(p provider.Provider) http.Handler {
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: p, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})

	prices := usage.PriceTable{
		{Provider: "a", Model: "modelo-fisico"}: {InputPerMillion: 150_000, OutputPerMillion: 600_000},
	}
	return api.New(r, prices).Handler()
}

// Verifica o contrato que permite ao cliente trocar só a base_url: um pedido no formato
// da OpenAI entra, e uma resposta no formato da OpenAI sai. O httptest.NewRecorder chama
// o handler direto, sem abrir porta, e guarda o status, os cabeçalhos e o corpo.
func TestChatCompletionsTraduzPedidoEResposta(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{
		Content:      "olá",
		FinishReason: "stop",
		Usage:        provider.Usage{PromptTokens: 1000, CompletionTokens: 500},
	}))
	body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}], "temperature": 0, "top_p": 1}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (corpo: %s)", rec.Code, http.StatusOK, rec.Body)
	}

	var got struct {
		Object  string `json:"object"`
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("corpo da resposta: %v", err)
	}

	if got.Object != "chat.completion" {
		t.Errorf("object = %q, want %q", got.Object, "chat.completion")
	}
	// O cliente recebe o nome lógico que pediu, não o físico.
	if got.Model != "fast" {
		t.Errorf("model = %q, want %q", got.Model, "fast")
	}
	if len(got.Choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(got.Choices))
	}
	if c := got.Choices[0]; c.Message.Role != "assistant" || c.Message.Content != "olá" || c.FinishReason != "stop" {
		t.Errorf("choice = %+v, want assistant/olá/stop", c)
	}
	if got.Usage.TotalTokens != 1500 {
		t.Errorf("total_tokens = %d, want 1500", got.Usage.TotalTokens)
	}

	// 1000 × 150.000 + 500 × 600.000, calculado à mão: é o que liga o custo ao router.
	if got := rec.Header().Get("X-Llmux-Cost-Picodollars"); got != "450000000" {
		t.Errorf("custo = %q, want %q", got, "450000000")
	}

	// O outro lado da tradução: o que chegou ao provedor.
	calls := a.Calls()
	if len(calls) != 1 {
		t.Fatalf("chamadas = %d, want 1", len(calls))
	}
	if calls[0].Model != "modelo-fisico" {
		t.Errorf("Model no provedor = %q, want %q", calls[0].Model, "modelo-fisico")
	}
	// Temperatura 0 precisa sobreviver à leitura do JSON do cliente.
	if calls[0].Temperature == nil || *calls[0].Temperature != 0 {
		t.Errorf("Temperature = %v, want 0", calls[0].Temperature)
	}
}

// Verifica a tradução de falhas para HTTP. Importa porque o SDK do cliente decide o que
// fazer pelo status: um 429 é tratado diferente de um 503. E o caso sem classe prova que o
// detalhe de um provedor não chega ao cliente.
func TestChatCompletionsTraduzErroParaStatus(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantType   string
	}{
		{name: "indisponível", err: provider.ErrUnavailable, wantStatus: http.StatusServiceUnavailable, wantType: "server_error"},
		{name: "tempo esgotado", err: provider.ErrTimeout, wantStatus: http.StatusGatewayTimeout, wantType: "server_error"},
		{name: "limite excedido", err: provider.ErrRateLimited, wantStatus: http.StatusTooManyRequests, wantType: "rate_limit_error"},
		{name: "sem classe", err: errors.New("segredo interno"), wantStatus: http.StatusBadGateway, wantType: "server_error"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Fail(tt.err))
			body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()

			newHandler(a).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			var got struct {
				Error struct {
					Type string `json:"type"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("corpo da resposta: %v", err)
			}
			if got.Error.Type != tt.wantType {
				t.Errorf("error.type = %q, want %q", got.Error.Type, tt.wantType)
			}
			if strings.Contains(rec.Body.String(), "segredo interno") {
				t.Errorf("detalhe do provedor vazou para o cliente: %s", rec.Body)
			}
		})
	}
}

func TestChatCompletionsRecusaPedidoInvalido(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
	}{
		{name: "JSON inválido", body: `{"model":`, wantStatus: http.StatusBadRequest},
		{name: "stream", body: `{"model": "fast", "stream": true, "messages": [{"role": "user", "content": "oi"}]}`, wantStatus: http.StatusBadRequest},
		{name: "sem model", body: `{"messages": [{"role": "user", "content": "oi"}]}`, wantStatus: http.StatusBadRequest},
		{name: "sem messages", body: `{"model": "fast", "messages": []}`, wantStatus: http.StatusBadRequest},
		{name: "role inválido", body: `{"model": "fast", "messages": [{"role": "assistent", "content": "oi"}]}`, wantStatus: http.StatusBadRequest},
		{name: "modelo desconhecido", body: `{"model": "nao-existe", "messages": [{"role": "user", "content": "oi"}]}`, wantStatus: http.StatusNotFound},
		{name: "limite negativo", body: `{"model": "fast", "max_completion_tokens": -1, "messages": [{"role": "user", "content": "oi"}]}`, wantStatus: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body))
			rec := httptest.NewRecorder()

			newHandler(a).ServeHTTP(rec, req)

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tt.wantStatus)
			}

			if calls := a.Calls(); len(calls) != 0 {
				t.Errorf("provedor foi chamado %d vezes, mas o pedido era inválido", len(calls))
			}
		})
	}
}

func TestChatCompletionsNaoRespondeClienteQueDesistiu(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	if rec.Body.Len() != 0 {
		t.Errorf("corpo da resposta = %d, want 0", rec.Body.Len())
	}
	if calls := a.Calls(); len(calls) != 0 {
		t.Errorf("provedor foi chamado %d vezes, mas o cliente desistiu", len(calls))
	}
}

func TestChatCompletionsRepassaLimiteDeTokens(t *testing.T) {
	tests := []struct {
		name   string
		fields string
		want   int
	}{
		{name: "max_tokens", fields: `"max_tokens": 5`, want: 5},
		{name: "max_completion_tokens", fields: `"max_completion_tokens": 7`, want: 7},
		{name: "os dois", fields: `"max_tokens": 5, "max_completion_tokens": 7`, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
			body := `{"model": "fast", "messages": [{"role": "user", "content": "oi"}], ` + tt.fields + `}`
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			rec := httptest.NewRecorder()

			newHandler(a).ServeHTTP(rec, req)

			calls := a.Calls()
			if len(calls) != 1 {
				t.Fatalf("chamadas = %d, want 1", len(calls))
			}
			if calls[0].MaxTokens != tt.want {
				t.Errorf("MaxTokens = %d, want %d", calls[0].MaxTokens, tt.want)
			}
		})
	}
}
