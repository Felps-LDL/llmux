// Package fake implementa um provider.Provider roteirizado para testes, sem
// rede, sem chave de API e sem custo.
package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// ErrScriptExhausted indica uma chamada além do roteiro. Não pertence a nenhuma
// classe de provider, para o router não tratar um teste errado como fallback.
var ErrScriptExhausted = errors.New("fake: roteiro esgotado")

// Step é um passo do roteiro: o resultado de uma chamada a Chat ou ChatStream.
type Step struct {
	resp   *provider.ChatResponse
	err    error
	chunks []provider.StreamChunk
	// streamErr falha um Recv depois dos pedaços; err falha o próprio ChatStream.
	// O router precisa distinguir os dois: só antes do primeiro pedaço ainda dá
	// para trocar de provedor.
	streamErr error
}

// Reply cria um passo que responde com sucesso.
func Reply(resp provider.ChatResponse) Step {
	return Step{resp: &resp}
}

// Fail cria um passo que falha com err.
func Fail(err error) Step {
	return Step{err: err}
}

// Provider é um provedor falso que segue um roteiro e registra as chamadas.
type Provider struct {
	name string

	// O router atende pedidos concorrentes; sem esta trava, o -race acusaria o
	// falso em vez do código sob teste.
	mu     sync.Mutex
	steps  []Step
	calls  []provider.ChatRequest
	closes int
}

// Falha na compilação, e não no primeiro teste, se o falso deixar de cumprir o
// contrato.
var _ provider.Provider = (*Provider)(nil)

// New cria um Provider que executa steps na ordem dada.
func New(name string, steps ...Step) *Provider {
	return &Provider{name: name, steps: steps}
}

// Name devolve o nome dado em New.
func (p *Provider) Name() string {
	return p.name
}

// Chat registra a chamada e executa o próximo passo do roteiro.
func (p *Provider) Chat(ctx context.Context, req provider.ChatRequest) (*provider.ChatResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, req)

	// Um provedor real respeita o contexto; se o falso ignorasse, um router que
	// esquecesse de propagar o cancelamento passaria nos testes.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake: %w", err)
	}

	if len(p.steps) == 0 {
		return nil, ErrScriptExhausted
	}

	step := p.steps[0]
	p.steps = p.steps[1:]
	return step.resp, step.err
}

// Calls devolve uma cópia dos pedidos recebidos, para o teste não compartilhar
// memória com chamadas ainda em andamento.
func (p *Provider) Calls() []provider.ChatRequest {
	p.mu.Lock()
	defer p.mu.Unlock()

	return slices.Clone(p.calls)
}

// Closes devolve quantas vezes Close foi chamado nos streams deste provedor. É
// assim que um teste prova que o código sob teste não vaza conexões.
func (p *Provider) Closes() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.closes
}

// StreamReply cria um passo que entrega os pedaços e termina com io.EOF.
func StreamReply(chunks ...provider.StreamChunk) Step {
	return Step{chunks: chunks}
}

// StreamFailAfter entrega os pedaços e então falha, como uma conexão que cai.
func StreamFailAfter(err error, chunks ...provider.StreamChunk) Step {
	return Step{chunks: chunks, streamErr: err}
}

// ChatStream registra a chamada e executa o próximo passo do roteiro como stream.
// Um passo de Fail faz o próprio ChatStream falhar, antes de qualquer pedaço.
func (p *Provider) ChatStream(ctx context.Context, req provider.ChatRequest) (provider.StreamReader, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.calls = append(p.calls, req)

	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("fake: %w", err)
	}

	if len(p.steps) == 0 {
		return nil, ErrScriptExhausted
	}

	step := p.steps[0]
	p.steps = p.steps[1:]
	if step.err != nil {
		return nil, step.err
	}
	return &streamReader{p: p, ctx: ctx, chunks: step.chunks, err: step.streamErr}, nil
}

type streamReader struct {
	p      *Provider
	ctx    context.Context
	chunks []provider.StreamChunk
	err    error
}

func (r *streamReader) Recv() (provider.StreamChunk, error) {
	// O cliente pode desistir no meio do stream, depois de ChatStream ter
	// verificado o contexto; um provedor real pararia de entregar nesse ponto.
	if err := r.ctx.Err(); err != nil {
		return provider.StreamChunk{}, fmt.Errorf("fake: %w", err)
	}
	if len(r.chunks) > 0 {
		chunk := r.chunks[0]
		r.chunks = r.chunks[1:]
		return chunk, nil
	}
	if r.err != nil {
		return provider.StreamChunk{}, r.err
	}
	return provider.StreamChunk{}, io.EOF
}

func (r *streamReader) Close() error {
	r.p.mu.Lock()
	defer r.p.mu.Unlock()

	r.p.closes++
	return nil
}
