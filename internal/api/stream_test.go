package api_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
)

const streamBody = `{"model": "fast", "stream": true, "messages": [{"role": "user", "content": "oi"}]`

// sseData devolve o conteúdo de cada linha "data:" do corpo, na ordem. É o que um
// cliente SSE faz antes de interpretar os eventos.
func sseData(t *testing.T, body string) []string {
	t.Helper()
	var events []string
	for line := range strings.SplitSeq(body, "\n") {
		if data, ok := strings.CutPrefix(line, "data: "); ok {
			events = append(events, data)
		}
	}
	return events
}

type streamChunk struct {
	Object  string `json:"object"`
	Model   string `json:"model"`
	Choices []struct {
		Delta struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		TotalTokens int `json:"total_tokens"`
	} `json:"usage"`
}

// Verifica o formato de streaming da OpenAI, que é o que o SDK do cliente sabe ler:
// o role só no primeiro pedaço, finish_reason null até o fim, o pedaço de uso só
// quando pedido, e o [DONE] por último. Verifica também o custo, que só existe no
// fim e por isso vai como trailer, e não como cabeçalho.
func TestChatCompletionsStreamSegueFormatoDaOpenAI(t *testing.T) {
	a := fake.New("a", fake.StreamReply(
		provider.StreamChunk{Content: "o"},
		provider.StreamChunk{Content: "lá"},
		provider.StreamChunk{FinishReason: "stop", Usage: &provider.Usage{PromptTokens: 1000, CompletionTokens: 500}},
	))
	body := streamBody + `, "stream_options": {"include_usage": true}}`
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (corpo: %s)", rec.Code, http.StatusOK, rec.Body)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", got)
	}
	// Prova só que houve flush, não que foi um por pedaço; a entrega aos poucos foi
	// conferida contra o Ollama real.
	if !rec.Flushed {
		t.Error("Flushed = false, want true: sem flush, o cliente recebe tudo só no fim")
	}

	events := sseData(t, rec.Body.String())
	// Três pedaços do provedor, um de uso e o [DONE].
	if len(events) != 5 {
		t.Fatalf("eventos = %d, want 5: %q", len(events), events)
	}
	if events[4] != "[DONE]" {
		t.Errorf("último evento = %q, want [DONE]", events[4])
	}

	chunks := make([]streamChunk, 4)
	for i := range chunks {
		if err := json.Unmarshal([]byte(events[i]), &chunks[i]); err != nil {
			t.Fatalf("evento %d: %v", i, err)
		}
		if chunks[i].Object != "chat.completion.chunk" || chunks[i].Model != "fast" {
			t.Errorf("evento %d: object/model = %q/%q, want chat.completion.chunk/fast", i, chunks[i].Object, chunks[i].Model)
		}
	}

	var content string
	for i, c := range chunks[:3] {
		if len(c.Choices) != 1 {
			t.Fatalf("pedaço %d: choices = %d, want 1", i, len(c.Choices))
		}
		content += c.Choices[0].Delta.Content
		if wantRole := i == 0; (c.Choices[0].Delta.Role == "assistant") != wantRole {
			t.Errorf("pedaço %d: role = %q, want assistant só no primeiro", i, c.Choices[0].Delta.Role)
		}
		if wantFinish := i == 2; (c.Choices[0].FinishReason != nil) != wantFinish {
			t.Errorf("pedaço %d: finish_reason = %v, want só no último", i, c.Choices[0].FinishReason)
		}
	}
	if content != "olá" {
		t.Errorf("conteúdo = %q, want %q", content, "olá")
	}
	if u := chunks[3]; len(u.Choices) != 0 || u.Usage == nil || u.Usage.TotalTokens != 1500 {
		t.Errorf("pedaço de uso = %+v, want sem choices e 1500 tokens", u)
	}

	// O mesmo cálculo do teste sem streaming: 1000 × 150.000 + 500 × 600.000.
	if got := rec.Result().Trailer.Get("X-Llmux-Cost-Picodollars"); got != "450000000" {
		t.Errorf("trailer de custo = %q, want %q", got, "450000000")
	}
}

// Verifica a falha depois do primeiro pedaço, quando o 200 já foi enviado e não
// há mais como mudar o status. O cliente precisa receber um evento de erro e não
// receber o [DONE]: sem isso, meia resposta pareceria uma resposta completa.
func TestChatCompletionsStreamSinalizaErroNoMeio(t *testing.T) {
	a := fake.New("a", fake.StreamFailAfter(provider.ErrUnavailable, provider.StreamChunk{Content: "o"}))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody+"}"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	events := sseData(t, rec.Body.String())
	if len(events) != 2 {
		t.Fatalf("eventos = %d, want 2 (o pedaço e o erro): %q", len(events), events)
	}
	var got struct {
		Error *struct {
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(events[1]), &got); err != nil || got.Error == nil {
		t.Fatalf("último evento = %q, want um objeto error", events[1])
	}
	if got.Error.Type != "server_error" {
		t.Errorf("error.type = %q, want server_error", got.Error.Type)
	}
	if a.Closes() != 1 {
		t.Errorf("Closes = %d, want 1", a.Closes())
	}
}

func TestChatCompletionsStreamUsaStatusParaErroAntesDoPrimeiroPedaco(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody+"}"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
}

func TestChatCompletionsStreamSoEnviaUsoQuandoPedido(t *testing.T) {
	a := fake.New("a", fake.StreamReply(
		provider.StreamChunk{Content: "olá", FinishReason: "stop", Usage: &provider.Usage{PromptTokens: 1, CompletionTokens: 1}},
	))
	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(streamBody+"}"))
	req.Header.Set("Authorization", "Bearer "+testKey)
	rec := httptest.NewRecorder()

	newHandler(a).ServeHTTP(rec, req)

	events := sseData(t, rec.Body.String())
	if len(events) != 2 || events[1] != "[DONE]" {
		t.Errorf("eventos = %q, want o pedaço e o [DONE], sem pedaço de uso", events)
	}
	// O custo não depende de o cliente pedir o uso.
	if got := rec.Result().Trailer.Get("X-Llmux-Cost-Picodollars"); got == "" {
		t.Error("trailer de custo vazio, want o custo mesmo sem include_usage")
	}
}
