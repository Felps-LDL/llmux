// Package anthropic implementa provider.Provider sobre a Messages API da Anthropic,
// e de qualquer servidor que a imite.
package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Felps-LDL/llmux/internal/provider"
)

const (
	// Nesta versão da API, temperature, top_p e top_k dão 400 em todos os modelos;
	// por isso a temperatura do cliente não é repassada.
	apiVersion = "2026-10-21"
	// max_tokens é obrigatório na Anthropic e opcional na API da OpenAI, que é a
	// que os clientes falam. O valor é o que a Anthropic recomenda para pedidos sem
	// streaming: alto o bastante para não cortar uma resposta normal.
	defaultMaxTokens = 16000
	// Mesmos motivos dos limites do adaptador da OpenAI.
	maxErrorBody  = 4 << 10
	maxEventBytes = 1 << 20
)

// Client é um provedor que chama a Messages API da Anthropic.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// New cria um Client. baseURL não inclui o /v1, como nos SDKs da Anthropic. O
// httpClient não deve ter Timeout: o prazo vem do contexto.
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: httpClient}
}

// Name devolve o nome do provedor, usado no breaker e na tabela de preços.
func (c *Client) Name() string {
	return "anthropic"
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type messagesRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Stream    bool      `json:"stream,omitempty"`
}

// usage soma os tokens de cache na entrada: com cache, input_tokens conta só a parte
// que não veio dele, e o custo sairia menor do que o real.
type usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
}

func (u usage) input() int {
	return u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
}

type apiError struct {
	Message string `json:"message"`
}

type contentBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type messagesResponse struct {
	Model      string         `json:"model"`
	Content    []contentBlock `json:"content"`
	StopReason string         `json:"stop_reason"`
	Usage      usage          `json:"usage"`
	Error      *apiError      `json:"error"`
}

// Chat envia o pedido e traduz a resposta e os erros para o nosso contrato.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body, err := c.send(ctx, req, false)
	if err != nil {
		return nil, err
	}
	// O erro de fechar é ignorado pelo mesmo motivo do adaptador do Ollama.
	defer func() { _ = body.Close() }()

	var out messagesResponse
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, provider.ClassifyTransportError(err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %w", out.Error.Message, provider.ErrUnavailable)
	}

	// Só os blocos de texto: os de thinking não fazem parte da resposta na API da
	// OpenAI, e o gateway não suporta ferramentas.
	var content strings.Builder
	for _, b := range out.Content {
		if b.Type == "text" {
			content.WriteString(b.Text)
		}
	}

	return &provider.ChatResponse{
		Model:        out.Model,
		Content:      content.String(),
		FinishReason: finishReason(out.StopReason),
		Usage:        provider.Usage{PromptTokens: out.Usage.input(), CompletionTokens: out.Usage.OutputTokens},
	}, nil
}

// ChatStream envia o pedido em modo streaming. Só falhas antes do primeiro pedaço
// saem daqui; as do meio do stream aparecem em Recv.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.StreamReader, error) {
	body, err := c.send(ctx, req, true)
	if err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxEventBytes)
	return &streamReader{body: body, scanner: scanner}, nil
}

// send monta e envia o pedido e devolve o corpo de uma resposta 200, que quem
// chama precisa fechar; em caso de erro, send já fechou.
func (c *Client) send(ctx context.Context, req provider.ChatRequest, stream bool) (io.ReadCloser, error) {
	in := messagesRequest{Model: req.Model, MaxTokens: req.MaxTokens, Stream: stream}
	if in.MaxTokens == 0 {
		in.MaxTokens = defaultMaxTokens
	}
	// A Anthropic recebe as instruções num campo próprio, e não como mensagem.
	var system []string
	for _, m := range req.Messages {
		if m.Role == provider.RoleSystem {
			system = append(system, m.Content)
			continue
		}
		in.Messages = append(in.Messages, message{Role: string(m.Role), Content: m.Content})
	}
	in.System = strings.Join(system, "\n\n")

	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("codificando pedido: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("criando pedido: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("X-Api-Key", c.apiKey)
	httpReq.Header.Set("Anthropic-Version", apiVersion)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.ClassifyTransportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		// O 529 (sobrecarga), exclusivo da Anthropic, cai em >= 500 e vira
		// ErrUnavailable, que é o certo: o provedor está vivo, mas sem capacidade.
		return nil, provider.StatusError(resp.StatusCode, errorDetail(resp.Body))
	}
	return resp.Body, nil
}

