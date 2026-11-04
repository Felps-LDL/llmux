package router

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Verifica o limite exato: as falhas abaixo dele não abrem o circuito, e a que o
// atinge abre. Um erro de uma unidade aqui abriria o circuito por uma falha
// isolada ou martelaria um provedor morto por uma chamada a mais.
func TestBreakerAbreAoAtingirLimite(t *testing.T) {
	b := newBreaker(3, time.Minute, time.Now)

	b.failure()
	b.failure()
	if !b.allow() {
		t.Fatal("allow() = false com 2 falhas, want true")
	}

	b.failure()
	if b.allow() {
		t.Error("allow() = true com 3 falhas, want false")
	}
}

// Verifica o caminho de recuperação, que depende do tempo: por isso o relógio é
// uma variável que o teste avança, sem espera real. Importa porque um breaker que
// nunca fecha tira o provedor de produção para sempre, e um que libera várias
// sondas martela quem está se recuperando.
func TestBreakerLiberaUmaSondaAposCooldown(t *testing.T) {
	now := time.Now()
	b := newBreaker(1, time.Minute, func() time.Time { return now })
	b.failure()

	now = now.Add(time.Minute - time.Nanosecond)
	if b.allow() {
		t.Fatal("allow() = true antes do cooldown, want false")
	}

	now = now.Add(time.Nanosecond)
	if !b.allow() {
		t.Fatal("allow() = false após o cooldown, want true (a sonda)")
	}
	if b.allow() {
		t.Fatal("allow() = true com sonda em andamento, want false")
	}

	b.success()
	if !b.allow() {
		t.Error("allow() = false após sonda bem-sucedida, want true")
	}
}

func TestBreakerLiberaNovaSondaAposRelease(t *testing.T) {
	now := time.Now()
	b := newBreaker(1, time.Minute, func() time.Time { return now })
	b.failure()
	now = now.Add(time.Minute)

	if !b.allow() {
		t.Fatal("allow() = false após cooldown, want true (a sonda)")
	}
	if b.allow() {
		t.Fatal("allow() = true com sonda em andamento, want false")
	}
	b.release()

	if !b.allow() {
		t.Fatal("allow() = false após release, want true (nova sonda)")
	}
}

func TestBreakerReabreQuandoSondaFalha(t *testing.T) {
	now := time.Now()
	b := newBreaker(1, time.Minute, func() time.Time { return now })
	b.failure()
	now = now.Add(time.Minute)

	if !b.allow() {
		t.Fatal("allow() = false após cooldown, want true (a sonda)")
	}

	b.failure()

	if b.allow() {
		t.Fatal("allow() = true após falha da sonda, want false (circuito reaberto)")
	}

	now = now.Add(time.Minute)
	if !b.allow() {
		t.Fatal("allow() = false após cooldown, want true (nova sonda)")
	}
}

func TestBreakerSucessoZeraFalhas(t *testing.T) {
	b := newBreaker(3, time.Minute, time.Now)

	b.failure()
	b.failure()
	b.success()
	b.failure()

	if !b.allow() {
		t.Fatal("allow() = false após sucesso, want true")
	}
}

func TestBreakerRespostaAtrasadaNaoFechaCircuito(t *testing.T) {
	b := newBreaker(1, time.Minute, time.Now)
	b.failure()

	b.success()

	if b.allow() {
		t.Fatal("allow() = true após sucesso da resposta atrasada, want false")
	}
}

func TestRecordFechaCircuitoQuandoSondaTemSucesso(t *testing.T) {
	br := newBreaker(1, 0, time.Now)
	br.failure()

	if !br.allow() {
		t.Fatal("allow() = false após cooldown, want true (a sonda)")
	}

	record(context.Background(), br, nil)

	if !br.allow() {
		t.Fatal("allow() = false após sucesso da sonda, want true")
	}
	if !br.allow() {
		t.Fatal("allow() = false após sucesso da sonda, want true")
	}
}

// Verifica a garantia que só existe sob concorrência: entre pedidos simultâneos,
// um único vence a vaga da sonda. Em sequência, este teste não provaria nada, e
// sem a trava em allow vários pedidos poderiam martelar um provedor em recuperação.
func TestBreakerLiberaUmaSondaEntrePedidosConcorrentes(t *testing.T) {
	b := newBreaker(1, 0, time.Now)
	b.failure()

	const requests = 100
	// atomic porque o próprio contador é disputado: um int comum faria o -race
	// acusar o teste, e não o breaker.
	var allowed atomic.Int64
	// Fechar o canal solta todas as goroutines juntas. Sem isso, as primeiras
	// poderiam terminar antes de as últimas começarem, e não haveria disputa.
	start := make(chan struct{})

	var wg sync.WaitGroup
	for range requests {
		wg.Go(func() {
			<-start
			if b.allow() {
				allowed.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 1 {
		t.Errorf("pedidos liberados = %d, want 1", got)
	}
}

func TestBreakerContaFalhasConcorrentes(t *testing.T) {
	const failures = 100
	b := newBreaker(failures, time.Minute, time.Now)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for range failures {
		wg.Go(func() {
			<-start
			b.failure()
		})
	}
	close(start)
	wg.Wait()
	if b.allow() {
		t.Fatal("allow() = true após falhas concorrentes, want false")
	}
}
