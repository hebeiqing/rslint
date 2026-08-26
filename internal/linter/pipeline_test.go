package linter

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/microsoft/typescript-go/shim/bundled"
	"github.com/microsoft/typescript-go/shim/core"
	"github.com/microsoft/typescript-go/shim/tspath"
	"github.com/microsoft/typescript-go/shim/vfs/osvfs"

	"github.com/web-infra-dev/rslint/internal/program"
	"github.com/web-infra-dev/rslint/internal/rule"
	"github.com/web-infra-dev/rslint/internal/utils"
)

func pipelineTestProgram(t *testing.T, root string, fileName string, text string) *program.Program {
	t.Helper()
	fs := utils.NewOverlayVFS(bundled.WrapFS(osvfs.FS()), map[string]string{fileName: text})
	result, err := program.NewFromRoots(program.RootOptions{
		RootFileNames:   []string{fileName},
		Host:            utils.CreateCompilerHost(root, fs),
		CompilerOptions: &core.CompilerOptions{},
		SingleThreaded:  true,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func pipelineTestGeneration(
	t *testing.T,
	root string,
	fileName string,
	text string,
	configuredRules []rule.ConfiguredRule,
	pluginConfig *EslintPluginFileConfig,
) Generation {
	t.Helper()
	result := Generation{
		Native: NativeGeneration{
			Programs:       []*program.Program{pipelineTestProgram(t, root, fileName, text)},
			Targets:        [][]string{{fileName}},
			SingleThreaded: true,
			Cwd:            root,
			RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
				return configuredRules
			},
		},
		Target: TargetProjection{
			Path: func(string) string { return fileName },
			ReadFixText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			},
		},
	}
	if pluginConfig != nil {
		result.Plugin = &PluginGeneration{
			ConfigForFile: func(string) EslintPluginFileConfig { return *pluginConfig },
			InlineText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			},
		}
	}
	return result
}

func pipelineTestProvider(generation Generation, release ReleaseFunc) GenerationProvider {
	return GenerationProviderFunc(func(context.Context) (Generation, ReleaseFunc, error) {
		return generation, release, nil
	})
}

type pipelineProgressiveDiagnostics struct {
	baseline  []rule.RuleDiagnostic
	parentCtx context.Context
	run       DeferredPluginRun
	onPublish func()
	onSubmit  func()
}

func (p *pipelineProgressiveDiagnostics) PublishBaseline(
	_ context.Context,
	diagnostics []rule.RuleDiagnostic,
) {
	if p.onPublish != nil {
		p.onPublish()
	}
	p.baseline = append([]rule.RuleDiagnostic(nil), diagnostics...)
}

func (p *pipelineProgressiveDiagnostics) Submit(parentCtx context.Context, run DeferredPluginRun) {
	if p.onSubmit != nil {
		p.onSubmit()
	}
	p.parentCtx = parentCtx
	p.run = run
}

func TestPipelineReleasesGenerationOnPreparationFailureAndPanic(t *testing.T) {
	t.Run("error", func(t *testing.T) {
		var releases atomic.Int32
		_, err := RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(Generation{Native: NativeGeneration{
				Programs: []*program.Program{nil},
				RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
					return nil
				},
			}}, func() {
				releases.Add(1)
			}),
			ObservationPolicy{},
			nil,
		))
		if err == nil || releases.Load() != 1 {
			t.Fatalf("error/releases = %v/%d, want error/1", err, releases.Load())
		}
	})

	t.Run("panic", func(t *testing.T) {
		root := tspath.NormalizePath(t.TempDir())
		fileName := tspath.ResolvePath(root, "source.ts")
		generation := pipelineTestGeneration(t, root, fileName, "const value = 1;", nil, nil)
		generation.Native.RulesForFile = func(*ast.SourceFile) []rule.ConfiguredRule {
			panic("resolver failed")
		}
		var releases atomic.Int32
		var recovered any
		func() {
			defer func() { recovered = recover() }()
			_, _ = RunPipeline(context.Background(), NewLintRequest(
				pipelineTestProvider(generation, func() { releases.Add(1) }),
				ObservationPolicy{},
				nil,
			))
		}()
		if recovered == nil || releases.Load() != 1 {
			t.Fatalf("panic/releases = %v/%d, want panic/1", recovered, releases.Load())
		}
	})
}

