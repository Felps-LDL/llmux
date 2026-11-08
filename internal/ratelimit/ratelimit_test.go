package ratelimit_test

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/redis"

	"github.com/Felps-LDL/llmux/internal/ratelimit"
)

var rdb *goredis.Client

// TestMain sobe um Redis para o pacote, pelo mesmo motivo do Postgres no store.
// Cada teste usa ids de chave próprios (nextKey), então os baldes não se misturam
// e não é preciso limpar o Redis entre os testes.
func TestMain(m *testing.M) {
	ctx := context.Background()
	// A mesma imagem do docker-compose.
	container, err := redis.Run(ctx, "redis:8-alpine")
	if err != nil {
		log.Fatalf("subindo o Redis: %v", err)
	}
	url, err := container.ConnectionString(ctx)
	if err != nil {
		log.Fatalf("lendo o endereço: %v", err)
	}
	opts, err := goredis.ParseURL(url)
	if err != nil {
		log.Fatalf("lendo o endereço: %v", err)
	}
	rdb = goredis.NewClient(opts)

	code := m.Run()

	_ = rdb.Close()
	if err := testcontainers.TerminateContainer(container); err != nil {
		log.Printf("derrubando o Redis: %v", err)
	}
	os.Exit(code)
}

var keyIDs atomic.Int64

func nextKey() int64 { return keyIDs.Add(1) }

func reserve(t *testing.T, l *ratelimit.Limiter, key int64, limits ratelimit.Limits, tokens int) ratelimit.Decision {
	t.Helper()
	d, err := l.Reserve(t.Context(), key, limits, tokens)
	if err != nil {
		t.Fatalf("Reserve: %v", err)
	}
	return d
}

// Verifica o limite de pedidos e a informação que o cliente recebe quando passa
// dele. Com 3 por minuto, a recarga é de uma ficha a cada 20 s: o Retry-After
// precisa estar nesse intervalo. Um valor zero faria os SDKs tentarem de novo na
// hora, martelando o gateway; um valor muito alto deixaria o cliente parado à toa.
func TestReserveRecusaAlemDoLimiteDePedidos(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 3, TokensPerMinute: 1000}

	for i := range 3 {
		d := reserve(t, l, key, limits, 1)
		if !d.Allowed {
			t.Fatalf("pedido %d recusado, want permitido (balde cheio comporta 3)", i+1)
		}
		if want := 2 - i; d.RemainingRequests != want {
			t.Errorf("pedido %d: RemainingRequests = %d, want %d", i+1, d.RemainingRequests, want)
		}
	}

	d := reserve(t, l, key, limits, 1)
	if d.Allowed {
		t.Fatal("quarto pedido permitido, want recusado")
	}
	if d.RetryAfter <= 0 || d.RetryAfter > 20*time.Second {
		t.Errorf("RetryAfter = %v, want entre 0 e 20s", d.RetryAfter)
	}
}

// Verifica a garantia que só existe sob concorrência, como no teste do breaker:
// 50 pedidos ao mesmo tempo contra um limite de 10. Se ler o saldo e descontar
// fossem dois comandos separados, vários leriam o mesmo saldo e passariam juntos.
// O script Lua roda inteiro sem nada no meio, então passam exatamente 10.
func TestReserveNaoDeixaPassarMaisQueOLimiteSobConcorrencia(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 10, TokensPerMinute: 100000}

	var allowed atomic.Int64
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 50 {
		wg.Go(func() {
			<-start
			d, err := l.Reserve(t.Context(), key, limits, 1)
			if err != nil {
				t.Errorf("Reserve: %v", err)
				return
			}
			if d.Allowed {
				allowed.Add(1)
			}
		})
	}
	close(start)
	wg.Wait()

	if got := allowed.Load(); got != 10 {
		t.Errorf("pedidos permitidos = %d, want 10", got)
	}
}

