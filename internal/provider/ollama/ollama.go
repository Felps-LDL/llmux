// Package ollama implementa provider.Provider sobre a API HTTP do Ollama.
package ollama

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
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
	// Explícito porque o padrão da API do Ollama é responder em streaming.
	Stream  bool    `json:"stream"`
	Options options `json:"options"`
}

type chatResponse struct {
	Model           string  `json:"model"`
	Message         message `json:"message"`
	DoneReason      string  `json:"done_reason"`
	PromptEvalCount int     `json:"prompt_eval_count"`
	EvalCount       int     `json:"eval_count"`
	Error           string  `json:"error"`
}

// Chat envia o pedido ao Ollama e traduz a resposta e os erros para o nosso contrato.
func (c *Client) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	messages := make([]message, len(req.Messages))
	for i, m := range req.Messages {
		messages[i] = message{Role: string(m.Role), Content: m.Content}
	}

	body, err := json.Marshal(chatRequest{
		Model:    req.Model,
		Messages: messages,
		Options:  options{Temperature: req.Temperature, NumPredict: req.MaxTokens},
	})
	if err != nil {
		return nil, fmt.Errorf("ollama: codificando pedido: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/api/chat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("ollama: criando pedido: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, classifyTransportError(err)
	}
	// O erro de fechar é ignorado de propósito: a esta altura, o corpo já foi lido ou
	// a chamada já falhou, e ele não mudaria o resultado.
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}

	var out chatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, classifyTransportError(err)
	}
	// Status 200 não garante sucesso: um corpo com "error" também é uma falha.
	if out.Error != "" {
		return nil, fmt.Errorf("ollama: %s: %w", out.Error, provider.ErrUnavailable)
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

// classifyTransportError trata falhas sem status HTTP: de conexão, de prazo ou de
// uma resposta que chegou cortada ou malformada.
func classifyTransportError(err error) error {
	// Fica sem classe: quem desistiu foi o cliente, não o provedor.
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("ollama: %w", err)
	}

	// O timeout do transporte nem sempre aparece como context.DeadlineExceeded.
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("ollama: %w: %w", provider.ErrTimeout, err)
	}

	return fmt.Errorf("ollama: %w: %w", provider.ErrUnavailable, err)
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

	switch {
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("ollama: status %d: %s: %w", resp.StatusCode, detail, provider.ErrRateLimited)
	case resp.StatusCode >= http.StatusInternalServerError:
		return fmt.Errorf("ollama: status %d: %s: %w", resp.StatusCode, detail, provider.ErrUnavailable)
	default:
		// Sem classe: um 400 ou um 404 de modelo inexistente falharia igual em
		// qualquer nova tentativa e não diz nada sobre a saúde do provedor.
		return fmt.Errorf("ollama: status %d: %s", resp.StatusCode, detail)
	}
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
