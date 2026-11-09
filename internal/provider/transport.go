package provider

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// ClassifyTransportError classifica falhas sem status HTTP: de conexão, de prazo ou
// de uma resposta que chegou cortada ou malformada. Os adaptadores a dividem porque
// a classe decide o fallback e o breaker, e precisa ser a mesma para todo provedor.
func ClassifyTransportError(err error) error {
	// Fica sem classe: quem desistiu foi o cliente, não o provedor.
	if errors.Is(err, context.Canceled) {
		return err
	}

	// O timeout do transporte nem sempre aparece como context.DeadlineExceeded.
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return fmt.Errorf("%w: %w", ErrTimeout, err)
	}

	return fmt.Errorf("%w: %w", ErrUnavailable, err)
}

// StatusError cria o erro de uma resposta HTTP sem sucesso. detail é a mensagem que
// o adaptador extraiu do corpo, no formato do provedor dele.
func StatusError(status int, detail string) error {
	switch {
	case status == http.StatusTooManyRequests:
		return fmt.Errorf("status %d: %s: %w", status, detail, ErrRateLimited)
	case status >= http.StatusInternalServerError:
		return fmt.Errorf("status %d: %s: %w", status, detail, ErrUnavailable)
	default:
		// Sem classe: um 400 ou um 404 de modelo inexistente falharia igual em
		// qualquer nova tentativa e não diz nada sobre a saúde do provedor.
		return fmt.Errorf("status %d: %s", status, detail)
	}
}