func TestPipelineGenerationSnapshotClonesSlicesAndPreservesShapes(t *testing.T) {
	programs := []*program.Program{nil}
	targets := [][]string{{"source.ts"}}
	snapshot := clonePipelineGeneration(Generation{Native: NativeGeneration{
		Programs: programs,
		Targets:  targets,
	}})
	programs[0] = &program.Program{}
	targets[0][0] = "changed.ts"
	if snapshot.Native.Programs[0] != nil || snapshot.Native.Targets[0][0] != "source.ts" {
		t.Fatalf("generation snapshot retained mutable slices: %+v", snapshot.Native)
	}

	emptyPrograms := make([]*program.Program, 0)
	emptyTargets := make([][]string, 0)
	emptyInner := make([]string, 0)
	empty := clonePipelineGeneration(Generation{Native: NativeGeneration{
		Programs: emptyPrograms,
		Targets:  emptyTargets,
	}})
	inner := clonePipelineGeneration(Generation{Native: NativeGeneration{
		Targets: [][]string{emptyInner, nil},
	}})
	nilGeneration := clonePipelineGeneration(Generation{})
	if empty.Native.Programs == nil || empty.Native.Targets == nil {
		t.Fatal("non-nil empty generation slices became nil")
	}
	if inner.Native.Targets[0] == nil || inner.Native.Targets[1] != nil {
		t.Fatal("inner target nil shape changed while snapshotting")
	}
	if nilGeneration.Native.Programs != nil || nilGeneration.Native.Targets != nil {
		t.Fatal("nil generation slices became non-nil")
	}
}

func TestPipelineConcurrentPluginStartsBeforeNativeAndPlansProjectedFix(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	projectedPath := "target:" + fileName
	pluginStarted := make(chan struct{})
	allowPluginFinish := make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	nativeRan := false
	configuredRules := []rule.ConfiguredRule{
		{
			Name:     "native/order",
			Severity: rule.SeverityWarning,
			Run: func(rule.RuleContext) rule.RuleListeners {
				select {
				case <-pluginStarted:
				case <-ctx.Done():
					t.Fatal("native work started before plugin dispatch")
				}
				nativeRan = true
				close(allowPluginFinish)
				return nil
			},
		},
		{Name: "plugin/replace", Severity: rule.SeverityError, IsEslintPluginRule: true},
	}
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		configuredRules,
		&EslintPluginFileConfig{ConfigKey: "config"},
	)
	generation.Target.Path = func(string) string { return projectedPath }
	result, err := RunPipeline(ctx, NewPlanOnceRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{
			Demand: ArtifactDemand{Native: rule.EditDemandAutofix, Plugin: rule.EditDemandAutofix},
			Plugin: PluginConcurrentJoined,
		},
		func(_ context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if request.Files[0].Path != fileName {
				t.Fatalf("plugin wire path = %q, want Program source path %q", request.Files[0].Path, fileName)
			}
			close(pluginStarted)
			<-allowPluginFinish
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{
				FilePath: request.Files[0].Path,
				Diagnostics: []EslintPluginDiagnostic{{
					RuleName: "plugin/replace",
					Message:  "replace",
					StartPos: 0,
					EndPos:   1,
					Fixes:    []EslintPluginFix{{Range: [2]int{0, 1}, Text: "b"}},
				}},
			}}}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	diagnostics, complete := result.Observation.CompleteDiagnostics()
	changes, planned := result.Fix.PlannedChanges()
	if !nativeRan || !complete || len(diagnostics) != 1 || diagnostics[0].FilePath != projectedPath {
		t.Fatalf("observation = native:%v complete:%v diagnostics:%+v", nativeRan, complete, diagnostics)
	}
	if !planned || len(changes) != 1 || changes[0].Path != projectedPath || changes[0].Before != "a" || changes[0].After != "b" {
		t.Fatalf("planned changes = %+v, planned=%v", changes, planned)
	}
}

func TestPipelineJoinedModesPropagateCallerCancellation(t *testing.T) {
	for _, mode := range []PluginExecution{PluginConcurrentJoined, PluginAfterNativeJoined} {
		t.Run(fmt.Sprintf("mode-%d", mode), func(t *testing.T) {
			root := tspath.NormalizePath(t.TempDir())
			fileName := tspath.ResolvePath(root, "source.ts")
			nativeFinished := make(chan struct{})
			generation := pipelineTestGeneration(
				t,
				root,
				fileName,
				"a",
				[]rule.ConfiguredRule{
					{
						Name: "native/marker",
						Run: func(rule.RuleContext) rule.RuleListeners {
							close(nativeFinished)
							return nil
						},
					},
					{Name: "plugin/check", IsEslintPluginRule: true},
				},
				&EslintPluginFileConfig{},
			)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result, err := RunPipeline(ctx, NewLintRequest(
				pipelineTestProvider(generation, nil),
				ObservationPolicy{
					Demand:        ArtifactDemand{Plugin: rule.EditDemandAll},
					Plugin:        mode,
					PluginFailure: PluginDiscardOnFailure,
				},
				func(dispatchCtx context.Context, _ EslintPluginLintRequest) (*EslintPluginLintResult, error) {
					<-nativeFinished
					cancel()
					return nil, dispatchCtx.Err()
				},
			))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pipeline error = %v, want context.Canceled", err)
			}
			records := result.PluginOutcomes()
			if len(records) != 1 || records[0].Observation != 0 || !errors.Is(records[0].DispatchError, context.Canceled) {
				t.Fatalf("plugin records = %+v, want canceled observation 0", records)
			}
			if result.Observation.Native.Lint != nil {
				t.Fatal("canceled observation was published as authoritative")
			}
		})
	}
}

