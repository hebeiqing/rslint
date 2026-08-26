package linter

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/web-infra-dev/rslint/internal/rule"
)

type observationExecution struct {
	observation   ObservationResult
	fixBacking    fixBacking
	pluginOutcome *EslintPluginDispatchOutcome
	deferredTask  *pluginTask
}

func executeObservation(
	ctx context.Context,
	provider GenerationProvider,
	policy ObservationPolicy,
	dispatcher EslintPluginDispatcher,
	index int,
	planChanges bool,
	stopBeforePluginOnTargetSyntaxErrors bool,
) (observationExecution, error) {
	generation, release, err := provider.AcquireGeneration(ctx)
	if err != nil {
		return observationExecution{}, err
	}
	lease := &releaseLease{release: release}
	defer lease.close()
	generation = clonePipelineGeneration(generation)
	if err := ctx.Err(); err != nil {
		return observationExecution{}, err
	}

	plan, err := PrepareLintPlanContext(ctx, generation.runLinterOptions())
	if err != nil {
		return observationExecution{}, fmt.Errorf("linter pipeline: prepare lint plan: %w", err)
	}
	detachedPlugin := policy.Plugin != PluginConcurrentJoined
	pluginTask, err := materializePluginTask(plan, generation, policy, detachedPlugin)
	if err != nil {
		return observationExecution{}, err
	}
	if len(pluginTask.inputs) > 0 && policy.Plugin != pluginProgressiveAfterNative && dispatcher == nil {
		return observationExecution{}, errors.New("linter pipeline: joined plugin work requires a dispatcher")
	}
	execution := observationExecution{
		observation: ObservationResult{Index: index},
	}
	switch policy.Plugin {
	case PluginConcurrentJoined:
		execution, runErr := executeConcurrentObservation(
			ctx,
			generation,
			plan,
			pluginTask,
			dispatcher,
			execution,
			policy.Demand.Native,
		)
		if runErr == nil && planChanges {
			diagnostics, _ := execution.observation.CompleteDiagnostics()
			execution.fixBacking, runErr = freezeFixBackingForDiagnostics(
				ctx,
				generation,
				diagnostics,
			)
		}
		lease.close()
		return execution, joinContextError(runErr, ctx)
	case PluginAfterNativeJoined:
		native, nativeErr := runNativeObservation(ctx, generation, plan, policy.Demand.Native)
		execution.observation.Native = native
		if nativeErr != nil {
			lease.close()
			return execution, nativeErr
		}
		if stopBeforePluginOnTargetSyntaxErrors && native.HasTargetSyntaxErrors {
			pluginTask.fixCandidates = nil
			lease.close()
			execution.observation.pluginKind = pluginObservationNone
			return execution, ctx.Err()
		}
		if planChanges {
			candidateSources, sourceErr := fixSourcesFromDiagnostics(native.Diagnostics)
			if sourceErr != nil {
				return execution, sourceErr
			}
			if pluginTask.collectFixes {
				for _, candidate := range pluginTask.fixCandidates {
					if candidate.path == "" {
						return execution, errors.New("linter pipeline: plugin fix target path must not be empty")
					}
					if previous, duplicate := candidateSources[candidate.path]; duplicate && previous != candidate.source {
						return execution, fmt.Errorf("linter pipeline: duplicate fix target %q", candidate.path)
					}
					candidateSources[candidate.path] = candidate.source
				}
			}
			execution.fixBacking, err = freezeFixBackingSources(ctx, generation, candidateSources)
			if err != nil {
				return execution, err
			}
		}
		pluginTask.fixCandidates = nil
		// Detached plugin inputs and fix backing no longer reference generation
		// state, so watcher/Program resources are released before a reverse request
		// can block.
		lease.close()
		if err := ctx.Err(); err != nil {
			return execution, err
		}
		if len(pluginTask.inputs) == 0 {
			execution.observation.pluginKind = pluginObservationNone
			return execution, nil
		}
		outcome := pluginTask.run(ctx, dispatcher)
		execution.observation.pluginKind = pluginObservationJoined
		execution.observation.pluginOutcome = outcome
		execution.pluginOutcome = &execution.observation.pluginOutcome
		if err := ctx.Err(); err != nil {
			return execution, err
		}
		if planChanges {
			diagnostics, _ := execution.observation.CompleteDiagnostics()
			execution.fixBacking, err = retainFixBackingForDiagnostics(
				execution.fixBacking,
				diagnostics,
			)
			if err != nil {
				return execution, err
			}
		}
		return execution, nil
	case pluginProgressiveAfterNative:
		native, nativeErr := runNativeObservation(ctx, generation, plan, policy.Demand.Native)
		execution.observation.Native = native
		// Clear the last SourceFile-bearing side channel before releasing the
		// generation. The detached input itself was already deep-frozen above.
		pluginTask.fixCandidates = nil
		lease.close()
		if nativeErr != nil {
			return execution, nativeErr
		}
		if err := ctx.Err(); err != nil {
			return execution, err
		}
		if stopBeforePluginOnTargetSyntaxErrors && native.HasTargetSyntaxErrors {
			execution.observation.pluginKind = pluginObservationNone
			return execution, nil
		}
		if len(pluginTask.inputs) == 0 {
			execution.observation.pluginKind = pluginObservationNone
			return execution, nil
		}
		execution.observation.pluginKind = pluginObservationProgressive
		execution.deferredTask = &pluginTask
		return execution, nil
	default:
		return execution, errors.New("linter pipeline: plugin execution policy is invalid")
	}
}

