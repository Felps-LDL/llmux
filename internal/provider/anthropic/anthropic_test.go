package anthropic_test

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/anthropic"
)

type sentRequest struct {
	Model     string `json:"model"`
	MaxTokens *int   `json:"max_tokens"`
	System    string `json:"system"`
	Messages  []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Temperature *float64 `json:"temperature"`
	Stream      bool     `json:"stream"`
}

// Verifica as diferenças de formato que tornam este adaptador o mais traiçoeiro:
// a mensagem system vai para um campo próprio; a temperatura não pode ir, porque
// a versão da API a recusa com 400; os blocos de thinking não entram na resposta;
// e a entrada soma os tokens de cache, senão o custo sairia menor que o real.
func TestChatTraduzPedidoEResposta(t *testing.T) {
	var got sentRequest
	var path, key, version string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key, version = r.URL.Path, r.Header.Get("X-Api-Key"), r.Header.Get("Anthropic-Version")
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("corpo do pedido: %v", err)
		}
		_, _ = w.Write([]byte(`{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-x",
			"content": [
				{"type": "thinking", "thinking": ""},
				{"type": "text", "text": "o"},
				{"type": "text", "text": "lá"}
			],
			"stop_reason": "max_tokens",
			"usage": {"input_tokens": 1, "cache_read_input_tokens": 25, "cache_creation_input_tokens": 4, "output_tokens": 5}
		}`))
	}))
	defer srv.Close()

	temperature := 0.2
	c := anthropic.New(srv.URL, "chave-secreta", srv.Client())
	resp, err := c.Chat(t.Context(), provider.ChatRequest{
		Model: "claude-x",
		Messages: []provider.Message{
			{Role: provider.RoleSystem, Content: "seja breve"},
			{Role: provider.RoleUser, Content: "oi"},
		},
		Temperature: &temperature,
		MaxTokens:   7,
	})
	if err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}

	if path != "/v1/messages" || key != "chave-secreta" || version != "2026-10-21" {
		t.Errorf("caminho/chave/versão = %q/%q/%q, want /v1/messages/chave-secreta/2026-10-21", path, key, version)
	}
	if got.System != "seja breve" {
		t.Errorf("system = %q, want %q", got.System, "seja breve")
	}
	if len(got.Messages) != 1 || got.Messages[0].Role != "user" || got.Messages[0].Content != "oi" {
		t.Errorf("messages = %+v, want só a mensagem user/oi", got.Messages)
	}
	if got.Temperature != nil {
		t.Errorf("temperature = %v, want ausente (a versão da API recusa)", *got.Temperature)
	}
	if got.MaxTokens == nil || *got.MaxTokens != 7 {
		t.Errorf("max_tokens = %v, want 7", got.MaxTokens)
	}

	if resp.Content != "olá" || resp.FinishReason != "length" || resp.Model != "claude-x" {
		t.Errorf("resposta = %+v, want olá/length/claude-x", resp)
	}
	// 1 + 25 de cache lido + 4 de cache criado.
	if resp.Usage.PromptTokens != 30 || resp.Usage.CompletionTokens != 5 {
		t.Errorf("Usage = %+v, want 30 de entrada e 5 de saída", resp.Usage)
	}
}

// Verifica o valor padrão de max_tokens. Clientes no formato da OpenAI costumam
// omitir o campo, e a Anthropic recusa o pedido sem ele: sem o padrão, todo pedido
// desses viraria um 400.
func TestChatUsaMaxTokensPadraoQuandoAusente(t *testing.T) {
	var got sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		_, _ = w.Write([]byte(`{"content": [], "stop_reason": "end_turn", "usage": {}}`))
	}))
	defer srv.Close()

	if _, err := anthropic.New(srv.URL, "k", srv.Client()).Chat(t.Context(), provider.ChatRequest{}); err != nil {
		t.Fatalf("erro inesperado: %v", err)
	}
	if got.MaxTokens == nil || *got.MaxTokens <= 0 {
		t.Errorf("max_tokens = %v, want um valor positivo", got.MaxTokens)
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
		// O 529 é da Anthropic: sobrecarga. Precisa acionar o fallback.
		{name: "sobrecarregado", status: 529, want: provider.ErrUnavailable},
		{name: "chave inválida", status: http.StatusUnauthorized, want: nil},
		{name: "pedido inválido", status: http.StatusBadRequest, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(`{"type": "error", "error": {"type": "x", "message": "detalhe do erro"}}`))
			}))
			defer srv.Close()

			_, err := anthropic.New(srv.URL, "k", srv.Client()).Chat(t.Context(), provider.ChatRequest{})
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

