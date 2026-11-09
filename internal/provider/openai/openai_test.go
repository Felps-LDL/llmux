package openai_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/openai"
)

type sentRequest struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Temperature         *float64 `json:"temperature"`
	MaxTokens           *int     `json:"max_tokens"`
	MaxCompletionTokens *int     `json:"max_completion_tokens"`
	Stream              bool     `json:"stream"`
	StreamOptions       *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
}

// Verifica as duas direções da tradução, sobre HTTP de verdade. Além dos campos,
// importam dois detalhes que a OpenAI cobra: a chave no cabeçalho Authorization, e
// o limite em max_completion_tokens, que os modelos de raciocínio exigem.
func TestChatTraduzPedidoEResposta(t *testing.T) {
	var got sentRequest
	var path, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("corpo do pedido: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-1", "object": "chat.completion", "model": "gpt-x-2026",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "olá"}, "finish_reason": "length"}],
			"usage": {"prompt_tokens": 10, "completion_tokens": 20, "total_tokens": 30}
		}`))
	}))
	defer srv.Close()

	temperature := 0.0
	c := openai.New(srv.URL+"/v1", "chave-secreta", srv.Client())
	resp, err := c.Chat(t.Context(), provider.ChatRequest{
		Model:       "gpt-x",
		Messages:    []provider.Message{{Role: provider.RoleUser, Content: "oi"}},
		Temperature: &temperature,
		MaxTokens:   7,
	})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if path != "/v1/chat/completions" {
		t.Errorf("caminho = %q, want /v1/chat/completions", path)
	}
	if auth != "Bearer chave-secreta" {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer chave-secreta")
	}
	if got.Model != "gpt-x" || len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "oi" {
		t.Errorf("pedido = %+v, want gpt-x com uma mensagem user/oi", got)
	}
	if got.Temperature == nil || *got.Temperature != 0 {
		t.Errorf("temperature = %v, want 0", got.Temperature)
	}
	if got.MaxCompletionTokens == nil || *got.MaxCompletionTokens != 7 || got.MaxTokens != nil {
		t.Errorf("max_completion_tokens = %v, max_tokens = %v, want 7 e ausente", got.MaxCompletionTokens, got.MaxTokens)
	}
	if got.Stream || got.StreamOptions != nil {
		t.Errorf("stream = %v, stream_options = %v, want sem streaming", got.Stream, got.StreamOptions)
	}

	if resp.Content != "olá" || resp.FinishReason != "length" || resp.Model != "gpt-x-2026" {
		t.Errorf("resposta = %+v, want olá/length/gpt-x-2026", resp)
	}
	if resp.Usage.PromptTokens != 10 || resp.Usage.CompletionTokens != 20 {
		t.Errorf("Usage = %+v, want 10 de entrada e 20 de saída", resp.Usage)
	}
}

func TestChatTraduzStatusParaClasseDeErro(t *testing.T) {
	classes := []error{provider.ErrRateLimited, provider.ErrUnavailable, provider.ErrTimeout}

	tests := []struct {
		name   string
		status int
		want   error // nil significa "sem nenhuma classe"
	}{
		{name: "limite excedido", status: http.StatusTooManyRequests, want: provider.ErrRateLimited},
		{name: "sobrecarregado", status: http.StatusServiceUnavailable, want: provider.ErrUnavailable},
		{name: "chave inválida", status: http.StatusUnauthorized, want: nil},
		{name: "pedido inválido", status: http.StatusBadRequest, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"error": {"message": "detalhe do erro", "type": "x"}}`))
			}))
			defer srv.Close()

			_, err := openai.New(srv.URL, "k", srv.Client()).Chat(t.Context(), provider.ChatRequest{})
			if err == nil {
				t.Fatal("esperava erro")
			}
			for _, class := range classes {
				if got, want := errors.Is(err, class), errors.Is(tt.want, class); got != want {
					t.Errorf("errors.Is(err, %v) = %v, want %v (erro: %v)", class, got, want, err)
				}
			}
		})
	}
}

// Verifica a montagem do stream a partir do SSE da OpenAI. Os eventos seguem o que o
// Ollama, imitando a OpenAI, mandou de verdade, mais um comentário de keep-alive. O
// ponto principal: finish_reason e uso chegam em eventos separados, e o adaptador
// precisa juntá-los no último pedaço, que é o contrato que o router e a API seguem.
func TestChatStreamJuntaFimEUsoNoUltimoPedaco(t *testing.T) {
	var got sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("corpo do pedido: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"o"},"finish_reason":null}]}` + "\n\n" +
				": keep-alive\n\n" +
				`data: {"choices":[{"index":0,"delta":{"content":"lá"},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
				`data: {"choices":[],"usage":{"prompt_tokens":26,"completion_tokens":2,"total_tokens":28}}` + "\n\n" +
				"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	stream, err := openai.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()

	var chunks []provider.StreamChunk
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Recv: erro inesperado: %v", err)
		}
		chunks = append(chunks, chunk)
	}

	// Sem include_usage, a OpenAI não manda o uso, e o gateway não teria custo.
	if !got.Stream || got.StreamOptions == nil || !got.StreamOptions.IncludeUsage {
		t.Errorf("stream = %v, stream_options = %v, want stream com include_usage", got.Stream, got.StreamOptions)
	}
	if len(chunks) != 3 {
		t.Fatalf("pedaços = %d, want 3 (o, lá e o final): %+v", len(chunks), chunks)
	}
	if chunks[0].Content+chunks[1].Content != "olá" {
		t.Errorf("conteúdo = %q, want %q", chunks[0].Content+chunks[1].Content, "olá")
	}
	for i, c := range chunks[:2] {
		if c.FinishReason != "" || c.Usage != nil {
			t.Errorf("pedaço %d = %+v, want sem fim e sem uso", i, c)
		}
	}
	last := chunks[2]
	if last.FinishReason != "length" || last.Usage == nil || last.Usage.PromptTokens != 26 || last.Usage.CompletionTokens != 2 {
		t.Errorf("último pedaço = %+v (uso %+v), want length com 26 e 2 tokens", last, last.Usage)
	}
}

func TestChatStreamTrataCorteAntesDoDoneComoIndisponivel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"o"},"finish_reason":null}]}` + "\n\n"))
	}))
	defer srv.Close()

	stream, err := openai.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Recv(); err != nil {
		t.Fatalf("primeiro Recv: erro inesperado: %v", err)
	}
	if _, err := stream.Recv(); errors.Is(err, io.EOF) || !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Recv após o corte: erro = %v, want ErrUnavailable e não io.EOF", err)
	}
}

func TestChatStreamTrataEventoDeErroComoIndisponivel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// O [DONE] depois do erro garante que o teste prova a leitura do evento de
		// erro. Sem ele, um adaptador que ignorasse o evento ainda falharia pelo
		// corte, e o teste passaria pelo motivo errado.
		_, _ = w.Write([]byte(`data: {"error":{"message":"falha no modelo","type":"server_error"}}` + "\n\n" +
			"data: [DONE]\n\n"))
	}))
	defer srv.Close()

	stream, err := openai.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Recv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Recv: erro = %v, want ErrUnavailable", err)
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

	_, err := openai.New(srv.URL, "k", srv.Client()).Chat(ctx, provider.ChatRequest{})
	if !errors.Is(err, provider.ErrTimeout) {
		t.Errorf("esperava ErrTimeout, mas deu: %v", err)
	}
}
