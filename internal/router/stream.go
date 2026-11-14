package router

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// errFirstChunkTimeout é a causa do cancelamento quando o prazo da tentativa vence.
// Sem uma causa própria, o cancelamento pareceria uma desistência do cliente, e o
// provedor travado não contaria como falha nem acionaria o fallback.
var errFirstChunkTimeout = errors.New("router: prazo até o primeiro pedaço esgotado")

// StreamResult é um stream com o provedor e o modelo da rota que o produziu.
type StreamResult struct {
	Provider string
	// Model é o nome físico da rota, como em Result.
	Model  string
	Stream provider.StreamReader
}

// ChatStream é o Chat em streaming. Só há fallback até o primeiro pedaço: depois
// dele, o cliente já recebeu texto de um provedor, e os erros chegam por Recv.
// Quem recebe o stream precisa fechá-lo, inclusive depois de um erro.
func (r *Router) ChatStream(ctx context.Context, model string, req provider.ChatRequest) (*StreamResult, error) {
	var res *StreamResult
	err := r.fallback(ctx, model, func(ctx context.Context, route Route, br *breaker) error {
		req.Model = route.Model
		stream, err := r.openStream(ctx, route, req, br)
		if err == nil {
			res = &StreamResult{Provider: route.Provider.Name(), Model: route.Model, Stream: stream}
		}
		return err
	})
	return res, err
}

// openStream abre o stream e lê o primeiro pedaço. Um provedor pode aceitar o
// pedido e falhar logo na primeira leitura; lendo aqui, essa falha ainda permite
// fallback.
func (r *Router) openStream(ctx context.Context, route Route, req provider.ChatRequest, br *breaker) (provider.StreamReader, error) {
	// O prazo vale só até o primeiro pedaço. Um WithTimeout cortaria no meio uma
	// resposta longa e saudável, e o defer cancel de attempt mataria o stream assim
	// que esta função retornasse.
	attemptCtx, cancel := context.WithCancelCause(ctx)
	timer := time.AfterFunc(r.attemptTimeout, func() { cancel(errFirstChunkTimeout) })

	fail := func(stream provider.StreamReader, err error) (provider.StreamReader, error) {
		timer.Stop()
		if stream != nil {
			_ = stream.Close()
		}
		if errors.Is(context.Cause(attemptCtx), errFirstChunkTimeout) {
			err = fmt.Errorf("%w: %w", provider.ErrTimeout, err)
		}
		cancel(nil)
		record(ctx, br, err)
		return nil, err
	}

	stream, err := route.Provider.ChatStream(attemptCtx, req)
	if err != nil {
		return fail(nil, err)
	}

	first, err := stream.Recv()
	if err != nil {
		return fail(stream, err)
	}
	// Stop devolve false se o prazo venceu junto com o primeiro pedaço: o contexto
	// já foi cancelado, e a próxima leitura falharia depois de não haver mais
	// fallback.
	if !timer.Stop() {
		return fail(stream, errFirstChunkTimeout)
	}

	return &routedStream{ctx: ctx, cancel: cancel, br: br, stream: stream, first: &first}, nil
}

// routedStream devolve o primeiro pedaço já lido e informa o breaker no fim do
// stream. Como os StreamReader dos provedores, não é seguro para uso concorrente.
type routedStream struct {
	ctx    context.Context // o do pedido, pelo mesmo motivo de record
	cancel context.CancelCauseFunc
	br     *breaker
	stream provider.StreamReader
	first  *provider.StreamChunk
	// judged evita um segundo veredito: um Recv depois do fim, ou o Close depois
	// de um erro, não pode contar de novo.
	judged bool
}

func (s *routedStream) Recv() (provider.StreamChunk, error) {
	if s.first != nil {
		chunk := *s.first
		s.first = nil
		return chunk, nil
	}

	chunk, err := s.stream.Recv()
	if err != nil && !s.judged {
		s.judged = true
		if errors.Is(err, io.EOF) {
			s.br.success()
		} else {
			record(s.ctx, s.br, err)
		}
	}
	return chunk, err
}

// Close solta a conexão com o provedor. Fechar antes do fim é desistência do
// cliente e não diz nada sobre o provedor; sem o release, uma sonda do breaker
// cancelada deixaria o circuito meio-aberto para sempre.
func (s *routedStream) Close() error {
	if !s.judged {
		s.judged = true
		s.br.release()
	}
	err := s.stream.Close()
	s.cancel(nil)
	return err
}