func TestPipelineDoesNotPromoteIndependentPluginBudgetFailure(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	result, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			return nil, context.DeadlineExceeded
		},
	))
	if err != nil {
		t.Fatalf("pipeline promoted an independent plugin budget failure: %v", err)
	}
	outcome, ok := result.Observation.JoinedPluginOutcome()
	if !ok || !errors.Is(outcome.DispatchError, context.DeadlineExceeded) {
		t.Fatalf("joined plugin outcome = %+v, want deadline failure", outcome)
	}
}

func TestPipelineRejectsMissingJoinedPluginDispatcher(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "requires a dispatcher") {
		t.Fatalf("missing dispatcher error = %v", err)
	}
}

func TestPipelineRejectsDuplicatePluginWirePath(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	generation := Generation{
		Native: NativeGeneration{
			Programs: []*program.Program{
				pipelineTestProgram(t, root, firstPath, "a"),
				pipelineTestProgram(t, root, secondPath, "b"),
			},
			Targets:        [][]string{{firstPath}, {secondPath}},
			SingleThreaded: true,
			Cwd:            root,
			RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
				return []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
			},
		},
		Plugin: &PluginGeneration{
			WirePath: func(string) string { return "shared.ts" },
		},
	}
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			return &EslintPluginLintResult{}, nil
		},
	))
	if err == nil || !strings.Contains(err.Error(), "duplicate plugin wire path") {
		t.Fatalf("duplicate wire path error = %v", err)
	}
}

func TestPipelineRejectsDistinctFixSourcesForOneTarget(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	firstPath := tspath.ResolvePath(root, "first.ts")
	secondPath := tspath.ResolvePath(root, "second.ts")
	generation := Generation{
		Native: NativeGeneration{
			Programs: []*program.Program{
				pipelineTestProgram(t, root, firstPath, "a"),
				pipelineTestProgram(t, root, secondPath, "b"),
			},
			Targets:        [][]string{{firstPath}, {secondPath}},
			SingleThreaded: true,
			Cwd:            root,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				return []rule.ConfiguredRule{{
					Name: "native/fix",
					Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						textRange := core.NewTextRange(0, len(source.Text()))
						ruleCtx.ReportRangeWithFixes(
							textRange,
							rule.RuleMessage{Description: "fix"},
							rule.RuleFix{Range: textRange, Text: "fixed"},
						)
						return nil
					},
				}}
			},
		},
		Target: TargetProjection{
			Path: func(string) string { return "shared.ts" },
			ReadFixText: func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			},
		},
	}
	_, err := RunPipeline(context.Background(), NewPlanOnceRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "duplicate fix target") {
		t.Fatalf("duplicate fix target error = %v", err)
	}
}

func TestProgressivePipelineChecksCancellationAfterRelease(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	presentation := &pipelineProgressiveDiagnostics{}
	result, err := RunPipeline(ctx, NewProgressiveLintRequest(
		pipelineTestProvider(generation, ReleaseFunc(cancel)),
		ArtifactDemand{},
		presentation,
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want cancellation raised by release", err)
	}
	if result.Observation.Native.Lint != nil {
		t.Fatal("post-release canceled observation was published")
	}
	if presentation.baseline != nil || presentation.run != nil {
		t.Fatal("canceled progressive result reached presentation ports")
	}
}

func TestProgressivePipelineOwnsReleasePresentationGateAndSubmissionOrder(t *testing.T) {
	t.Run("eligible enrichment", func(t *testing.T) {
		root := tspath.NormalizePath(t.TempDir())
		fileName := tspath.ResolvePath(root, "source.ts")
		generation := pipelineTestGeneration(
			t,
			root,
			fileName,
			"const value = 1;",
			[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
			&EslintPluginFileConfig{},
		)
		released := false
		presented := false
		presentation := &pipelineProgressiveDiagnostics{
			onPublish: func() {
				if !released {
					t.Fatal("baseline was published before generation release")
				}
				presented = true
			},
			onSubmit: func() {
				if !presented {
					t.Fatal("enrichment was submitted before baseline publication")
				}
			},
		}
		result, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
			pipelineTestProvider(generation, func() { released = true }),
			ArtifactDemand{Plugin: rule.EditDemandAll},
			presentation,
		))
		if err != nil {
			t.Fatal(err)
		}
		if presentation.run == nil || !released || !presented {
			t.Fatalf("run/released/presented = %v/%v/%v", presentation.run != nil, released, presented)
		}
		if _, complete := result.Observation.CompleteDiagnostics(); complete {
			t.Fatal("progressive result reported complete before enrichment")
		}
	})

	for _, test := range []struct {
		name         string
		text         string
		pluginConfig *EslintPluginFileConfig
		rules        []rule.ConfiguredRule
	}{
		{
			name:         "target syntax error",
			text:         "const value = ;",
			pluginConfig: &EslintPluginFileConfig{},
			rules:        []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		},
		{name: "no plugin work", text: "const value = 1;"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := tspath.NormalizePath(t.TempDir())
			fileName := tspath.ResolvePath(root, "source.ts")
			generation := pipelineTestGeneration(t, root, fileName, test.text, test.rules, test.pluginConfig)
			presentation := &pipelineProgressiveDiagnostics{}
			result, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
				pipelineTestProvider(generation, nil),
				ArtifactDemand{Plugin: rule.EditDemandAll},
				presentation,
			))
			if err != nil {
				t.Fatal(err)
			}
			if presentation.run != nil {
				t.Fatal("ineligible enrichment was submitted")
			}
			if _, complete := result.Observation.CompleteDiagnostics(); !complete {
				t.Fatal("baseline without enrichment was reported incomplete")
			}
		})
	}
}

