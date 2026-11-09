package provider

// StreamChunk é um pedaço de uma resposta em streaming.
type StreamChunk struct {
	Content      string
	FinishReason string
	// Ponteiro porque só o último pedaço traz o uso: nil é "ainda não chegou",
	// diferente de zero tokens.
	Usage *Usage
}

// StreamReader lê os pedaços de uma resposta. Recv devolve io.EOF no fim.
type StreamReader interface {
	Recv() (StreamChunk, error)
	Close() error
}