func executeConcurrentObservation(
	ctx context.Context,
	generation Generation,
	plan *LintPlan,
	pluginTask pluginTask,
	dispatcher EslintPluginDispatcher,
	execution observationExecution,
	nativeDemand rule.EditDemand,
) (observationExecution, error) {
	var (
		pluginCh     <-chan EslintPluginDispatchOutcome
		cancelPlugin context.CancelFunc
		pluginJoined bool
	)
	if len(pluginTask.inputs) > 0 {
		pluginCtx, cancel := context.WithCancel(ctx)
		cancelPlugin = cancel
		ch := make(chan EslintPluginDispatchOutcome, 1)
		pluginCh = ch
		go func() {
			ch <- pluginTask.run(pluginCtx, dispatcher)
		}()
	}
	// This defer runs before executeObservation's lease defer, so panic,
	// cancellation, and native errors cancel and join plugin work before release.
	defer func() {
		if cancelPlugin != nil {
			cancelPlugin()
		}
		if pluginCh != nil && !pluginJoined {
			<-pluginCh
		}
	}()

	native, nativeErr := runNativeObservation(ctx, generation, plan, nativeDemand)
	execution.observation.Native = native
	if nativeErr != nil && cancelPlugin != nil {
		cancelPlugin()
	}
	if pluginCh != nil {
		outcome := <-pluginCh
		pluginJoined = true
		execution.observation.pluginKind = pluginObservationJoined
		execution.observation.pluginOutcome = outcome
		execution.pluginOutcome = &execution.observation.pluginOutcome
	} else {
		execution.observation.pluginKind = pluginObservationNone
	}
	if cancelPlugin != nil {
		cancelPlugin()
	}
	return execution, joinContextError(nativeErr, ctx)
}

func runNativeObservation(
	ctx context.Context,
	generation Generation,
	plan *LintPlan,
	demand rule.EditDemand,
) (NativeObservation, error) {
	if err := ctx.Err(); err != nil {
		return NativeObservation{}, err
	}
	diagnostics := plan.SyntacticDiagnostics(generation.Native.TypeCheck)
	runOptions := generation.runLinterOptions()
	runOptions.PreparedPlan = plan
	consumer := rule.DiagnosticConsumer{Demand: demand}
	var diagnosticsWait sync.WaitGroup
	finishDiagnostics := func() {}
	if runOptions.SingleThreaded {
		consumer.Report = func(diagnostic rule.RuleDiagnostic) {
			diagnostics = append(diagnostics, diagnostic)
		}
	} else {
		diagnosticsChannel := make(chan rule.RuleDiagnostic, 4096)
		diagnosticsWait.Add(1)
		go func() {
			defer diagnosticsWait.Done()
			for diagnostic := range diagnosticsChannel {
				diagnostics = append(diagnostics, diagnostic)
			}
		}()
		consumer.Report = func(diagnostic rule.RuleDiagnostic) {
			diagnosticsChannel <- diagnostic
		}
		finishDiagnostics = func() {
			close(diagnosticsChannel)
			diagnosticsWait.Wait()
		}
	}
	var finishOnce sync.Once
	finish := func() { finishOnce.Do(finishDiagnostics) }
	defer finish()
	runOptions.Consumer = consumer
	lintResult, err := RunLinter(runOptions)
	finish()

	for index := range diagnostics {
		diagnostics[index].FilePath = projectTargetPath(generation.Target.Path, diagnostics[index].FilePath)
	}
	files := plan.Files()
	lintedFiles := make([]LintedFile, 0, len(files))
	for _, file := range files {
		if file == nil {
			continue
		}
		lintedFiles = append(lintedFiles, LintedFile{
			Path:       projectTargetPath(generation.Target.Path, file.FileName()),
			SourceFile: file,
		})
	}
	result := NativeObservation{
		Diagnostics:           diagnostics,
		Lint:                  lintResult,
		Files:                 lintedFiles,
		HasTargetSyntaxErrors: plan.HasSyntacticDiagnostics(),
	}
	return result, joinContextError(err, ctx)
}