func TestConcurrentPipelineChecksCancellationAfterRelease(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(t, root, fileName, "a", nil, nil)
	ctx, cancel := context.WithCancel(context.Background())
	result, err := RunPipeline(ctx, NewLintRequest(
		pipelineTestProvider(generation, ReleaseFunc(cancel)),
		ObservationPolicy{Plugin: PluginConcurrentJoined},
		nil,
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want cancellation raised by release", err)
	}
	if result.Observation.Native.Lint != nil {
		t.Fatal("post-release canceled observation was published")
	}
}

func TestPipelineAfterNativeReleasesBeforePluginDispatch(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}},
		&EslintPluginFileConfig{},
	)
	var releases atomic.Int32
	_, err := RunPipeline(context.Background(), NewLintRequest(
		pipelineTestProvider(generation, func() { releases.Add(1) }),
		ObservationPolicy{
			Demand:        ArtifactDemand{Plugin: rule.EditDemandAll},
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		func(_ context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if releases.Load() != 1 {
				t.Fatalf("release count at plugin dispatch = %d, want 1", releases.Load())
			}
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{FilePath: request.Files[0].Path}}}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	if releases.Load() != 1 {
		t.Fatalf("release calls = %d, want 1", releases.Load())
	}
}

func TestProgressivePluginRunIsFrozenAndSingleUse(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	settings := map[string]any{"value": "frozen"}
	options := []any{map[string]any{"choice": "frozen"}}
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"const value = 1;",
		[]rule.ConfiguredRule{{
			Name:               "plugin/check",
			IsEslintPluginRule: true,
			Options:            options,
		}},
		&EslintPluginFileConfig{Settings: settings},
	)
	var releases atomic.Int32
	presentation := &pipelineProgressiveDiagnostics{}
	_, err := RunPipeline(context.Background(), NewProgressiveLintRequest(
		pipelineTestProvider(generation, func() { releases.Add(1) }),
		ArtifactDemand{Plugin: rule.EditDemandAll},
		presentation,
	))
	if err != nil {
		t.Fatal(err)
	}
	if presentation.run == nil || releases.Load() != 1 {
		t.Fatalf("enrichment/release = %v/%d, want non-nil/1", presentation.run != nil, releases.Load())
	}
	settings["value"] = "mutated"
	options[0].(map[string]any)["choice"] = "mutated"
	var request EslintPluginLintRequest
	outcome, err := presentation.run(context.Background(), func(_ context.Context, got EslintPluginLintRequest) (*EslintPluginLintResult, error) {
		request = got
		return &EslintPluginLintResult{Results: []EslintPluginFileResult{{FilePath: got.Files[0].Path}}}, nil
	})
	if err != nil || outcome.DispatchError != nil {
		t.Fatalf("work errors = %v/%v", err, outcome.DispatchError)
	}
	frozenOptions := request.Rules["plugin/check"].Options
	if request.Files[0].Settings["value"] != "frozen" ||
		len(frozenOptions) != 1 || frozenOptions[0].(map[string]any)["choice"] != "frozen" {
		t.Fatalf("deferred request retained mutable config: %+v", request)
	}
	if _, err := presentation.run(context.Background(), func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
		return &EslintPluginLintResult{}, nil
	}); !errors.Is(err, ErrDeferredPluginRunAlreadyInvoked) {
		t.Fatalf("second run error = %v, want ErrDeferredPluginRunAlreadyInvoked", err)
	}
}

