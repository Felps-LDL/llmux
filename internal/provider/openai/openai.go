// Package openai implementa provider.Provider sobre a API de chat completions da
// OpenAI, e de qualquer servidor que a imite.
package openai

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
	// O corpo de um erro só serve para a mensagem; sem limite, um servidor com
	// defeito poderia fazer o gateway guardar uma resposta enorme na memória.
	maxErrorBody = 4 << 10
	// Um evento do stream é um pedaço pequeno de JSON. O limite só existe para um
	// servidor com defeito não fazer o Scanner crescer sem fim.
	maxEventBytes = 1 << 20
)

// Client é um provedor que chama a API da OpenAI.
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// New cria um Client. baseURL inclui o /v1, como nos SDKs da OpenAI. O httpClient não
// deve ter Timeout, pelo mesmo motivo do adaptador do Ollama: o prazo vem do contexto.
func New(baseURL, apiKey string, httpClient *http.Client) *Client {
	return &Client{baseURL: baseURL, apiKey: apiKey, http: httpClient}
}

// Name devolve o nome do provedor, usado no breaker e na tabela de preços.
func (c *Client) Name() string {
	return "openai"
}

type message struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	// Ponteiro pelo motivo documentado em provider.ChatRequest.
	Temperature *float64 `json:"temperature,omitempty"`
	// max_completion_tokens, e não max_tokens: os modelos de raciocínio da OpenAI
	// recusam o nome antigo.
	MaxCompletionTokens int  `json:"max_completion_tokens,omitempty"`
	Stream              bool `json:"stream,omitempty"`
	// Sem include_usage, a OpenAI não manda o uso no stream, e não haveria custo.
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
}

type apiError struct {
	Message string `json:"message"`
}

// chatResponse serve para a resposta inteira e para cada evento do stream: a
// resposta traz message, e os eventos trazem delta.
type chatResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message      message `json:"message"`
		Delta        message `json:"delta"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *usage    `json:"usage"`
	Error *apiError `json:"error"`
}

// Chat envia o pedido e traduz a resposta e os erros para o nosso contrato.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body, err := c.send(ctx, req, false)
	if err != nil {
		return nil, err
	}
	// O erro de fechar é ignorado pelo mesmo motivo do adaptador do Ollama.
	defer func() { _ = body.Close() }()

	var out chatResponse
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, provider.ClassifyTransportError(err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("%s: %w", out.Error.Message, provider.ErrUnavailable)
	}
	if len(out.Choices) == 0 {
		return nil, fmt.Errorf("resposta sem choices: %w", provider.ErrUnavailable)
	}

	resp := &provider.ChatResponse{
		Model:   out.Model,
		Content: out.Choices[0].Message.Content,
		// O vocabulário da OpenAI já é o da nossa API.
		FinishReason: out.Choices[0].FinishReason,
	}
	if out.Usage != nil {
		resp.Usage = provider.Usage{PromptTokens: out.Usage.PromptTokens, CompletionTokens: out.Usage.CompletionTokens}
	}
	return resp, nil
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
	messages := make([]message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = message{Role: string(m.Role), Content: m.Content}
	}

	in := chatRequest{
		Model:               req.Model,
		Messages:            messages,
		Temperature:         req.Temperature,
		MaxCompletionTokens: req.MaxTokens,
	}
	if stream {
		in.Stream = true
		in.StreamOptions = &streamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("codificando pedido: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("criando pedido: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.ClassifyTransportError(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, provider.StatusError(resp.StatusCode, errorDetail(resp.Body))
	}
	return resp.Body, nil
}

// errorDetail extrai a mensagem do corpo de erro da OpenAI, que fica em
// error.message. A mensagem nunca leva a chave: a OpenAI só mostra o final dela.
func errorDetail(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, maxErrorBody))
	if err != nil {
		return "corpo ilegível"
	}
	var parsed chatResponse
	if json.Unmarshal(data, &parsed) == nil && parsed.Error != nil && parsed.Error.Message != "" {
		return parsed.Error.Message
	}
	return string(data)
}

type streamReader struct {
	body    io.ReadCloser
	scanner *bufio.Scanner
	// final é o pedaço com finish_reason, guardado até o uso chegar: a OpenAI os
	// manda em eventos separados, e o nosso contrato é um último pedaço com os dois.
	final *provider.StreamChunk
	done  bool
}

func (r *streamReader) Recv() (provider.StreamChunk, error) {
	if r.done {
		return provider.StreamChunk{}, io.EOF
	}

	for r.scanner.Scan() {
		// Linhas vazias separam eventos, e as que começam com ":" são comentários de
		// keep-alive; nenhuma traz dado.
		data, ok := strings.CutPrefix(r.scanner.Text(), "data:")
		if !ok {
			continue
		}
		data = strings.TrimSpace(data)

		if data == "[DONE]" {
			r.done = true
			if r.final != nil {
				return *r.final, nil
			}
			return provider.StreamChunk{}, io.EOF
		}

		var ev chatResponse
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			return provider.StreamChunk{}, fmt.Errorf("evento malformado: %w: %w", provider.ErrUnavailable, err)
		}
		if ev.Error != nil {
			return provider.StreamChunk{}, fmt.Errorf("%s: %w", ev.Error.Message, provider.ErrUnavailable)
		}

		if ev.Usage != nil {
			if r.final == nil {
				r.final = &provider.StreamChunk{}
			}
			r.final.Usage = &provider.Usage{PromptTokens: ev.Usage.PromptTokens, CompletionTokens: ev.Usage.CompletionTokens}
		}
		if len(ev.Choices) == 0 {
			continue
		}

		chunk := provider.StreamChunk{Content: ev.Choices[0].Delta.Content, FinishReason: ev.Choices[0].FinishReason}
		if chunk.FinishReason == "" {
			return chunk, nil
		}
		if r.final != nil {
			chunk.Usage = r.final.Usage
		}
		r.final = &chunk
	}

	if err := r.scanner.Err(); err != nil {
		return provider.StreamChunk{}, provider.ClassifyTransportError(err)
	}
	// O corpo acabou sem [DONE]: resposta cortada, não terminada.
	return provider.StreamChunk{}, provider.ClassifyTransportError(io.ErrUnexpectedEOF)
}

// Close solta a conexão; sem ele, um cliente que desiste no meio deixaria o
// provedor gerando, e cobrando, tokens para ninguém.
func (r *streamReader) Close() error {
	return r.body.Close()
}
