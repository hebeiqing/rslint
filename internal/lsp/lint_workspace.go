package lsp

import (
	"context"
	"fmt"

	"github.com/microsoft/typescript-go/shim/lsp/lsproto"

	"github.com/web-infra-dev/rslint/internal/linter"
)

type speculativeGenerationAcquire func(
	context.Context,
	string,
	documentLintSnapshot,
	speculativeLintEnvironment,
) (linter.Generation, linter.ReleaseFunc, error)

// speculativeLintWorkspace adapts request-local editor text to the linter's
// autofix port. Commits only advance currentContent; the TypeScript Session,
// open-document store, and diagnostics cache are never mutated.
type speculativeLintWorkspace struct {
	uri             lsproto.DocumentUri
	targetPath      string
	snapshot        documentLintSnapshot
	environment     speculativeLintEnvironment
	acquire         speculativeGenerationAcquire
	originalContent string
	currentContent  string
}

func (s *Server) newSpeculativeLintWorkspace(
	uri lsproto.DocumentUri,
	originalContent string,
	snapshot documentLintSnapshot,
) *speculativeLintWorkspace {
	acquire := speculativeGenerationAcquire(acquireSpeculativeLintGeneration)
	if s.speculativeGeneration != nil {
		acquire = s.speculativeGeneration
	}
	return &speculativeLintWorkspace{
		uri:             uri,
		targetPath:      snapshot.target.Path,
		snapshot:        snapshot,
		environment:     s.freezeSpeculativeLintEnvironment(uri, snapshot.target),
		acquire:         acquire,
		originalContent: originalContent,
		currentContent:  originalContent,
	}
}

func (w *speculativeLintWorkspace) AcquireGeneration(ctx context.Context) (linter.Generation, linter.ReleaseFunc, error) {
	if w == nil || w.acquire == nil {
		return linter.Generation{}, nil, fmt.Errorf("LSP fix workspace for %s is not configured", w.uri)
	}
	return w.acquire(ctx, w.currentContent, w.snapshot, w.environment)
}

func (w *speculativeLintWorkspace) ApplyChanges(
	ctx context.Context,
	changes []linter.FileChange,
) (linter.ApplyResult, error) {
	applyResult := linter.ApplyResult{}
	if len(changes) == 0 {
		return applyResult, nil
	}
	if err := ctx.Err(); err != nil {
		return linter.ApplyResult{}, err
	}
	if len(changes) != 1 || changes[0].Path != w.targetPath {
		return linter.ApplyResult{}, fmt.Errorf("invalid LSP fix change set for %s", w.uri)
	}
	change := changes[0]
	if change.Before != w.currentContent {
		return linter.ApplyResult{}, fmt.Errorf("stale LSP fix change for %s", w.uri)
	}
	w.currentContent = change.After
	applyResult.ConfirmedPaths = []string{change.Path}
	applyResult.RestoredInitial = w.currentContent == w.originalContent
	return applyResult, nil
}
