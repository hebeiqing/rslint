package linter

import (
	"errors"
	"fmt"
	"sort"

	"github.com/web-infra-dev/rslint/internal/rule"
)

// FileChange is a pure whole-file change. Persistence, aliases, invalidation,
// rollback, and protocol projection belong to ChangeApplier implementations.
type FileChange struct {
	Path               string
	Before             string
	After              string
	AppliedDiagnostics int
}

// ApplyResult identifies only paths whose complete planned After text is known
// to have committed. Attempted paths and applied diagnostic counts are derived
// by the pipeline from its immutable plan rather than trusted to persistence.
type ApplyResult struct {
	ConfirmedPaths  []string
	RestoredInitial bool
}

type fixBacking map[string]string

func planFixes(diagnostics []rule.RuleDiagnostic, backing fixBacking) ([]FileChange, error) {
	byPath := make(map[string][]rule.RuleDiagnostic)
	for _, diagnostic := range diagnostics {
		if len(diagnostic.Fixes()) == 0 {
			continue
		}
		if diagnostic.FilePath == "" {
			return nil, errors.New("linter pipeline: fix diagnostic path must not be empty")
		}
		byPath[diagnostic.FilePath] = append(byPath[diagnostic.FilePath], cloneDiagnosticFixes(diagnostic))
	}
	paths := make([]string, 0, len(byPath))
	for path := range byPath {
		paths = append(paths, path)
	}
	sort.Strings(paths)

	changes := make([]FileChange, 0, len(paths))
	for _, path := range paths {
		text, ok := backing[path]
		if !ok {
			return nil, fmt.Errorf("linter pipeline: fix target %q has no frozen backing text", path)
		}
		fixed, unapplied, changed := ApplyRuleFixes(text, byPath[path])
		if !changed {
			continue
		}
		changes = append(changes, FileChange{
			Path:               path,
			Before:             text,
			After:              fixed,
			AppliedDiagnostics: len(byPath[path]) - len(unapplied),
		})
	}
	return changes, nil
}

func cloneDiagnosticFixes(diagnostic rule.RuleDiagnostic) rule.RuleDiagnostic {
	if diagnostic.FixesPtr == nil {
		return diagnostic
	}
	fixes := append([]rule.RuleFix(nil), (*diagnostic.FixesPtr)...)
	diagnostic.FixesPtr = &fixes
	return diagnostic
}

func validateApplyResult(
	planned []FileChange,
	result ApplyResult,
	applyErr error,
) (FixRoundResult, error) {
	plannedByPath := make(map[string]FileChange, len(planned))
	attempted := make([]string, 0, len(planned))
	for _, change := range planned {
		if change.Path == "" {
			return FixRoundResult{}, errors.Join(applyErr, errors.New("linter pipeline: planned change path must not be empty"))
		}
		if _, duplicate := plannedByPath[change.Path]; duplicate {
			return FixRoundResult{}, errors.Join(applyErr, fmt.Errorf("linter pipeline: duplicate planned change %q", change.Path))
		}
		plannedByPath[change.Path] = change
		attempted = append(attempted, change.Path)
	}

	round := FixRoundResult{
		AttemptedPaths: attempted,
	}
	confirmed := make(map[string]struct{}, len(result.ConfirmedPaths))
	var contractErrors []error
	for _, path := range result.ConfirmedPaths {
		if path == "" {
			contractErrors = append(contractErrors, errors.New("linter pipeline: confirmed path must not be empty"))
			continue
		}
		if _, duplicate := confirmed[path]; duplicate {
			contractErrors = append(contractErrors, fmt.Errorf("linter pipeline: duplicate confirmed path %q", path))
			continue
		}
		change, plannedPath := plannedByPath[path]
		if !plannedPath {
			contractErrors = append(contractErrors, fmt.Errorf("linter pipeline: confirmed path %q was not planned", path))
			continue
		}
		confirmed[path] = struct{}{}
		round.ConfirmedPaths = append(round.ConfirmedPaths, path)
		round.AppliedDiagnostics += change.AppliedDiagnostics
	}
	if applyErr == nil && len(confirmed) != len(plannedByPath) {
		contractErrors = append(contractErrors, fmt.Errorf(
			"linter pipeline: change applier confirmed %d of %d paths without an error",
			len(confirmed),
			len(plannedByPath),
		))
	}
	if applyErr != nil && len(confirmed) == len(plannedByPath) {
		contractErrors = append(contractErrors, errors.New(
			"linter pipeline: change applier returned an error after confirming the complete change set",
		))
	}
	if result.RestoredInitial && (applyErr != nil || len(confirmed) != len(plannedByPath)) {
		contractErrors = append(contractErrors, errors.New(
			"linter pipeline: restored-initial proof requires a complete successful commit",
		))
	}
	validationErr := errors.Join(append([]error{applyErr}, contractErrors...)...)
	if validationErr == nil {
		round.RestoredInitial = result.RestoredInitial
	}
	return round, validationErr
}
