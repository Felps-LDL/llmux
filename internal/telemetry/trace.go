package telemetry

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
)

// SetupTracing liga o tracing quando OTEL_EXPORTER_OTLP_ENDPOINT está definida, a
// variável padrão do OpenTelemetry. Sem ela, nada é configurado, e a API do
// OpenTelemetry segue no modo padrão, que não faz nada e custa quase zero: sem um
// coletor, cada lote de spans viraria um erro de envio no log.
//
// O endereço, a amostragem (OTEL_TRACES_SAMPLER) e o nome do serviço
// (OTEL_SERVICE_NAME) são lidos pelo próprio SDK, pelas variáveis do padrão. A
// função devolvida esvazia o buffer de spans e precisa ser chamada no fim.
func SetupTracing(ctx context.Context, getenv func(string) string) (shutdown func(context.Context) error, err error) {
	// O traceparent de quem chama é lido sempre, mesmo com o tracing desligado: assim
	// o id do trace do cliente segue para os logs (ver contextHandler).
	otel.SetTextMapPropagator(propagation.TraceContext{})

	if getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func(context.Context) error { return nil }, nil
	}

	// OTLP por HTTP, e não por gRPC: o mesmo protocolo, sem trazer o gRPC inteiro
	// como dependência.
	exporter, err := otlptracehttp.New(ctx)
	if err != nil {
		return nil, fmt.Errorf("tracing: criando o exportador: %w", err)
	}

	// Primeiro o nome padrão, depois o ambiente: um OTEL_SERVICE_NAME definido pelo
	// operador vence, e sem ele o serviço não aparece como "unknown_service".
	res, err := resource.New(ctx,
		resource.WithAttributes(attribute.String("service.name", "llmux-gateway")),
		resource.WithFromEnv(),
		resource.WithTelemetrySDK(),
	)
	if err != nil {
		return nil, fmt.Errorf("tracing: descrevendo o serviço: %w", err)
	}

	// O batcher junta spans e os envia em segundo plano: exportar um por um no fim de
	// cada span poria a latência do coletor no caminho do pedido.
	tp := sdktrace.NewTracerProvider(sdktrace.WithBatcher(exporter), sdktrace.WithResource(res))
	otel.SetTracerProvider(tp)
	return tp.Shutdown, nil
}