func TestReserveRecusaPorTokensSemGastarPedido(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 10, TokensPerMinute: 1000}

	if d := reserve(t, l, key, limits, 600); !d.Allowed || d.RemainingTokens != 400 {
		t.Fatalf("primeira reserva = %+v, want permitida com 400 restantes", d)
	}
	d := reserve(t, l, key, limits, 600)
	if d.Allowed {
		t.Fatal("segunda reserva de 600 permitida com 400 no balde, want recusada")
	}
	// Faltam 200 tokens a 1000 por minuto: 12 s.
	if d.RetryAfter <= 0 || d.RetryAfter > 12*time.Second {
		t.Errorf("RetryAfter = %v, want entre 0 e 12s", d.RetryAfter)
	}
	// A recusa por tokens não pode ter gastado a ficha de pedido. O saldo devolvido
	// na recusa é o de antes de gravar, então só a reserva seguinte mostra o que
	// ficou gravado: 10 - 1 (a primeira) - 1 (esta) = 8, e não 7.
	d = reserve(t, l, key, limits, 1)
	if d.RemainingRequests != 8 {
		t.Errorf("RemainingRequests = %d, want 8 (a recusa não pode ter contado)", d.RemainingRequests)
	}
}

func TestSettleDevolveSobraEDescontaExcesso(t *testing.T) {
	l := ratelimit.New(rdb)
	limits := ratelimit.Limits{RequestsPerMinute: 100, TokensPerMinute: 1000}

	t.Run("sobra volta ao balde", func(t *testing.T) {
		key := nextKey()
		d := reserve(t, l, key, limits, 800)
		// A reserva foi de 800, mas a resposta gastou 100.
		if err := l.Settle(t.Context(), key, limits, d.Reserved, 100); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		if d := reserve(t, l, key, limits, 800); !d.Allowed {
			t.Errorf("reserva após a devolução recusada: %+v", d)
		}
	})

	t.Run("excesso vira dívida", func(t *testing.T) {
		key := nextKey()
		d := reserve(t, l, key, limits, 100)
		// A estimativa foi de 100, mas a resposta gastou 1500: o saldo fica negativo.
		if err := l.Settle(t.Context(), key, limits, d.Reserved, 1500); err != nil {
			t.Fatalf("Settle: %v", err)
		}
		d = reserve(t, l, key, limits, 1)
		if d.Allowed {
			t.Fatal("reserva com dívida permitida, want recusada")
		}
		// A dívida aparece como zero, e não como um número negativo no cabeçalho.
		if d.RemainingTokens != 0 {
			t.Errorf("RemainingTokens = %d, want 0", d.RemainingTokens)
		}
		// Faltam ~501 tokens a 1000 por minuto: ~30 s, bem mais que a recarga de um
		// único token.
		if d.RetryAfter < 25*time.Second {
			t.Errorf("RetryAfter = %v, want ~30s (a dívida precisa ser paga)", d.RetryAfter)
		}
	})
}

// Sem a redução ao limite, um pedido com max_tokens acima do limite por minuto
// nunca caberia no balde, nem cheio, e o cliente esperaria para sempre.
func TestReserveMaiorQueOLimiteCabeComBaldeCheio(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 10, TokensPerMinute: 100}

	d := reserve(t, l, key, limits, 5000)
	if !d.Allowed || d.Reserved != 100 {
		t.Fatalf("reserva = %+v, want permitida com 100 reservados", d)
	}
	if d := reserve(t, l, key, limits, 1); d.Allowed {
		t.Error("reserva com o balde vazio permitida, want recusada")
	}
}

func TestBaldesDeChavesDiferentesSaoIndependentes(t *testing.T) {
	l := ratelimit.New(rdb)
	a, b := nextKey(), nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 1, TokensPerMinute: 1000}

	if d := reserve(t, l, a, limits, 1); !d.Allowed {
		t.Fatal("chave A recusada, want permitida")
	}
	if d := reserve(t, l, b, limits, 1); !d.Allowed {
		t.Error("chave B recusada depois de A esgotar o próprio balde, want permitida")
	}
}