func errorDetail(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return "corpo ilegível"
	}
	var parsed messagesResponse
	if json.Unmarshal(data, &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return string(data)
}

// finishReason traduz para o vocabulário da OpenAI, que é o da nossa API.
func finishReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "refusal":
		return "content_filter"
	default:
		return "stop"
	}
}

// streamEvent cobre os eventos do stream que o adaptador usa. O tipo vem no próprio
// JSON, então as linhas "event:" do SSE não precisam ser lidas.
type streamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage usage `json:"usage"`
	} `json:"message"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usage    `json:"usage"`
	Error *apiError `json:"error"`
}

type streamReader struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	// O uso chega em partes: a entrada no message_start e os totais no
	// message_delta, que são cumulativos e por isso substituem os anteriores.
	usage      usage
	stopReason string
	done       bool
}

func (r *streamReader) Recv() (provider.StreamChunk, error) {
	if r.done {
		return provider.StreamChunk{}, io.EOF
	}

	for r.scanner.Scan() {
		data, ok := strings.CutPrefix(r.scanner.Text(), "data:")
		if !ok {
			continue
		}

		var ev streamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &ev); err != nil {
			return provider.StreamChunk{}, fmt.Errorf("evento malformado: %w: %w", provider.ErrUnavailable, err)
		}

		switch ev.Type {
		case "message_start":
			r.usage = ev.Message.Usage
		case "content_block_delta":
			// Os deltas de thinking ficam de fora, como os blocos em Chat.
			if ev.Delta.Type == "text_delta" {
				return provider.StreamChunk{Content: ev.Delta.Text}, nil
			}
		case "message_delta":
			r.stopReason = ev.Delta.StopReason
			if ev.Usage != nil {
				r.mergeUsage(*ev.Usage)
			}
		case "message_stop":
			r.done = true
			return provider.StreamChunk{
				FinishReason: finishReason(r.stopReason),
				Usage:        &provider.Usage{PromptTokens: r.usage.input(), CompletionTokens: r.usage.OutputTokens},
			}, nil
		case "error":
			msg := "erro sem mensagem"
			if ev.Error != nil {
				msg = ev.Error.Message
			}
			return provider.StreamChunk{}, fmt.Errorf("%s: %w", msg, provider.ErrUnavailable)
		}
		// ping, content_block_start e content_block_stop não trazem nada que o
		// contrato do stream use.
	}

	if err := r.scanner.Err(); err != nil {
		return provider.StreamChunk{}, provider.ClassifyTransportError(err)
	}
	// O corpo acabou sem message_stop: resposta cortada, não terminada.
	return provider.StreamChunk{}, provider.ClassifyTransportError(io.ErrUnexpectedEOF)
}

// mergeUsage aplica os totais do message_delta. Um campo ausente nele chega como
// zero e não pode apagar o valor do message_start; as contagens só crescem.
func (r *streamReader) mergeUsage(u usage) {
	r.usage.InputTokens = max(r.usage.InputTokens, u.InputTokens)
	r.usage.CacheCreationInputTokens = max(r.usage.CacheCreationInputTokens, u.CacheCreationInputTokens)
	r.usage.CacheReadInputTokens = max(r.usage.CacheReadInputTokens, u.CacheReadInputTokens)
	r.usage.OutputTokens = max(r.usage.OutputTokens, u.OutputTokens)
}

// Close solta a conexão; sem ele, um cliente que desiste no meio deixaria o
// provedor gerando, e cobrando, tokens para ninguém.
func (r *streamReader) Close() error {
	return r.body.Close()
}
