package main

import (
	"fmt"
	"io"
	"os"
)

const defaultAddr = ":8080"

type config struct {
	Addr string
}

// getenv é injetado para o teste fornecer um ambiente falso sem mexer no
// ambiente real do processo.
func loadConfig(getenv func(string) string) config {
	cfg := config{Addr: defaultAddr}
	if v := getenv("LLMUX_ADDR"); v != "" {
		cfg.Addr = v
	}
	return cfg
}

// run concentra a lógica fora do main para ser testável: retorna erro em vez
// de chamar os.Exit e escreve num io.Writer em vez de os.Stdout.
func run(getenv func(string) string, stdout io.Writer) error {
	cfg := loadConfig(getenv)
	if _, err := fmt.Fprintf(stdout, "llmux gateway: addr=%s\n", cfg.Addr); err != nil {
		return fmt.Errorf("escrevendo saída: %w", err)
	}
	return nil
}

func main() {
	if err := run(os.Getenv, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "erro: %v\n", err)
		os.Exit(1)
	}
}
