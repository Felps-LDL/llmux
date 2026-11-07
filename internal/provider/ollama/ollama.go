// Package ollama implementa provider.Provider sobre a API HTTP do Ollama.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// O corpo de um erro só serve para a mensagem; sem limite, um servidor com defeito
// poderia fazer o gateway guardar uma resposta enorme na memória.
const maxErrorBody = 4 << 10

// Client é um provedor que chama um servidor Ollama.
type Client struct {
	baseURL string
	http    *http.Client
}

var _ provider.Provider = (*Client)(nil)

// New cria um Client. O httpClient não deve ter Timeout: o prazo de cada tentativa
// vem do contexto que o router passa, e dois limites competindo confundem o erro.
func New(baseURL string, httpClient *http.Client) *Client {
	return &Client{baseURL: baseURL, http: httpClient}
}

// Name devolve o nome do provedor, usado no breaker e na tabela de preços.
func (c *Client) Name() string {
	return "ollama"
}

type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type options struct {
	// Ponteiro pelo motivo documentado em provider.ChatRequest: com omitempty, um
	// float64 zero sumiria do JSON, e o Ollama aplicaria o default dele.
	Temperature *float64 `json:"temperature,omitempty"`
	NumPredict  int      `json:"num_predict,omitempty"`
}

type chatRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	// Sem omitempty: o padrão da API do Ollama é responder em streaming, então o
	// false precisa ir explícito no JSON.
	Stream  bool    `json:"stream"`
	Options options `json:"options"`
}

type chatResponse struct {
	Model           string  `json:"model"`
	Message         message `json:"message"`
	Done            bool    `json:"done"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	Error           string  `json:"error"`
}

// Chat envia o pedido ao Ollama e traduz a resposta e os erros para o nosso contrato.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	body, err := c.send(ctx, req, false)
	if err != nil {
		return nil, err
	}
	// O erro de fechar é ignorado de propósito: a esta altura, o corpo já foi lido ou
	// a chamada já falhou, e ele não mudaria o resultado.
	defer func() { _ = body.Close() }()

	var out chatResponse
	if err := json.NewDecoder(body).Decode(&out); err != nil {
		return nil, provider.ClassifyTransportError(err)
	}
	// Status 200 não garante sucesso: um corpo com "error" também é uma falha.
	if out.Error != "" {
		return nil, fmt.Errorf("%s: %w", out.Error, provider.ErrUnavailable)
	}

	return &provider.ChatResponse{
		Model:        out.Model,
		Content:      out.Message.Content,
		FinishReason: finishReason(out.DoneReason),
		Usage: provider.Usage{
			PromptTokens:     out.PromptEvalCount,
			CompletionTokens: out.EvalCount,
		},
	}, nil
}

// ChatStream envia o pedido em modo streaming. Só falhas antes do primeiro pedaço
// saem daqui; as do meio do stream aparecem em Recv.
func (c *Client) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.StreamReader, error) {
	body, err := c.send(ctx, req, true)
	if err != nil {
		return nil, err
	}
	return &streamReader{body: body, dec: json.NewDecoder(body)}, nil
}

// send monta e envia o pedido e devolve o corpo de uma resposta 200, que quem
// chama precisa fechar; em caso de erro, send já fechou. Devolve só o corpo porque
// o status já foi tratado aqui e nenhum chamador precisa do resto.
func (c *Client) send(ctx context.Context, req provider.ChatRequest, stream bool) (io.ReadCloser, error) {
	messages := make([]message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = message{Role: string(m.Role), Content: m.Content}
	}

	body, err := json.Marshal(chatRequest{
		Model:    req.Model,
		Messages: messages,
		Stream:   stream,
		Options:  options{Temperature: req.Temperature, NumPredict: req.MaxTokens},
	})
	if err != nil {
		return nil, fmt.Errorf("codificando pedido: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("criando pedido: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, provider.ClassifyTransportError(err)
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, statusError(resp)
	}
	return resp.Body, nil
}

type streamReader struct {
	body io.ReadCloser
	dec  *json.Decoder
	done bool
}

func (r *streamReader) Recv() (provider.StreamChunk, error) {
	if r.done {
		return provider.StreamChunk{}, io.EOF
	}

	var line chatResponse
	if err := r.dec.Decode(&line); err != nil {
		// O corpo acabar antes do done significa uma resposta cortada. Devolver io.EOF
		// aqui entregaria meia resposta ao cliente como se fosse completa.
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return provider.StreamChunk{}, provider.ClassifyTransportError(err)
	}
	if line.Error != "" {
		return provider.StreamChunk{}, fmt.Errorf("%s: %w", line.Error, provider.ErrUnavailable)
	}

	if !line.Done {
		return provider.StreamChunk{Content: line.Message.Content}, nil
	}
	r.done = true
	return provider.StreamChunk{
		Content:      line.Message.Content,
		FinishReason: finishReason(line.DoneReason),
		Usage: &provider.Usage{
			PromptTokens:     line.PromptEvalCount,
			CompletionTokens: line.EvalCount,
		},
	}, nil
}

// Close solta a conexão. Sem ele, um cliente que desiste no meio deixaria o Ollama
// gerando tokens para ninguém e a conexão presa fora do pool.
func (r *streamReader) Close() error {
	return r.body.Close()
}

func statusError(resp *http.Response) error {
	detail := "corpo ilegível"
	if data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody)); err == nil {
		var parsed struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &parsed) == nil && parsed.Error != "" {
			detail = parsed.Error
		} else {
			detail = string(data)
		}
	}

	return provider.StatusError(resp.StatusCode, detail)
}

// finishReason traduz para o vocabulário da OpenAI, que é o da nossa API. A
// documentação do Ollama não lista todos os valores, então o desconhecido vira
// "stop", o término normal.
func finishReason(doneReason string) string {
	if doneReason == "length" {
		return "length"
	}
	return "stop"
}
