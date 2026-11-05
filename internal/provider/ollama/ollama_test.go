package ollama_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/ollama"
)

// Verifica as duas direções da tradução, sobre HTTP de verdade: o que o Ollama
// recebe e o que o gateway devolve. O falso não testaria isso, porque ele nem passa
// por JSON. O httptest sobe um servidor local, sem precisar do Ollama instalado.
func TestChatTraduzPedidoEResposta(t *testing.T) {
	var got struct {
		Model    string `json:"model"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		// Ponteiros para diferenciar "ausente" de "false" e de "0" no JSON recebido.
		Stream  *bool `json:"stream"`
		Options struct {
			Temperature *float64 `json:"temperature"`
		} `json:"options"`
	}
	var path string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("corpo do pedido: %v", err)
		}
		// No formato que o Ollama documenta; não foi capturada de um servidor real.
		_, _ = w.Write([]byte(`{
			"model": "llama3.2",
			"message": {"role": "assistant", "content": "olá"},
			"done": true,
			"done_reason": "length",
			"prompt_eval_count": 10,
			"eval_count": 20
		}`))
	}))
	defer srv.Close()

	temperature := 0.0
	c := ollama.New(srv.URL, srv.Client())
	resp, err := c.Chat(t.Context(), provider.ChatRequest{
		Model:       "llama3.2",
		Messages:    []provider.Message{{Role: provider.RoleUser, Content: "oi"}},
		Temperature: &temperature,
	})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if path != "/api/chat" {
		t.Errorf("caminho = %q, want %q", path, "/api/chat")
	}
	// Sem stream: false, o Ollama responderia em streaming e o JSON não seria lido.
	if got.Stream == nil || *got.Stream {
		t.Errorf("stream = %v, want false", got.Stream)
	}
	// O bug que o ponteiro evita: temperatura 0 sumir do JSON e virar o default.
	if got.Options.Temperature == nil || *got.Options.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", got.Options.Temperature)
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "oi" {
		t.Errorf("messages = %+v, want uma mensagem user/oi", got.Messages)
	}

	if resp.Content != "olá" {
		t.Errorf("Content = %q, want %q", resp.Content, "olá")
	}
	if resp.FinishReason != "length" {
		t.Errorf("FinishReason = %q, want %q", resp.FinishReason, "length")
	}
	// Os tokens são a base do custo: trocar os dois não daria erro nenhum.
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("Usage = %+v, want 10 de entrada e 20 de saída", resp.Usage)
	}
}

// Verifica a tradução de status HTTP para as classes de erro, que é o que decide o
// fallback e o breaker. Um erro aqui faria o router tentar de novo um pedido inválido,
// ou desistir de um provedor que só estava momentaneamente fora.
func TestChatTraduzStatusParaClasseDeErro(t *testing.T) {
	classes := []error{provider.ErrRateLimited, provider.ErrUnavailable, provider.ErrTimeout}

	tests := []struct {
		name   string
		status int
		want   error // nil significa "sem nenhuma classe"
	}{
		{name: "limite excedido", status: http.StatusTooManyRequests, want: provider.ErrRateLimited},
		{name: "erro interno", status: http.StatusInternalServerError, want: provider.ErrUnavailable},
		{name: "pedido inválido", status: http.StatusBadRequest, want: nil},
		{name: "modelo inexistente", status: http.StatusNotFound, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error": "detalhe do erro"}`))
			}))
			defer srv.Close()

			_, err := ollama.New(srv.URL, srv.Client()).Chat(t.Context(), provider.ChatRequest{})
			if err == nil {
				t.Fatal("esperava erro")
			}

			// Olha todas as classes: "ter a classe certa" não basta, porque um erro
			// embrulhado em duas classes também passaria num único errors.Is.
			for _, class := range classes {
				if got, want := errors.Is(err, class), errors.Is(tt.want, class); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v (erro: %v)", class, got, want, err)
				}
			}
		})
	}
}

func TestChatTrataRespostaRuimComStatus200ComoIndisponivel(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{name: "JSON malformado", body: `{"model": "m"`},
		{name: "erro no modelo", body: `{"error": "falha no modelo"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := ollama.New(srv.URL, srv.Client()).Chat(t.Context(), provider.ChatRequest{})

			if !errors.Is(err, provider.ErrUnavailable) {
				t.Errorf("esperava ErrUnavailable, mas deu: %v", err)
			}
		})
	}
}

func TestChatTraduzPrazoEsgotadoParaErrTimeout(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()

	_, err := ollama.New(srv.URL, srv.Client()).Chat(ctx, provider.ChatRequest{})

	if !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("esperava ErrTimeout, mas deu: %v", err)
	}
}

func TestChatNaoClassificaCancelamento(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-release
	}))
	defer srv.Close()
	defer close(release)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := ollama.New(srv.URL, srv.Client()).Chat(ctx, provider.ChatRequest{})

	if !errors.Is(err, context.Canceled) {
		t.Errorf("esperava context.Canceled, mas deu: %v", err)
	}

	for _, class := range []error{provider.ErrRateLimited, provider.ErrUnavailable, provider.ErrTimeout} {
		if errors.Is(err, class) {
			t.Errorf("não esperava que err fosse %v, mas é: %v", class, err)
		}
	}
}