func TestConcurrentPipelineCancelsAndJoinsPluginBeforeReleaseOnNativePanic(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fileName := tspath.ResolvePath(root, "source.ts")
	pluginStarted := make(chan struct{})
	pluginStopped := make(chan struct{})
	generation := pipelineTestGeneration(
		t,
		root,
		fileName,
		"a",
		[]rule.ConfiguredRule{
			{
				Name: "native/panic",
				Run: func(rule.RuleContext) rule.RuleListeners {
					<-pluginStarted
					panic("native failed")
				},
			},
			{Name: "plugin/check", IsEslintPluginRule: true},
		},
		&EslintPluginFileConfig{},
	)
	var releases atomic.Int32
	var recovered any
	func() {
		defer func() { recovered = recover() }()
		_, _ = RunPipeline(context.Background(), NewLintRequest(
			pipelineTestProvider(generation, func() {
				select {
				case <-pluginStopped:
				default:
					t.Fatal("generation released before plugin dispatch joined")
				}
				releases.Add(1)
			}),
			ObservationPolicy{Plugin: PluginConcurrentJoined},
			func(pluginCtx context.Context, _ EslintPluginLintRequest) (*EslintPluginLintResult, error) {
				close(pluginStarted)
				<-pluginCtx.Done()
				close(pluginStopped)
				return nil, pluginCtx.Err()
			},
		))
	}()
	if recovered == nil || releases.Load() != 1 {
		t.Fatalf("panic/releases = %v/%d, want panic/1", recovered, releases.Load())
	}
}

type pipelineAutofixWorkspace struct {
	t            *testing.T
	root         string
	fileName     string
	current      string
	next         map[string]string
	acquisitions int
	applies      int
	apply        func([]FileChange) (ApplyResult, error)
	rules        func(string) []rule.ConfiguredRule
}

type pipelinePartialApplyWorkspace struct {
	t            *testing.T
	root         string
	paths        [2]string
	acquisitions int
}

func (w *pipelinePartialApplyWorkspace) AcquireGeneration(context.Context) (Generation, ReleaseFunc, error) {
	w.acquisitions++
	programs := make([]*program.Program, len(w.paths))
	targets := make([][]string, len(w.paths))
	for index, path := range w.paths {
		programs[index] = pipelineTestProgram(w.t, w.root, path, string(rune('a'+index)))
		targets[index] = []string{path}
	}
	return Generation{
		Native: NativeGeneration{
			Programs:       programs,
			Targets:        targets,
			SingleThreaded: true,
			Cwd:            w.root,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				return []rule.ConfiguredRule{{
					Name: "native/fix",
					Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						textRange := core.NewTextRange(0, len(source.Text()))
						ruleCtx.ReportRangeWithFixes(
							textRange,
							rule.RuleMessage{Description: "fix"},
							rule.RuleFix{Range: textRange, Text: "fixed"},
						)
						return nil
					},
				}}
			},
		},
		Target: TargetProjection{ReadFixText: func(_ string, source ast.SourceFileLike) (string, error) {
			return source.Text(), nil
		}},
	}, nil, nil
}

func (w *pipelinePartialApplyWorkspace) ApplyChanges(
	_ context.Context,
	changes []FileChange,
) (ApplyResult, error) {
	if len(changes) != 2 {
		return ApplyResult{}, fmt.Errorf("planned changes = %d, want 2", len(changes))
	}
	return ApplyResult{ConfirmedPaths: []string{changes[0].Path}}, errors.New("partial commit")
}

func (w *pipelineAutofixWorkspace) AcquireGeneration(context.Context) (Generation, ReleaseFunc, error) {
	w.acquisitions++
	configuredRules := []rule.ConfiguredRule{}
	if next, ok := w.next[w.current]; ok {
		before := w.current
		configuredRules = append(configuredRules, rule.ConfiguredRule{
			Name:     "native/fix",
			Severity: rule.SeverityError,
			Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
				rangeToFix := core.NewTextRange(0, len(before))
				ruleCtx.ReportRangeWithFixes(
					rangeToFix,
					rule.RuleMessage{Description: "fix"},
					rule.RuleFix{Range: rangeToFix, Text: next},
				)
				return nil
			},
		})
	}
	if w.rules != nil {
		configuredRules = append(configuredRules, w.rules(w.current)...)
	}
	var pluginConfig *EslintPluginFileConfig
	for _, configured := range configuredRules {
		if configured.IsEslintPluginRule {
			pluginConfig = &EslintPluginFileConfig{}
			break
		}
	}
	return pipelineTestGeneration(w.t, w.root, w.fileName, w.current, configuredRules, pluginConfig), nil, nil
}

func (w *pipelineAutofixWorkspace) ApplyChanges(_ context.Context, changes []FileChange) (ApplyResult, error) {
	w.applies++
	if w.apply != nil {
		return w.apply(changes)
	}
	if len(changes) != 1 || changes[0].Path != w.fileName || changes[0].Before != w.current {
		return ApplyResult{}, errors.New("unexpected change set")
	}
	w.current = changes[0].After
	return ApplyResult{
		ConfirmedPaths:  []string{changes[0].Path},
		RestoredInitial: w.current == "a",
	}, nil
}

