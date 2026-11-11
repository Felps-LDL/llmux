package telemetry

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// Os baldes padrão do Prometheus vão de 5 ms a 10 s, e uma resposta de LLM passa
// disso com folga: todo pedido acima de 10 s cairia no mesmo balde, e o p95 ficaria
// cego justamente nos pedidos lentos.
var latencyBuckets = []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20, 40, 60, 120}

// Metrics guarda as métricas do gateway. Um valor nil é válido e não mede nada, para
// os testes que não olham métricas não precisarem montar uma.
type Metrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

// NewMetrics cria as métricas num registro próprio, e não no global do pacote: dois
// testes não somam no mesmo contador. circuits é lido a cada coleta (ver
// circuitCollector).
func NewMetrics(circuits func() map[string]int) *Metrics {
	m := &Metrics{
		registry: prometheus.NewRegistry(),
		// Rótulos de cardinalidade limitada: o status é um código, e o provedor e o
		// modelo vêm da config. Cada combinação é uma série guardada para sempre, então
		// um rótulo como o request_id criaria uma série por pedido.
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "llm_requests_total",
			Help: "Pedidos atendidos, por provedor, modelo lógico e status HTTP.",
		}, []string{"provider", "model", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "llm_request_duration_seconds",
			Help:    "Duração dos pedidos, do primeiro byte recebido à resposta completa.",
			Buckets: latencyBuckets,
		}, []string{"provider", "model"}),
	}
	m.registry.MustRegister(
		m.requests,
		m.duration,
		circuitCollector{states: circuits},
		// Memória, goroutines e GC do próprio processo: o básico para saber se o
		// problema é o gateway, e não o provedor.
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// Handler serve as métricas no formato que o Prometheus lê.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

// ObserveRequest registra um pedido terminado. Provedor e modelo vazios (um pedido
// recusado antes de chegar a um provedor) viram "none", para a série existir.
func (m *Metrics) ObserveRequest(provider, model string, status int, d time.Duration) {
	if m == nil {
		return
	}
	provider, model = orNone(provider), orNone(model)
	m.requests.WithLabelValues(provider, model, strconv.Itoa(status)).Inc()
	m.duration.WithLabelValues(provider, model).Observe(d.Seconds())
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

var circuitDesc = prometheus.NewDesc(
	"llm_circuit_state",
	"Estado do circuito de cada provedor: 0 fechado, 1 aberto, 2 meio-aberto.",
	[]string{"provider"}, nil,
)

// circuitCollector lê o estado dos circuitos na hora da coleta, em vez de o breaker
// atualizar uma métrica a cada mudança: assim o router não conhece o Prometheus.
type circuitCollector struct {
	states func() map[string]int
}

func (c circuitCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- circuitDesc
}

func (c circuitCollector) Collect(ch chan<- prometheus.Metric) {
	if c.states == nil {
		return
	}
	for provider, state := range c.states() {
		ch <- prometheus.MustNewConstMetric(circuitDesc, prometheus.GaugeValue, float64(state), provider)
	}
}
