package router

import (
	"sync"
	"time"
)

type breakerState int

const (
	stateClosed breakerState = iota
	stateOpen
	stateHalfOpen
)

// breaker é o circuit breaker de um provedor: depois de falhas consecutivas, para
// de enviar pedidos a ele até o cooldown passar.
type breaker struct {
	threshold int
	cooldown  time.Duration
	// now é injetado para os testes avançarem o tempo sem esperar por ele.
	now func() time.Time

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
	probing  bool
}

func newBreaker(threshold int, cooldown time.Duration, now func() time.Time) *breaker {
	return &breaker{threshold: threshold, cooldown: cooldown, now: now}
}

// allow diz se um pedido pode ser enviado. Quem recebe true precisa informar o
// resultado com success, failure ou release; sem isso, uma sonda nunca terminaria.
func (b *breaker) allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false
		}
		b.state = stateHalfOpen
		b.probing = true
		return true
	case stateHalfOpen:
		// Uma sonda por vez: se o provedor ainda estiver mal, liberar o tráfego
		// inteiro para testá-lo seria martelar justamente quem está se recuperando.
		if b.probing {
			return false
		}
		b.probing = true
		return true
	default:
		return true
	}
}

func (b *breaker) success() {
	b.mu.Lock()
	defer b.mu.Unlock()

	// Uma resposta atrasada, de antes de o circuito abrir, não prova recuperação.
	if b.state == stateOpen {
		return
	}
	b.state = stateClosed
	b.failures = 0
	b.probing = false
}

func (b *breaker) failure() {
	b.mu.Lock()
	defer b.mu.Unlock()

	switch b.state {
	case stateHalfOpen:
		b.trip()
	case stateClosed:
		b.failures++
		if b.failures >= b.threshold {
			b.trip()
		}
	}
}

// release encerra uma chamada que não diz nada sobre a saúde do provedor, como um
// cancelamento pelo cliente. Sem isso, uma sonda cancelada deixaria o circuito
// meio-aberto para sempre, recusando todos os pedidos.
func (b *breaker) release() {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.state == stateHalfOpen {
		b.probing = false
	}
}

// trip precisa ser chamado com a trava presa.
func (b *breaker) trip() {
	b.state = stateOpen
	b.openedAt = b.now()
	b.probing = false
}
