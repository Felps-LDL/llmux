// Package fake implementa um provider.Provider roteirizado para testes, sem
// rede, sem chave de API e sem custo.
package fake

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/Felps-LDL/llmux/internal/provider"
)

// ErrScriptExhausted indica uma chamada além do roteiro. Não pertence a nenhuma
// classe de provider, para o router não tratar um teste errado como fallback.
var ErrScriptExhausted = errors.New("fake: roteiro esgotado")

// Step é um passo do roteiro: o resultado de uma chamada a Chat.
type Step struct {
	resp *provider.ChatResponse
	err  error
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
	mu    sync.Mutex
	steps []Step
	calls []provider.ChatRequest
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
