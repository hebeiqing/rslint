package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/program/loader"
)

// cliGenerationProvider adapts the CLI's disk-backed Program loader to the
// linter's source port. It owns rebuild mechanics only; observation order and
// fix-round scheduling remain private to linter.RunPipeline.
type cliGenerationProvider struct {
	initial     loader.LoadResult
	usedInitial bool
	rebuild     func() (loader.LoadResult, error)
	generation  func(loader.LoadResult) linter.Generation
}

func (p *cliGenerationProvider) AcquireGeneration(ctx context.Context) (linter.Generation, linter.ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return linter.Generation{}, nil, err
	}
	if p == nil || p.generation == nil {
		return linter.Generation{}, nil, errors.New("CLI lint generation provider is not configured")
	}
	if !p.usedInitial {
		p.usedInitial = true
		return p.generation(p.initial), nil, nil
	}
	if p.rebuild == nil {
		return linter.Generation{}, nil, errors.New("rebuild Programs after fixes: provider is not configured")
	}
	binding, err := p.rebuild()
	if err != nil {
		return linter.Generation{}, nil, fmt.Errorf("rebuild Programs after fixes: %w", err)
	}
	if len(binding.Programs) == 0 {
		return linter.Generation{}, nil, errors.New("rebuild Programs after fixes: no Program returned")
	}
	return p.generation(binding), nil, nil
}