func TestAutofixPipelineOwnsRoundsAndReobservesWorkspace(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "b", "b": "c"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: MaxFixRounds},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 2 || workspace.acquisitions != 3 || workspace.applies != 2 || workspace.current != "c" {
		t.Fatalf("result/workspace = %+v / %+v", applied, workspace)
	}
	if applied.Rounds[0].AppliedDiagnostics != 1 || applied.Rounds[1].AppliedDiagnostics != 1 {
		t.Fatalf("applied diagnostic counts = %+v", applied.Rounds)
	}
}

func TestAutofixSyntaxGateStopsBeforeAfterNativePluginDispatch(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "const value = ;",
		next:     map[string]string{},
		rules: func(string) []rule.ConfiguredRule {
			return []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
		},
	}
	dispatches := 0
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{
			Demand: ArtifactDemand{
				Native: rule.EditDemandAutofix,
				Plugin: rule.EditDemandAutofix,
			},
			Plugin:        PluginAfterNativeJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		AutofixPolicy{MaxRounds: MaxFixRounds, StopOnTargetSyntaxErrors: true},
		func(context.Context, EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			dispatches++
			return &EslintPluginLintResult{}, nil
		},
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 0 || !result.Observation.Native.HasTargetSyntaxErrors {
		t.Fatalf("syntax-gated applied result = %+v", applied)
	}
	if dispatches != 0 {
		t.Fatalf("plugin dispatches after target syntax error = %d, want 0", dispatches)
	}
}

func TestAutofixUsesProductRoundLimitThenVerifiesOnce(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	next := make(map[string]string, MaxFixRounds)
	for round := range MaxFixRounds {
		next[strconv.Itoa(round)] = strconv.Itoa(round + 1)
	}
	var fixArtifactBuilds atomic.Int32
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "0",
		next:     next,
		rules: func(string) []rule.ConfiguredRule {
			return []rule.ConfiguredRule{{
				Name: "native/demand-probe",
				Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
					probeRange := core.NewTextRange(0, 0)
					ruleCtx.ReportRangeWithDeferredFixes(
						probeRange,
						rule.RuleMessage{Description: "probe"},
						func() []rule.RuleFix {
							fixArtifactBuilds.Add(1)
							return nil
						},
					)
					return nil
				},
			}}
		},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{
			MaxRounds:            MaxFixRounds,
			VerifyAfterLastRound: true,
			VerificationDemand: ArtifactDemand{
				Native: rule.EditDemandSuggestion,
			},
		},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != MaxFixRounds || applied.Last.Index != MaxFixRounds {
		t.Fatalf("applied result = %+v", applied)
	}
	if workspace.applies != MaxFixRounds || workspace.acquisitions != MaxFixRounds+1 || workspace.current != strconv.Itoa(MaxFixRounds) {
		t.Fatalf("workspace applies/acquisitions/current = %d/%d/%q", workspace.applies, workspace.acquisitions, workspace.current)
	}
	if fixArtifactBuilds.Load() != MaxFixRounds {
		t.Fatalf("autofix artifact builds = %d, want %d; final verification demand was not isolated", fixArtifactBuilds.Load(), MaxFixRounds)
	}
}

func TestAutofixCanStopAtRoundLimitWithoutVerification(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "b", "b": "c", "c": "d"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 2},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || len(applied.Rounds) != 2 || applied.Last.Index != 1 {
		t.Fatalf("unverified applied result = %+v", applied)
	}
	if workspace.applies != 2 || workspace.acquisitions != 2 || workspace.current != "c" {
		t.Fatalf("workspace applies/acquisitions/current = %d/%d/%q", workspace.applies, workspace.acquisitions, workspace.current)
	}
}

