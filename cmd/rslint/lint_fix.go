package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/microsoft/typescript-go/shim/bundled"
	"github.com/microsoft/typescript-go/shim/vfs/osvfs"
	"github.com/web-infra-dev/rslint/internal/linter"
	"github.com/web-infra-dev/rslint/internal/utils"
)

type cliAutofixWorkspace struct {
	*cliGenerationProvider
	sourceGeneration sourceGenerationInvalidator
}

type sourceGenerationInvalidator interface {
	InvalidateSourceSnapshots()
}

// ApplyChanges is the physical-disk adapter for the shared autofix lifecycle.
// Multi-file writes are deliberately best-effort rather than falsely atomic:
// every independent file is attempted, successful writes are retained, and
// errors are joined. Any non-empty attempt invalidates the loader's complete
// source generation before a possible rebuild, including partial failures.
func (w *cliAutofixWorkspace) ApplyChanges(
	ctx context.Context,
	changes []linter.FileChange,
) (linter.ApplyResult, error) {
	applyResult := linter.ApplyResult{}
	if len(changes) == 0 {
		return applyResult, nil
	}

	var writeErrors []error
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			writeErrors = append(writeErrors, err)
			break
		}
		current, ok := readPhysicalFixSource(change.Path)
		if !ok {
			writeErrors = append(writeErrors, fmt.Errorf("read fix target %q", change.Path))
			continue
		}
		if current != change.Before {
			writeErrors = append(writeErrors, fmt.Errorf("fix target %q changed after lint generation", change.Path))
			continue
		}
		if err := ctx.Err(); err != nil {
			writeErrors = append(writeErrors, err)
			break
		}
		if err := os.WriteFile(change.Path, []byte(change.After), 0644); err != nil {
			writeErrors = append(writeErrors, fmt.Errorf("write fixed file %q: %w", change.Path, err))
			continue
		}
		applyResult.ConfirmedPaths = append(applyResult.ConfirmedPaths, change.Path)
	}
	if len(changes) > 0 {
		w.sourceGeneration.InvalidateSourceSnapshots()
	}
	return applyResult, errors.Join(writeErrors...)
}

func readPhysicalFixSource(path string) (string, bool) {
	fsys := bundled.WrapFS(osvfs.FS())
	text, ok := fsys.ReadFile(path)
	if !ok {
		return "", false
	}
	if utils.SourceHasBOM(fsys, path) {
		text = utils.BOM + text
	}
	return text, true
}