func readAll(t *testing.T, stream provider.StreamReader) []provider.StreamChunk {
	t.Helper()
	var chunks []provider.StreamChunk
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return chunks
		}
		if err != nil {
			t.Fatalf("Recv: erro inesperado: %v", err)
		}
		chunks = append(chunks, chunk)
	}
}

// Verifica a montagem do stream a partir dos eventos da Anthropic. A sequência é a
// que o Ollama, imitando a Anthropic, mandou de verdade, mais um ping, um delta de
// thinking e tokens de cache, que a Anthropic real manda. O ponto principal: o uso
// do message_start é parcial (7), e o do message_delta é o total (34); ficar com o
// primeiro cobraria a entrada errada.
func TestChatStreamUsaTotaisDoMessageDelta(t *testing.T) {
	var got sentRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			"event: message_start\n" +
				`data: {"type":"message_start","message":{"model":"claude-x","usage":{"input_tokens":7,"cache_read_input_tokens":10,"output_tokens":0}}}` + "\n\n" +
				"event: ping\n" + `data: {"type":"ping"}` + "\n\n" +
				"event: content_block_start\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}` + "\n\n" +
				"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}` + "\n\n" +
				"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"o"}}` + "\n\n" +
				"event: content_block_delta\n" + `data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"lá"}}` + "\n\n" +
				"event: content_block_stop\n" + `data: {"type":"content_block_stop","index":1}` + "\n\n" +
				"event: message_delta\n" + `data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"input_tokens":34,"output_tokens":3}}` + "\n\n" +
				"event: message_stop\n" + `data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer srv.Close()

	stream, err := anthropic.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()
	chunks := readAll(t, stream)

	if !got.Stream {
		t.Error("stream = false, want true")
	}
	// O thinking_delta não pode virar pedaço: o cliente veria o raciocínio como resposta.
	if len(chunks) != 3 || chunks[0].Content+chunks[1].Content != "olá" {
		t.Fatalf("pedaços = %+v, want o, lá e o final", chunks)
	}
	last := chunks[2]
	// 34 do message_delta, que substitui o 7, mais os 10 de cache do message_start,
	// que o message_delta não repetiu e por isso não pode apagar.
	if last.FinishReason != "length" || last.Usage == nil || last.Usage.PromptTokens != 44 || last.Usage.CompletionTokens != 3 {
		t.Errorf("último pedaço = %+v (uso %+v), want length com 44 e 3 tokens", last, last.Usage)
	}
}

func TestChatStreamTrataCorteAntesDoMessageStopComoIndisponivel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"o"}}` + "\n\n"))
	}))
	defer srv.Close()

	stream, err := anthropic.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
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
		// O message_stop depois do erro prova que o adaptador lê o evento de erro,
		// e não só falha pelo corte (a lição do teste equivalente da OpenAI).
		_, _ = w.Write([]byte(
			"event: error\n" + `data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n" +
				`data: {"type":"message_stop"}` + "\n\n"))
	}))
	defer srv.Close()

	stream, err := anthropic.New(srv.URL, "k", srv.Client()).ChatStream(t.Context(), provider.ChatRequest{})
	if err != nil {
		t.Fatalf("ChatStream: erro inesperado: %v", err)
	}
	defer func() { _ = stream.Close() }()

	if _, err := stream.Recv(); !errors.Is(err, provider.ErrUnavailable) {
		t.Errorf("Recv: erro = %v, want ErrUnavailable", err)
	}
}
