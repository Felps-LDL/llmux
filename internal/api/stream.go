package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
)

type chunkDelta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

type chunkChoice struct {
	Index int        `json:"index"`
	Delta chunkDelta `json:"delta"`
	// Ponteiro porque a OpenAI manda null até o último pedaço, e os SDKs leem
	// qualquer valor não nulo como o fim da resposta.
	FinishReason *string `json:"finish_reason"`
}

type chatChunk struct {
	ID      string        `json:"id"`
	Object  string        `json:"object"`
	Created int64         `json:"created"`
	Model   string        `json:"model"`
	Choices []chunkChoice `json:"choices"`
	Usage   *chatUsage    `json:"usage,omitempty"`
}

// streamCompletions responde em SSE, no formato de streaming da OpenAI. Erros antes
// do primeiro pedaço saem como no modo sem streaming, com status; depois dele, o
// 200 já foi enviado, e o erro vai como um evento.
func (s *Server) streamCompletions(w http.ResponseWriter, r *http.Request, in chatRequest, req provider.ChatRequest, rsv reservation) {
	res, err := s.router.ChatStream(r.Context(), in.Model, req)
	if err != nil {
		// Nenhum provedor começou a responder: a reserva volta inteira.
		rsv.settle(r.Context(), 0)
		writeRouterError(r.Context(), w, in.Model, err)
		return
	}
	defer func() { _ = res.Stream.Close() }()

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Llmux-Provider", res.Provider)
	infoFrom(r.Context()).provider = res.Provider
	// O custo só é conhecido no fim, quando os cabeçalhos já saíram. Um trailer é um
	// cabeçalho enviado depois do corpo, e precisa ser declarado antes dele.
	h.Set("Trailer", costHeader)
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	base := chatChunk{
		ID:      "chatcmpl-" + rand.Text(),
		Object:  "chat.completion.chunk",
		Created: time.Now().Unix(),
		Model:   in.Model,
	}

	var u *provider.Usage
	for first := true; ; first = false {
		chunk, err := res.Stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// Com o cliente fora, não há para quem escrever o erro.
			if r.Context().Err() == nil {
				slog.WarnContext(r.Context(), "stream interrompido", "error", err)
				writeStreamError(r.Context(), rc, w)
			}
			return
		}
		if chunk.Usage != nil {
			u = chunk.Usage
		}

		choice := chunkChoice{Delta: chunkDelta{Content: chunk.Content}}
		if first {
			choice.Delta.Role = string(provider.RoleAssistant)
		}
		if chunk.FinishReason != "" {
			choice.FinishReason = &chunk.FinishReason
		}
		out := base
		out.Choices = []chunkChoice{choice}
		if err := writeEvent(rc, w, out); err != nil {
			slog.WarnContext(r.Context(), "escrevendo stream", "error", err)
			return
		}
	}

	if u == nil {
		// Sem uso não há como cobrar; o log é o que permite notar um provedor assim.
		// A reserva fica como está: sem o uso real, o mais seguro é cobrar o
		// reservado.
		slog.WarnContext(r.Context(), "stream sem uso, custo desconhecido")
	} else {
		rsv.settle(r.Context(), u.PromptTokens+u.CompletionTokens)
		s.setCost(r.Context(), w, res.Provider, res.Model, *u)
		if in.StreamOptions.IncludeUsage {
			out := base
			out.Choices = []chunkChoice{}
			out.Usage = &chatUsage{
				PromptTokens:     u.PromptTokens,
				CompletionTokens: u.CompletionTokens,
				TotalTokens:      u.PromptTokens + u.CompletionTokens,
			}
			if err := writeEvent(rc, w, out); err != nil {
				slog.WarnContext(r.Context(), "escrevendo stream", "error", err)
				return
			}
		}
	}

	if err := writeData(rc, w, []byte("[DONE]")); err != nil {
		slog.WarnContext(r.Context(), "escrevendo stream", "error", err)
	}
}

// writeStreamError não traz o detalhe do erro pelo mesmo motivo de writeRouterError.
// Sem o [DONE] depois dele, o cliente também sabe que a resposta não terminou.
func writeStreamError(ctx context.Context, rc *http.ResponseController, w io.Writer) {
	if err := writeEvent(rc, w, errorResponse{Error: errorDetail{
		Message: "o provedor falhou no meio da resposta",
		Type:    "server_error",
	}}); err != nil {
		slog.WarnContext(ctx, "escrevendo stream", "error", err)
	}
}

// writeEvent recebe any pelo mesmo motivo de writeJSON: pedaços e erros têm tipos
// diferentes.
func writeEvent(rc *http.ResponseController, w io.Writer, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("codificando evento: %w", err)
	}
	return writeData(rc, w, data)
}

// writeData escreve um evento SSE e o envia na hora. Sem o Flush, o pedaço ficaria
// no buffer do servidor, e o cliente receberia tudo junto no fim.
func writeData(rc *http.ResponseController, w io.Writer, data []byte) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return rc.Flush()
}