func TestAutofixApplyErrorReturnsConfirmedStateWithoutReobserve(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelinePartialApplyWorkspace{
		t:     t,
		root:  root,
		paths: [2]string{tspath.ResolvePath(root, "first.ts"), tspath.ResolvePath(root, "second.ts")},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 2},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "partial commit") {
		t.Fatalf("apply error = %v", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || len(applied.Rounds) != 1 ||
		len(applied.Rounds[0].ConfirmedPaths) != 1 || applied.Rounds[0].ConfirmedPaths[0] != workspace.paths[0] ||
		applied.Rounds[0].AppliedDiagnostics != 1 {
		t.Fatalf("partial applied result = %+v", applied)
	}
	if workspace.acquisitions != 1 {
		t.Fatalf("acquisitions after apply error = %d, want 1", workspace.acquisitions)
	}
}

func TestAutofixPropagatesCancellationFromChangeApplier(t *testing.T) {
	tests := []struct {
		name              string
		restored          bool
		applyErr          error
		wantVerified      bool
		wantRestoredRound bool
	}{
		{name: "committed without verification"},
		{name: "restored initial", restored: true, wantVerified: true, wantRestoredRound: true},
		{name: "apply error", applyErr: errors.New("commit failed")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := tspath.NormalizePath(t.TempDir())
			ctx, cancel := context.WithCancel(context.Background())
			workspace := &pipelineAutofixWorkspace{
				t:        t,
				root:     root,
				fileName: tspath.ResolvePath(root, "source.ts"),
				current:  "a",
				next:     map[string]string{"a": "b"},
				apply: func(changes []FileChange) (ApplyResult, error) {
					cancel()
					return ApplyResult{
						ConfirmedPaths:  []string{changes[0].Path},
						RestoredInitial: test.restored,
					}, test.applyErr
				},
			}
			result, err := RunPipeline(ctx, NewAutofixRequest(
				workspace,
				ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
				AutofixPolicy{MaxRounds: 1},
				nil,
			))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("pipeline error = %v, want context.Canceled", err)
			}
			if test.applyErr != nil && !strings.Contains(err.Error(), test.applyErr.Error()) {
				t.Fatalf("pipeline error = %v, want joined apply error", err)
			}
			applied, ok := result.AppliedFixes()
			if !ok || applied.Verified != test.wantVerified || len(applied.Rounds) != 1 ||
				applied.Rounds[0].RestoredInitial != test.wantRestoredRound {
				t.Fatalf("applied result = %+v", applied)
			}
			if test.restored && applied.Last.Index != applied.Initial.Index {
				t.Fatalf("restored cancellation did not reuse initial observation: %+v", applied)
			}
		})
	}
}

func TestAutofixRestoredInitialReusesInitialObservation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "b", "b": "a"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 3},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || !applied.Verified || len(applied.Rounds) != 2 || !applied.Rounds[1].RestoredInitial ||
		applied.Last.Index != applied.Initial.Index || result.Observation.Index != 0 || workspace.current != "a" {
		t.Fatalf("restored applied result/workspace = %+v / %+v", applied, workspace)
	}
	if workspace.acquisitions != 2 {
		t.Fatalf("restored cycle acquisitions = %d, want 2", workspace.acquisitions)
	}
}

func TestAutofixSameTextFixConsumesRound(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "a"},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || len(applied.Rounds) != 1 || workspace.applies != 1 ||
		len(applied.Rounds[0].AttemptedPaths) != 1 || applied.Rounds[0].AppliedDiagnostics != 1 {
		t.Fatalf("same-text applied result/workspace = %+v / %+v", applied, workspace)
	}
}

func TestAutofixRejectsRoundLimitAboveProductBound(t *testing.T) {
	workspace := &pipelineAutofixWorkspace{}
	_, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: MaxFixRounds + 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "product safety bound") {
		t.Fatalf("round bound error = %v", err)
	}
	if workspace.acquisitions != 0 {
		t.Fatalf("invalid request acquired %d generations", workspace.acquisitions)
	}
}

