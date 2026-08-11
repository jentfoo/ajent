package tokens

import (
	"context"

	"github.com/jentfoo/ajent/pkg/llm"
)

// Recount returns an exact count of the prompt req would produce, via the
// provider's tokenizer endpoint. It returns llm.ErrNoTokenizer when the provider
// has no usable counting endpoint.
func Recount(ctx context.Context, p llm.Provider, req llm.Request) (int, error) {
	counter, ok := p.(llm.Counter)
	if !ok {
		return 0, llm.ErrNoTokenizer
	}
	n, err := counter.CountTokens(ctx, req)
	if err != nil {
		return 0, err
	}
	return n, nil
}
