package router

import (
	"context"
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
