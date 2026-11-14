// Package api expõe o gateway com a API HTTP compatível com a da OpenAI.
package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/Felps-LDL/llmux/internal/cache"
	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/ratelimit"
	"github.com/Felps-LDL/llmux/internal/router"
	"github.com/Felps-LDL/llmux/internal/telemetry"
	"github.com/Felps-LDL/llmux/internal/usage"
)

// Sem limite, um único pedido poderia esgotar a memória do gateway. O valor é folgado
// porque um prompt com contexto longo é um pedido legítimo.
const maxBodyBytes = 10 << 20

const costHeader = "X-Llmux-Cost-Picodollars"

// Server atende a API HTTP do gateway.
type Server struct {
	router        *router.Router
	prices        usage.PriceTable
	keys          KeyLookup
	limiter       Limiter
	defaultLimits ratelimit.Limits
	cache         ResponseCache
	metrics       *telemetry.Metrics
}

// Options são as dependências do Server. Uma struct, e não argumentos, porque
// cinco parâmetros posicionais são um erro de ordem esperando para acontecer.
type Options struct {
	Router *router.Router
	Prices usage.PriceTable
	Keys   KeyLookup
	// Limiter nil desliga o rate limit; DefaultLimits vale para as chaves sem
	// limites próprios.
	Limiter       Limiter
	DefaultLimits ratelimit.Limits
	// Cache nil desliga o cache de respostas.
	Cache ResponseCache
	// Metrics nil não mede nada.
	Metrics *telemetry.Metrics
}

// New cria um Server.
func New(o Options) *Server {
	return &Server{router: o.Router, prices: o.Prices, keys: o.Keys, limiter: o.Limiter, defaultLimits: o.DefaultLimits, cache: o.Cache, metrics: o.Metrics}
}

// Handler devolve o http.Handler com as rotas da API.
func (s *Server) Handler() http.Handler {
	r := chi.NewRouter()
	// O id vem antes do access log, para a linha do pedido levar o id; e os dois
	// valem para todas as rotas, inclusive as que a autenticação recusa.
	r.Use(requestID, s.accessLog)
	// Num grupo, para toda rota nova sob /v1 nascer protegida, em vez de depender
	// de alguém lembrar do middleware.
	r.Group(func(r chi.Router) {
		r.Use(s.authenticate)
		r.Post(chatPath, s.chatCompletions)
	})
	// Por fora de tudo, para o id, o access log e a autenticação ficarem dentro do
	// trace. O traceparent de quem chama é lido aqui.
	return otelhttp.NewHandler(r, "http.server", otelhttp.WithSpanNameFormatter(spanName))
}

const chatPath = "/v1/chat/completions"

// spanName dá nome ao span do pedido. As ferramentas de trace agrupam por nome, e o
// caminho cru criaria um nome por URL inventada: só as rotas conhecidas dão nome.
func spanName(_ string, r *http.Request) string {
	if r.URL.Path == chatPath {
		return r.Method + " " + chatPath
	}
	return r.Method + " rota desconhecida"
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	// Ponteiro pelo motivo documentado em provider.ChatRequest.
	Temperature *float64 `json:"temperature"`
	MaxTokens   int      `json:"max_tokens"`
	// Os SDKs atuais também oferecem este nome para o mesmo limite. Sem ele, o limite
	// seria ignorado sem aviso, e o cliente pagaria por uma resposta inteira.
	MaxCompletionTokens int           `json:"max_completion_tokens"`
	Stream              bool          `json:"stream"`
	StreamOptions       streamOptions `json:"stream_options"`
}

type streamOptions struct {
	// Na API da OpenAI, o uso só vai no stream se o cliente pedir; um pedaço extra
	// sem choices poderia quebrar clientes que não o esperam.
	IncludeUsage bool `json:"include_usage"`
}

