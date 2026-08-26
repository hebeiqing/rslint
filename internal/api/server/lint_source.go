package server

import (
	"context"
	"errors"

	"github.com/web-infra-dev/rslint/internal/linter"
)

// apiLintSource exposes the immutable Program/config snapshot prepared for one
// API request through the linter's source port. API requests lint or plan once;
// a second acquisition would indicate that execution policy leaked upward or
// that the wrong linter request kind was selected.
type apiLintSource struct {
	generation linter.Generation
	acquired   bool
}

func (s *apiLintSource) AcquireGeneration(ctx context.Context) (linter.Generation, linter.ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return linter.Generation{}, nil, err
	}
	if s == nil {
		return linter.Generation{}, nil, errors.New("API lint source is not configured")
	}
	if s.acquired {
		return linter.Generation{}, nil, errors.New("API lint source was acquired more than once")
	}
	s.acquired = true
	return s.generation, nil, nil
}
