package telemetry_test

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Felps-LDL/llmux/internal/telemetry"
)

// scrape lê /metrics como o Prometheus faria: pelo HTTP, no formato de texto. Testar
// pela saída, e não pelos objetos internos, prova também o nome e os rótulos que um
// dashboard vai consultar.
func scrape(t *testing.T, m *telemetry.Metrics) string {
	t.Helper()
	srv := httptest.NewServer(m.Handler())
	defer srv.Close()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL, nil)
	if err != nil {
		t.Fatalf("montando o pedido: %v", err)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("lendo /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("lendo /metrics: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	return string(body)
}

// Verifica o contrato com o Prometheus: os nomes e rótulos exatos das séries, que
// um painel ou um alerta vão consultar. O pedido recusado antes do provedor vira
// "none", senão a série dele teria rótulos vazios. E a duração precisa cair no
// balde certo: 15 s não pode estar no de 10 s.
func TestMetricsExpoePedidosComOsRotulos(t *testing.T) {
	m := telemetry.NewMetrics(nil)
	m.ObserveRequest("ollama", "fast", 200, 15*time.Second)
	m.ObserveRequest("ollama", "fast", 200, time.Second)
	m.ObserveRequest("", "", 401, time.Millisecond)

	out := scrape(t, m)
	for _, want := range []string{
		`llm_requests_total{model="fast",provider="ollama",status="200"} 2`,
		`llm_requests_total{model="none",provider="none",status="401"} 1`,
		`llm_request_duration_seconds_count{model="fast",provider="ollama"} 2`,
		`llm_request_duration_seconds_bucket{model="fast",provider="ollama",le="10"} 1`,
		`llm_request_duration_seconds_bucket{model="fast",provider="ollama",le="20"} 2`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics sem %q", want)
		}
	}
}

// O estado do circuito é lido na hora da coleta: duas coletas com estados
// diferentes precisam mostrar valores diferentes, sem ninguém avisar a métrica.
func TestMetricsLeEstadoDoCircuitoNaColeta(t *testing.T) {
	states := map[string]int{"a": 0}
	m := telemetry.NewMetrics(func() map[string]int { return states })

	if out := scrape(t, m); !strings.Contains(out, `llm_circuit_state{provider="a"} 0`) {
		t.Errorf("primeira coleta sem o circuito fechado:\n%s", out)
	}
	states = map[string]int{"a": 1}
	if out := scrape(t, m); !strings.Contains(out, `llm_circuit_state{provider="a"} 1`) {
		t.Errorf("segunda coleta sem o circuito aberto:\n%s", out)
	}
}

// Um Metrics nil não mede nada, e não pode derrubar quem o chama.
func TestMetricsNilNaoFazNada(t *testing.T) {
	var m *telemetry.Metrics
	m.ObserveRequest("a", "b", 200, time.Second)
}

// Verifica as métricas de uso, com a conversão que é fácil de errar: o custo chega
// em picodólares (10^-12 USD) e sai em dólares. 450.000.000 picodólares são
// US$ 0,00045; um fator errado de mil passaria despercebido sem a conta à mão.
func TestMetricsExpoeUsoCustoCacheETTFT(t *testing.T) {
	m := telemetry.NewMetrics(nil)
	m.ObserveUsage("openai", "gpt-x", 7, 1000, 500, 450_000_000)
	m.CacheHit()
	m.CacheHit()
	m.ObserveTTFT("openai", 300*time.Millisecond)

	out := scrape(t, m)
	for _, want := range []string{
		`llm_tokens_total{direction="input",provider="openai"} 1000`,
		`llm_tokens_total{direction="output",provider="openai"} 500`,
		`llm_cost_usd_total{api_key="7",model="gpt-x",provider="openai"} 0.00045`,
		`llm_cache_hits_total 2`,
		`llm_time_to_first_token_seconds_bucket{provider="openai",le="0.25"} 0`,
		`llm_time_to_first_token_seconds_bucket{provider="openai",le="0.5"} 1`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("/metrics sem %q", want)
		}
	}
}