// O único teste que depende do tempo real, porque o relógio é o do Redis. A taxa
// alta (10 por segundo) mantém a espera curta; a margem (300 ms para 1 ficha)
// evita falhas por atraso do agendador.
func TestBaldeRecarregaComOTempo(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 600, TokensPerMinute: 100000}

	for range 600 {
		reserve(t, l, key, limits, 1)
	}
	if d := reserve(t, l, key, limits, 1); d.Allowed {
		t.Fatal("balde esgotado permitiu pedido, want recusado")
	}

	time.Sleep(300 * time.Millisecond)

	if d := reserve(t, l, key, limits, 1); !d.Allowed {
		t.Errorf("após 300ms (~3 fichas recarregadas) o pedido foi recusado: %+v", d)
	}
}

// Um balde cheio é igual a um que não existe, então as chaves precisam expirar:
// sem isso, cada chave de API que já fez um pedido ocuparia o Redis para sempre.
func TestBaldeExpiraQuandoEstariaCheio(t *testing.T) {
	l := ratelimit.New(rdb)
	key := nextKey()
	limits := ratelimit.Limits{RequestsPerMinute: 60, TokensPerMinute: 1000}

	reserve(t, l, key, limits, 1)

	for _, k := range []string{"llmux:rl:{%d}:req", "llmux:rl:{%d}:tok"} {
		name := fmt.Sprintf(k, key)
		ttl, err := rdb.PTTL(t.Context(), name).Result()
		if err != nil {
			t.Fatalf("PTTL %s: %v", name, err)
		}
		// Uma ficha gasta a 1 por segundo enche em 1 s; com a folga de 1 s, ~2 s.
		if ttl <= 0 || ttl > 3*time.Second {
			t.Errorf("TTL de %s = %v, want entre 0 e 3s", name, ttl)
		}
	}
}

// Um balde ocioso não pode acumular mais fichas que o limite: o cliente ganharia
// uma rajada maior que a contratada. Os dois caminhos que somam fichas têm teto,
// a recarga e a devolução do Settle. As taxas altas deixam a espera curta.
func TestSaldoNuncaPassaDaCapacidade(t *testing.T) {
	l := ratelimit.New(rdb)

	t.Run("recarga", func(t *testing.T) {
		key := nextKey()
		// 600 por minuto: a ficha gasta volta em 100 ms.
		limits := ratelimit.Limits{RequestsPerMinute: 600, TokensPerMinute: 100000}
		reserve(t, l, key, limits, 1)
		time.Sleep(500 * time.Millisecond)

		// Sem teto, a recarga teria somado ~5 fichas a 599, e restariam ~603.
		if d := reserve(t, l, key, limits, 1); d.RemainingRequests != 599 {
			t.Errorf("RemainingRequests = %d, want 599 (600 de teto, menos este)", d.RemainingRequests)
		}
	})

	t.Run("devolução", func(t *testing.T) {
		key := nextKey()
		// 60000 por minuto: os 100 reservados voltam pela recarga em 100 ms.
		limits := ratelimit.Limits{RequestsPerMinute: 100, TokensPerMinute: 60000}
		d := reserve(t, l, key, limits, 100)
		time.Sleep(200 * time.Millisecond)
		// O provedor falhou sem gerar nada: a reserva volta inteira, mas o balde já
		// está cheio pela recarga.
		if err := l.Settle(t.Context(), key, limits, d.Reserved, 0); err != nil {
			t.Fatalf("Settle: %v", err)
		}

		if d := reserve(t, l, key, limits, 1); d.RemainingTokens != 59999 {
			t.Errorf("RemainingTokens = %d, want 59999 (60000 de teto, menos este)", d.RemainingTokens)
		}
	})
}