type chatChoice struct {
	Index        int         `json:"index"`
	Message      chatMessage `json:"message"`
	FinishReason string      `json:"finish_reason"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type chatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []chatChoice `json:"choices"`
	Usage   chatUsage    `json:"usage"`
}

type errorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
}

type errorResponse struct {
	Error errorDetail `json:"error"`
}

func (s *Server) chatCompletions(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	// Campos desconhecidos são ignorados de propósito: os SDKs da OpenAI enviam
	// vários, e recusá-los quebraria a adoção trocando só a base_url.
	var in chatRequest
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request_error", "corpo do pedido grande demais")
			return
		}
		writeError(w, http.StatusBadRequest, "invalid_request_error", "corpo do pedido não é um JSON válido")
		return
	}

	req, err := toProviderRequest(in)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	info := infoFrom(r.Context())
	info.model = in.Model

	rsv, ok := s.reserve(w, r, req)
	if !ok {
		return
	}

	if in.Stream {
		info.setCache(w, "bypass")
		s.streamCompletions(w, r, in, req, rsv)
		return
	}

	key, cacheable := s.cacheKey(r.Context(), in.Model, req)
	if cacheable {
		if entry, hit := s.cacheGet(r.Context(), key); hit {
			// O provedor não foi chamado: os tokens reservados voltam inteiros, e o
			// custo deste pedido é zero.
			rsv.settle(r.Context(), 0)
			info.setCache(w, "hit")
			s.metrics.CacheHit()
			w.Header().Set(costHeader, "0")
			w.Header().Set("X-Llmux-Provider", entry.Provider)
			info.provider = entry.Provider
			writeCompletion(w, in.Model, entry.Response)
			return
		}
		info.setCache(w, "miss")
	} else {
		info.setCache(w, "bypass")
	}

	res, err := s.router.Chat(r.Context(), in.Model, req)
	if err != nil {
		// Nenhum provedor respondeu: a reserva volta inteira.
		rsv.settle(r.Context(), 0)
		writeRouterError(r.Context(), w, in.Model, err)
		return
	}
	rsv.settle(r.Context(), res.Response.Usage.PromptTokens+res.Response.Usage.CompletionTokens)

	s.recordUsage(r.Context(), w, res.Provider, res.Model, res.Response.Usage)
	w.Header().Set("X-Llmux-Provider", res.Provider)
	info.provider = res.Provider
	writeCompletion(w, in.Model, *res.Response)

	// Depois de responder: o cliente não precisa esperar a gravação.
	if cacheable {
		s.cacheSet(r.Context(), key, cache.Entry{Provider: res.Provider, Model: res.Model, Response: *res.Response})
	}
}

// writeCompletion escreve uma resposta no formato da OpenAI. É a mesma para uma
// resposta do provedor e para um acerto do cache: duas cópias divergiriam.
func writeCompletion(w http.ResponseWriter, model string, resp provider.ChatResponse) {
	u := resp.Usage
	writeJSON(w, http.StatusOK, chatResponse{
		ID:      "chatcmpl-" + rand.Text(),
		Object:  "chat.completion",
		Created: time.Now().Unix(),
		// O modelo lógico que o cliente pediu: o nome físico é detalhe interno, que
		// pode mudar por config sem o cliente perceber.
		Model: model,
		Choices: []chatChoice{{
			Message:      chatMessage{Role: string(provider.RoleAssistant), Content: resp.Content},
			FinishReason: resp.FinishReason,
		}},
		Usage: chatUsage{
			PromptTokens:     u.PromptTokens,
			CompletionTokens: u.CompletionTokens,
			TotalTokens:      u.PromptTokens + u.CompletionTokens,
		},
	})
}

// recordUsage põe o custo no cabeçalho de custo (no streaming, ele vai como trailer,
// declarado antes do corpo) e mede os tokens e o custo. O provedor já respondeu e
// cobrou: um erro de preço não pode custar a resposta ao cliente. A carga da config
// impede que isso aconteça em produção.
func (s *Server) recordUsage(ctx context.Context, w http.ResponseWriter, providerName, model string, u provider.Usage) {
	cost, err := s.prices.Cost(providerName, model, u)
	if err != nil {
		slog.ErrorContext(ctx, "calculando custo", "error", err)
		return
	}
	w.Header().Set(costHeader, strconv.FormatInt(int64(cost), 10))
	s.metrics.ObserveUsage(providerName, model, keyFrom(ctx).ID, u.PromptTokens, u.CompletionTokens, int64(cost))
}

func toProviderRequest(in chatRequest) (provider.ChatRequest, error) {
	if in.Model == "" {
		return provider.ChatRequest{}, errors.New("model é obrigatório")
	}
	if len(in.Messages) == 0 {
		return provider.ChatRequest{}, errors.New("messages é obrigatório")
	}
	if in.MaxTokens < 0 || in.MaxCompletionTokens < 0 {
		return provider.ChatRequest{}, errors.New("o limite de tokens não pode ser negativo")
	}
	maxTokens := in.MaxTokens
	if in.MaxCompletionTokens > 0 {
		maxTokens = in.MaxCompletionTokens
	}

	messages := make([]provider.Message, len(in.Messages))
	for i, m := range in.Messages {
		// É aqui que um papel inválido é recusado: o tipo provider.Role não impede
		// um valor qualquer vindo do JSON.
		role := provider.Role(m.Role)
		switch role {
		case provider.RoleSystem, provider.RoleUser, provider.RoleAssistant:
		default:
			return provider.ChatRequest{}, fmt.Errorf("messages[%d]: role %q inválido", i, m.Role)
		}
		messages[i] = provider.Message{Role: role, Content: m.Content}
	}

	return provider.ChatRequest{
		Messages:    messages,
		Temperature: in.Temperature,
		MaxTokens:   maxTokens,
	}, nil
}

// writeRouterError traduz o erro do router para HTTP. O detalhe fica no log e não vai
// ao cliente, porque a mensagem de um provedor pode expor nomes internos da config.
func writeRouterError(ctx context.Context, w http.ResponseWriter, model string, err error) {
	// O cliente já foi embora: não há para quem responder.
	if errors.Is(ctx.Err(), context.Canceled) {
		return
	}

	slog.WarnContext(ctx, "nenhum provedor atendeu", "error", err)

	switch {
	case errors.Is(err, router.ErrUnknownModel):
		writeError(w, http.StatusNotFound, "invalid_request_error", fmt.Sprintf("modelo %q não existe", model))
	case errors.Is(err, router.ErrCircuitOpen), errors.Is(err, provider.ErrUnavailable):
		writeError(w, http.StatusServiceUnavailable, "server_error", "provedores indisponíveis")
	case errors.Is(err, provider.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "server_error", "tempo esgotado ao chamar os provedores")
	case errors.Is(err, provider.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "limite dos provedores excedido")
	default:
		// Inclui o 400 e o 401 de um provedor: sem classe, o gateway não sabe se a
		// culpa é do pedido ou da config dele, então responde que o provedor recusou.
		writeError(w, http.StatusBadGateway, "server_error", "o provedor recusou o pedido")
	}
}

func writeError(w http.ResponseWriter, status int, errType, message string) {
	writeJSON(w, status, errorResponse{Error: errorDetail{Message: message, Type: errType}})
}

// writeJSON recebe any porque é o que json.Encoder.Encode aceita, e o corpo tem tipos
// diferentes para sucesso e erro.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Sem contexto, e por isso sem request_id: writeJSON é chamada de muitos
		// lugares, e uma falha de escrita quase sempre é o cliente que já saiu.
		slog.Warn("escrevendo resposta", "error", err)
	}
}
