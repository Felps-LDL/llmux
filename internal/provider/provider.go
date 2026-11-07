package provider

import "context"

// Provider é um provedor de LLM. O router e os testes dependem desta interface,
// nunca de um provedor concreto.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req ChatRequest) (*ChatResponse, error)
	// ChatStream devolve erro só para falhas antes do primeiro pedaço; as do meio
	// do stream aparecem em StreamReader.Recv. Quem recebe o stream precisa
	// fechá-lo.
	ChatStream(ctx context.Context, req ChatRequest) (StreamReader, error)
}

// Role identifica o autor de uma Message.
type Role string

// Papéis aceitos em uma Message.
const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// Message é uma mensagem da conversa enviada ao provedor.
type Message struct {
	Role    Role
	Content string
}

// ChatRequest é um pedido de chat, independente do formato de cada provedor.
type ChatRequest struct {
	// Model é o nome físico do provedor ("gpt-4o-mini"), não o lógico ("fast"):
	// o router resolve o nome lógico antes da chamada, e o provedor não precisa
	// conhecer a config de roteamento.
	Model    string
	Messages []Message
	// Temperature é ponteiro porque, com omitempty, um float64 zero sumiria do
	// JSON e o provedor aplicaria o default dele: pediríamos a temperatura mínima
	// e receberíamos a aleatoriedade do default, sem erro nenhum. Não uniformizar
	// com MaxTokens. Nil significa "usar o default do provedor".
	Temperature *float64
	// Zero significa "usar o default do provedor"; 0 tokens não é um pedido útil.
	MaxTokens int
}

// Usage é a contagem de tokens reportada pelo provedor em uma chamada.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// ChatResponse é a resposta de um provedor, já normalizada pelo adaptador.
type ChatResponse struct {
	// Model é o nome que o provedor reporta ter usado, que pode ser mais
	// específico que o pedido ("gpt-4o-mini-2024-07-18").
	Model   string
	Content string
	// FinishReason usa o vocabulário da OpenAI ("stop", "length"), porque é o da
	// API que expomos. Cada adaptador traduz o do seu provedor na entrada: a
	// Anthropic devolve "end_turn" e "max_tokens".
	FinishReason string
	Usage        Usage
}