func TestAutofixPipelineRejectsFalseApplyConfirmation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "b"},
		apply: func([]FileChange) (ApplyResult, error) {
			return ApplyResult{}, nil
		},
	}
	result, err := RunPipeline(context.Background(), NewAutofixRequest(
		workspace,
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		AutofixPolicy{MaxRounds: 1},
		nil,
	))
	if err == nil || !strings.Contains(err.Error(), "confirmed 0 of 1") {
		t.Fatalf("apply contract error = %v", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || len(applied.Rounds) != 1 {
		t.Fatalf("partial contract result = %+v", applied)
	}
}

func TestAutofixReobserveFailurePreservesLastSuccessfulObservation(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	nativeFinished := make(chan struct{})
	workspace := &pipelineAutofixWorkspace{
		t:        t,
		root:     root,
		fileName: tspath.ResolvePath(root, "source.ts"),
		current:  "a",
		next:     map[string]string{"a": "b"},
		rules: func(content string) []rule.ConfiguredRule {
			rules := []rule.ConfiguredRule{{Name: "plugin/check", IsEslintPluginRule: true}}
			if content == "b" {
				rules = append(rules, rule.ConfiguredRule{
					Name: "native/second-only",
					Run: func(rule.RuleContext) rule.RuleListeners {
						close(nativeFinished)
						return nil
					},
				})
			}
			return rules
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var dispatches atomic.Int32
	result, err := RunPipeline(ctx, NewAutofixRequest(
		workspace,
		ObservationPolicy{
			Demand: ArtifactDemand{
				Native: rule.EditDemandAutofix,
				Plugin: rule.EditDemandAutofix,
			},
			Plugin:        PluginConcurrentJoined,
			PluginFailure: PluginDiscardOnFailure,
		},
		AutofixPolicy{MaxRounds: 1, VerifyAfterLastRound: true},
		func(dispatchCtx context.Context, request EslintPluginLintRequest) (*EslintPluginLintResult, error) {
			if dispatches.Add(1) == 2 {
				<-nativeFinished
				cancel()
				return nil, dispatchCtx.Err()
			}
			return &EslintPluginLintResult{Results: []EslintPluginFileResult{{
				FilePath: request.Files[0].Path,
			}}}, nil
		},
	))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pipeline error = %v, want context.Canceled", err)
	}
	applied, ok := result.AppliedFixes()
	if !ok || applied.Verified || applied.Last.Index != 0 || result.Observation.Index != 0 {
		t.Fatalf("applied result = %+v, observation=%+v", applied, result.Observation)
	}
	records := result.PluginOutcomes()
	if len(records) != 2 || records[1].Observation != 1 || !errors.Is(records[1].DispatchError, context.Canceled) {
		t.Fatalf("plugin records = %+v, want failed observation 1 retained", records)
	}
	if _, leaked := result.ExecutedRules()["native/second-only"]; leaked {
		t.Fatal("failed re-observation polluted successful rule aggregation")
	}
}

func TestApplyResultNeverPublishesInvalidRestoredClaim(t *testing.T) {
	planned := []FileChange{{Path: "a.ts", Before: "a", After: "b"}}
	tests := []struct {
		name   string
		result ApplyResult
		err    error
	}{
		{name: "partial", result: ApplyResult{RestoredInitial: true}},
		{name: "apply error", result: ApplyResult{ConfirmedPaths: []string{"a.ts"}, RestoredInitial: true}, err: errors.New("commit failed")},
		{name: "extra confirmation", result: ApplyResult{ConfirmedPaths: []string{"a.ts", "extra.ts"}, RestoredInitial: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			round, err := validateApplyResult(planned, test.result, test.err)
			if err == nil {
				t.Fatal("invalid restored-initial claim was accepted")
			}
			if round.RestoredInitial {
				t.Fatal("invalid restored-initial claim escaped in the public round result")
			}
		})
	}
}

func TestPipelineReadsFixBackingOnlyForFixableTargets(t *testing.T) {
	root := tspath.NormalizePath(t.TempDir())
	fixablePath := tspath.ResolvePath(root, "fixable.ts")
	nonFixablePath := tspath.ResolvePath(root, "clean.ts")
	fixableProgram := pipelineTestProgram(t, root, fixablePath, "a")
	nonFixableProgram := pipelineTestProgram(t, root, nonFixablePath, "clean")
	generation := Generation{
		Native: NativeGeneration{
			Programs:       []*program.Program{fixableProgram, nonFixableProgram},
			Targets:        [][]string{{fixablePath}, {nonFixablePath}},
			SingleThreaded: true,
			Cwd:            root,
			RulesForFile: func(source *ast.SourceFile) []rule.ConfiguredRule {
				if source.FileName() != fixablePath {
					return nil
				}
				return []rule.ConfiguredRule{{
					Name: "native/fix",
					Run: func(ruleCtx rule.RuleContext) rule.RuleListeners {
						textRange := core.NewTextRange(0, 1)
						ruleCtx.ReportRangeWithFixes(
							textRange,
							rule.RuleMessage{Description: "fix"},
							rule.RuleFix{Range: textRange, Text: "b"},
						)
						return nil
					},
				}}
			},
		},
		Target: TargetProjection{
			ReadFixText: func(path string, source ast.SourceFileLike) (string, error) {
				if path != fixablePath {
					return "", fmt.Errorf("unexpected fix backing read for %q", path)
				}
				return source.Text(), nil
			},
		},
	}
	result, err := RunPipeline(context.Background(), NewPlanOnceRequest(
		pipelineTestProvider(generation, nil),
		ObservationPolicy{Demand: ArtifactDemand{Native: rule.EditDemandAutofix}},
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	changes, ok := result.Fix.PlannedChanges()
	if !ok || len(changes) != 1 || changes[0].Path != fixablePath {
		t.Fatalf("planned changes = %+v, planned=%v", changes, ok)
	}
}

func TestPlanFixesIsPureAndDeterministic(t *testing.T) {
	unsortedFixes := []rule.RuleFix{
		{Range: core.NewTextRange(1, 2), Text: "Z"},
		{Range: core.NewTextRange(0, 1), Text: "A"},
	}
	diagnostics := []rule.RuleDiagnostic{
		{
			FilePath: "z.ts",
			Range:    core.NewTextRange(0, 1),
			FixesPtr: &[]rule.RuleFix{{Range: core.NewTextRange(0, 1), Text: "Z"}},
		},
		{
			FilePath: "a.ts",
			Range:    core.NewTextRange(0, 2),
			FixesPtr: &unsortedFixes,
		},
	}
	changes, err := planFixes(diagnostics, fixBacking{"z.ts": "z", "a.ts": "ab"})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0].Path != "a.ts" || changes[0].After != "AZ" || changes[1].Path != "z.ts" {
		t.Fatalf("changes = %+v", changes)
	}
	if unsortedFixes[0].Range.Pos() != 1 || unsortedFixes[1].Range.Pos() != 0 {
		t.Fatalf("planner mutated caller-owned fixes: %+v", unsortedFixes)
	}
}
