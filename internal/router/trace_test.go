package router_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace"

	"github.com/Felps-LDL/llmux/internal/provider"
	"github.com/Felps-LDL/llmux/internal/provider/fake"
	"github.com/Felps-LDL/llmux/internal/router"
)

// Verifica o que o trace precisa mostrar de um fallback: uma tentativa por rota, na
// ordem, com o provedor de cada uma e a que falhou marcada como erro. É isso que
// responde "onde foram os 40 segundos": o cronograma separa a espera pelo provedor
// que falhou do tempo do que respondeu.
func TestCadaTentativaViraUmSpan(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	a := fake.New("a", fake.Fail(provider.ErrUnavailable))
	b := fake.New("b", fake.Reply(provider.ChatResponse{Content: "olá"}))
	r := router.New(map[string][]router.Route{
		"fast": {{Provider: a, Model: "modelo-a"}, {Provider: b, Model: "modelo-b"}},
	}, time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})

	if _, err := r.Chat(t.Context(), "fast", provider.ChatRequest{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 2 {
		t.Fatalf("spans = %d, want 2 (uma tentativa por rota)", len(spans))
	}
	want := []struct {
		provider string
		status   codes.Code
	}{{"a", codes.Error}, {"b", codes.Unset}}
	for i, s := range spans {
		var got string
		for _, kv := range s.Attributes() {
			if kv.Key == "llmux.provider" {
				got = kv.Value.AsString()
			}
		}
		if s.Name() != "router.attempt" || got != want[i].provider || s.Status().Code != want[i].status {
			t.Errorf("span %d = %s/%s/%v, want router.attempt/%s/%v", i, s.Name(), got, s.Status().Code, want[i].provider, want[i].status)
		}
	}
}

// spanSeen é um provedor que guarda o span ativo no contexto que recebe, que é o que
// um cliente HTTP instrumentado usaria como pai.
type spanSeen struct{ got trace.SpanContext }

func (p *spanSeen) Name() string { return "a" }

func (p *spanSeen) Chat(ctx context.Context, _ provider.ChatRequest) (*provider.ChatResponse, error) {
	p.got = trace.SpanContextFromContext(ctx)
	return &provider.ChatResponse{}, nil
}

func (p *spanSeen) ChatStream(context.Context, provider.ChatRequest) (provider.StreamReader, error) {
	return nil, errors.New("não usado")
}

// A chamada ao provedor precisa acontecer dentro do span da tentativa. Com o
// contexto de fora, o span HTTP do provedor ficaria pendurado no span do pedido, e
// num fallback não daria para saber qual chamada pertence a qual tentativa. Este
// teste nasceu de um trace real no Jaeger, onde isso aconteceu.
func TestProvedorRecebeOContextoDaTentativa(t *testing.T) {
	rec := tracetest.NewSpanRecorder()
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(rec)))
	t.Cleanup(func() { otel.SetTracerProvider(prev) })

	p := &spanSeen{}
	r := router.New(map[string][]router.Route{"fast": {{Provider: p, Model: "m"}}},
		time.Second, router.BreakerConfig{Threshold: 100, Cooldown: time.Minute})
	if _, err := r.Chat(t.Context(), "fast", provider.ChatRequest{}); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	spans := rec.Ended()
	if len(spans) != 1 {
		t.Fatalf("spans = %d, want 1", len(spans))
	}
	if p.got.SpanID() != spans[0].SpanContext().SpanID() {
		t.Error("o provedor recebeu um contexto sem o span da tentativa")
	}
}
