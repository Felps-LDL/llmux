package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/api"
	"github.com/Felps-LDL/llmux/internal/cache"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/ratelimit"
	"github.com/Felps-LDL/llmux/internal/router"
)

// fakeCache é um ResponseCache sobre um map, com um erro opcional em toda leitura.
type fakeCache struct {
	getErr error

	mu      sync.Mutex
	entries map[string]cache.Entry
	gets    int
	sets    int
}

func newFakeCache() *fakeCache { return &fakeCache{entries: map[string]cache.Entry{}} }

func (c *fakeCache) Get(_ context.Context, key string) (cache.Entry, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gets++
	if c.getErr != nil {
		return cache.Entry{}, false, c.getErr
	}
	e, ok := c.entries[key]
	return e, ok, nil
}

func (c *fakeCache) Set(_ context.Context, key string, e cache.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sets++
	c.entries[key] = e
	return nil
}

func newCachedHandler(p provider.Provider, c api.ResponseCache, l api.Limiter) http.Handler {
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: p, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	return api.New(api.Options{
		Router:        r,
		Keys:          keys{testKey: {ID: 1}},
		Limiter:       l,
		DefaultLimits: defaultLimits,
		Cache:         c,
	}).Handler()
}

func content(t *testing.T, body []byte) string {
	t.Helper()
	var got struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &got); err != nil || len(got.Choices) != 1 {
		t.Fatalf("corpo da resposta: %v (%s)", err, body)
	}
	return got.Choices[0].Message.Content
}

// Verifica o ciclo do cache pelo caminho real da API: o primeiro pedido é um miss,
// chama o provedor e grava; o segundo, idêntico, é um acerto e não chama o provedor.
// O roteiro do falso tem uma resposta só: se o segundo pedido chegasse ao provedor,
// receberia ErrScriptExhausted, e o teste falharia. O acerto também precisa devolver
// os tokens reservados e informar custo zero, porque nada foi gasto no provedor.
func TestCacheResponde2oPedidoIgualSemChamarOProvedor(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá", FinishReason: "stop", Usage: provider.Usage{PromptTokens: 3, CompletionTokens: 2}}))
	c := newFakeCache()
	l := &fakeLimiter{decision: ratelimit.Decision{Allowed: true}}
	h := newCachedHandler(a, c, l)
	body := `{"model": "fast", "temperature": 0, "messages": [{"role": "user", "content": "oi"}]}`

	first := post(t.Context(), t, h, body)
	if first.Code != http.StatusOK || first.Header().Get("X-Llmux-Cache") != "miss" {
		t.Fatalf("primeiro pedido = %d, cache %q, want 200 e miss", first.Code, first.Header().Get("X-Llmux-Cache"))
	}

	second := post(t.Context(), t, h, body)
	if second.Code != http.StatusOK || second.Header().Get("X-Llmux-Cache") != "hit" {
		t.Fatalf("segundo pedido = %d, cache %q, want 200 e hit (corpo: %s)", second.Code, second.Header().Get("X-Llmux-Cache"), second.Body)
	}
	if got := content(t, second.Body.Bytes()); got != "olá" {
		t.Errorf("conteúdo do acerto = %q, want %q", got, "olá")
	}
	if got := second.Header().Get("X-Llmux-Cost-Picodollars"); got != "0" {
		t.Errorf("custo do acerto = %q, want %q", got, "0")
	}
	if got := second.Header().Get("X-Llmux-Provider"); got != "a" {
		t.Errorf("provedor do acerto = %q, want %q (o da resposta guardada)", got, "a")
	}
	if n := len(a.Calls()); n != 1 {
		t.Errorf("chamadas ao provedor = %d, want 1", n)
	}
	// O primeiro acerta com o uso real (5); o segundo devolve tudo (0).
	if len(l.settles) != 2 || l.settles[1].used != 0 {
		t.Errorf("acertos do rate limit = %+v, want o segundo com used 0", l.settles)
	}
}

// Verifica quando o cache não pode ser usado. Com temperatura acima de zero, ou
// sem temperatura (o padrão do provedor), o cliente espera variedade; o streaming
// ainda não tem cache. Nos três casos, nada pode ser lido nem gravado, senão um
// cliente receberia uma resposta repetida que não pediu.
func TestCacheNaoEUsadoSemTemperaturaZero(t *testing.T) {
	tests := []struct {
		name string
		body string
		step fake.Step
	}{
		{name: "sem temperatura", body: `{"model": "fast", "messages": [{"role": "user", "content": "oi"}]}`, step: fake.Reply(provider.ChatResponse{Content: "olá"})},
		{name: "temperatura 0.7", body: `{"model": "fast", "temperature": 0.7, "messages": [{"role": "user", "content": "oi"}]}`, step: fake.Reply(provider.ChatResponse{Content: "olá"})},
		{name: "streaming", body: `{"model": "fast", "stream": true, "temperature": 0, "messages": [{"role": "user", "content": "oi"}]}`, step: fake.StreamReply(provider.StreamChunk{Content: "olá"})},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newFakeCache()
			rec := post(t.Context(), t, newCachedHandler(fake.New("a", tt.step), c, nil), tt.body)

			if got := rec.Header().Get("X-Llmux-Cache"); got != "bypass" {
				t.Errorf("X-Llmux-Cache = %q, want bypass", got)
			}
			if c.gets != 0 || c.sets != 0 {
				t.Errorf("cache usado: %d leituras e %d gravações, want 0", c.gets, c.sets)
			}
		})
	}
}

func TestCacheSegueSemCacheQuandoORedisFalha(t *testing.T) {
	a := fake.New("a", fake.Reply(provider.ChatResponse{Content: "olá"}))
	c := newFakeCache()
	c.getErr = errors.New("redis: conexão recusada")

	rec := post(t.Context(), t, newCachedHandler(a, c, nil),
		`{"model": "fast", "temperature": 0, "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (corpo: %s)", rec.Code, rec.Body)
	}
	if n := len(a.Calls()); n != 1 {
		t.Errorf("chamadas ao provedor = %d, want 1", n)
	}
}

// Uma falha do provedor não pode ser gravada: o próximo pedido igual receberia o
// erro do cache, em vez de uma nova tentativa.
func TestCacheNaoGravaFalha(t *testing.T) {
	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	c := newFakeCache()

	rec := post(t.Context(), t, newCachedHandler(a, c, nil),
		`{"model": "fast", "temperature": 0, "messages": [{"role": "user", "content": "oi"}]}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if c.sets != 0 {
		t.Errorf("gravações = %d, want 0", c.sets)
	}
}

// As chaves de API não dividem cache: o mesmo pedido de outra chave é um miss.
func TestCacheSeparaChavesDeAPI(t *testing.T) {
	a := fake.New("a",
		fake.Reply(provider.ChatResponse{Content: "para a chave 1"}),
		fake.Reply(provider.ChatResponse{Content: "para a chave 2"}),
	)
	c := newFakeCache()
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-fisico"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	h := api.New(api.Options{
		Router: r,
		Keys:   keys{testKey: {ID: 1}, "llmux_outra": {ID: 2}},
		Cache:  c,
	}).Handler()
	body := `{"model": "fast", "temperature": 0, "messages": [{"role": "user", "content": "oi"}]}`

	post(t.Context(), t, h, body)

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer llmux_outra")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if got := rec.Header().Get("X-Llmux-Cache"); got != "miss" {
		t.Errorf("outra chave: X-Llmux-Cache = %q, want miss", got)
	}
	if got := content(t, rec.Body.Bytes()); got != "para a chave 2" {
		t.Errorf("outra chave recebeu %q, want a própria resposta", got)
	}
}