func freezeFixBackingForDiagnostics(
	ctx context.Context,
	generation Generation,
	diagnostics []rule.RuleDiagnostic,
) (fixBacking, error) {
	sources, err := fixSourcesFromDiagnostics(diagnostics)
	if err != nil {
		return nil, err
	}
	return freezeFixBackingSources(ctx, generation, sources)
}

func freezeFixBackingSources(
	ctx context.Context,
	generation Generation,
	sources map[string]ast.SourceFileLike,
) (fixBacking, error) {
	if len(sources) == 0 {
		return fixBacking{}, nil
	}
	if generation.Target.ReadFixText == nil {
		return nil, errors.New("linter pipeline: fix planning requires a target text reader")
	}
	backing := make(fixBacking, len(sources))
	paths := make([]string, 0, len(sources))
	for path := range sources {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := sources[path]
		if source == nil {
			return nil, fmt.Errorf("linter pipeline: fix target %q has no source frame", path)
		}
		text, err := generation.Target.ReadFixText(path, source)
		if err != nil {
			return nil, fmt.Errorf("linter pipeline: read fix target %q: %w", path, err)
		}
		backing[path] = text
	}
	return backing, ctx.Err()
}

func retainFixBackingForDiagnostics(
	backing fixBacking,
	diagnostics []rule.RuleDiagnostic,
) (fixBacking, error) {
	paths, err := fixTargetPaths(diagnostics)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return fixBacking{}, nil
	}
	result := make(fixBacking, len(paths))
	for path := range paths {
		text, ok := backing[path]
		if !ok {
			return nil, fmt.Errorf("linter pipeline: fix target %q has no frozen backing text", path)
		}
		result[path] = text
	}
	return result, nil
}

func fixTargetPaths(diagnostics []rule.RuleDiagnostic) (map[string]struct{}, error) {
	paths := make(map[string]struct{})
	for _, diagnostic := range diagnostics {
		if len(diagnostic.Fixes()) == 0 {
			continue
		}
		if diagnostic.FilePath == "" {
			return nil, errors.New("linter pipeline: fix diagnostic path must not be empty")
		}
		paths[diagnostic.FilePath] = struct{}{}
	}
	return paths, nil
}

func fixSourcesFromDiagnostics(
	diagnostics []rule.RuleDiagnostic,
) (map[string]ast.SourceFileLike, error) {
	sources := make(map[string]ast.SourceFileLike)
	for _, diagnostic := range diagnostics {
		if len(diagnostic.Fixes()) == 0 {
			continue
		}
		if diagnostic.FilePath == "" {
			return nil, errors.New("linter pipeline: fix diagnostic path must not be empty")
		}
		if diagnostic.SourceFile == nil {
			return nil, fmt.Errorf("linter pipeline: fix target %q has no source frame", diagnostic.FilePath)
		}
		if previous, duplicate := sources[diagnostic.FilePath]; duplicate && previous != diagnostic.SourceFile {
			return nil, fmt.Errorf("linter pipeline: duplicate fix target %q", diagnostic.FilePath)
		}
		sources[diagnostic.FilePath] = diagnostic.SourceFile
	}
	return sources, nil
}

func joinContextError(err error, ctx context.Context) error {
	contextErr := ctx.Err()
	if contextErr == nil || errors.Is(err, contextErr) {
		return err
	}
	return errors.Join(err, contextErr)
}

func (generation Generation) runLinterOptions() RunLinterOptions {
	native := generation.Native
	return RunLinterOptions{
		Programs:        native.Programs,
		SingleThreaded:  native.SingleThreaded,
		Cwd:             native.Cwd,
		TargetFiles:     native.Targets,
		GetRulesForFile: native.RulesForFile,
		TypeCheck:       native.TypeCheck,
		Timing:          native.Timing,
	}
}

func clonePipelineGeneration(generation Generation) Generation {
	generation.Native.Programs = clonePipelineSlice(generation.Native.Programs)
	if generation.Native.Targets != nil {
		targets := generation.Native.Targets
		generation.Native.Targets = make([][]string, len(targets))
		for index := range targets {
			generation.Native.Targets[index] = clonePipelineSlice(targets[index])
		}
	}
	if generation.Plugin != nil {
		plugin := *generation.Plugin
		generation.Plugin = &plugin
	}
	return generation
}

func clonePipelineSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}

func projectTargetPath(project func(string) string, sourcePath string) string {
	if project == nil {
		return sourcePath
	}
	return project(sourcePath)
}
