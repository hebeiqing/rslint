package linter

import (
	"context"
	"errors"
	"sync"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/web-infra-dev/rslint/internal/program"
	"github.com/web-infra-dev/rslint/internal/rule"
)

// MaxFixRounds is the product-wide safety bound. A round is one non-empty
// change-set commit attempt; a final verification observation is not a round.
const MaxFixRounds = 10

// ReleaseFunc releases resources owned by one acquired generation. The
// pipeline wraps it with exact-once semantics immediately after acquisition.
type ReleaseFunc func()

// GenerationProvider acquires the next immutable source generation in one
// coherent lineage. The pipeline deliberately does not expose observation or
// fix-round numbers: deciding when to acquire is orchestration, while the
// provider only describes how the current source state becomes lint input.
//
// AcquireGeneration and the returned ReleaseFunc are called on RunPipeline's
// goroutine. On an acquisition error, the provider retains responsibility for
// cleaning up resources that it did not successfully publish.
type GenerationProvider interface {
	AcquireGeneration(ctx context.Context) (Generation, ReleaseFunc, error)
}

// GenerationProviderFunc adapts a function to GenerationProvider.
type GenerationProviderFunc func(context.Context) (Generation, ReleaseFunc, error)

func (f GenerationProviderFunc) AcquireGeneration(ctx context.Context) (Generation, ReleaseFunc, error) {
	return f(ctx)
}

// ChangeApplier commits one pure, non-empty whole-file change set. It reports
// only paths whose complete After text is known to have committed. A medium
// that may have partially mutated another path must return an error without
// confirming that path.
type ChangeApplier interface {
	ApplyChanges(ctx context.Context, changes []FileChange) (ApplyResult, error)
}

// AutofixWorkspace binds observation and mutation to the same source lineage.
// NewAutofixRequest accepts one such object and RunPipeline never permits its
// provider or applier halves to be replaced independently.
type AutofixWorkspace interface {
	GenerationProvider
	ChangeApplier
}

// Generation is one immutable, internally coherent lint snapshot. Config
// discovery, Program construction, storage, and protocol state stay in the
// integration that implements GenerationProvider.
type Generation struct {
	Native NativeGeneration
	Target TargetProjection
	Plugin *PluginGeneration
}

// NativeGeneration is the input understood by the native lint engine.
type NativeGeneration struct {
	Programs       []*program.Program
	Targets        [][]string
	RulesForFile   RuleHandler
	Cwd            string
	TypeCheck      bool
	SingleThreaded bool
	Timing         *TimingCollector
}

// TargetProjection binds Program-facing paths to the stable target identity
// shared by diagnostics and FileChange. ReadFixText freezes the exact backing
// representation that the integration will compare and commit, including a
// byte order mark when that medium carries one.
type TargetProjection struct {
	Path        func(sourcePath string) string
	ReadFixText func(targetPath string, source ast.SourceFileLike) (string, error)
}

// PluginGeneration is the generation-local projection needed to materialize
// third-party plugin work. WirePath is independent from TargetProjection.Path:
// nil preserves the Program source path used by disk-backed plugin hosts,
// while an overlay-backed host may project it to its public document identity.
// A nil InlineText means the plugin host reads the wire path itself. Deferred
// and after-native execution require InlineText so work can be detached before
// the generation is released.
type PluginGeneration struct {
	ConfigForFile EslintPluginFileConfigResolver
	WirePath      func(sourcePath string) string
	InlineText    func(wirePath string, source ast.SourceFileLike) (string, error)
}

// ArtifactDemand states which optional edits native and plugin producers must
// materialize. Planning changes is a separate request kind, so a final
// verification may still collect edits without scheduling another commit.
type ArtifactDemand struct {
	Native rule.EditDemand
	Plugin rule.EditDemand
}

func (d ArtifactDemand) valid() bool {
	return d.Native.IsValid() && d.Plugin.IsValid()
}

func (d ArtifactDemand) plansAutofixes() bool {
	return d.Native&rule.EditDemandAutofix != 0 || d.Plugin&rule.EditDemandAutofix != 0
}

// PluginExecution defines the relative scheduling of one observation's native
// and third-party work.
type PluginExecution uint8

const (
	// PluginConcurrentJoined starts plugin work before native lint and joins it
	// before releasing the generation. CLI and API use this mode.
	PluginConcurrentJoined PluginExecution = iota
	// PluginAfterNativeJoined detaches plugin work, runs native lint, releases
	// the generation, and then joins plugin work. LSP fix-all uses this mode.
	PluginAfterNativeJoined
	// pluginProgressiveAfterNative is selected only by
	// NewProgressiveLintRequest. Integrations cannot opt an ordinary lint/fix
	// request into a partially executed pipeline.
	pluginProgressiveAfterNative
)

func (m PluginExecution) valid() bool {
	return m <= pluginProgressiveAfterNative
}

// PluginFailurePolicy controls diagnostic completeness after a transport or
// reconstruction failure. Logging and protocol presentation stay with the
// integration through the returned structured outcome.
type PluginFailurePolicy uint8

const (
	// PluginKeepPartialWithSynthetic keeps completed plugin diagnostics and adds
	// a visible error diagnostic for non-cancellation failures.
	PluginKeepPartialWithSynthetic PluginFailurePolicy = iota
	// PluginDiscardOnFailure drops the observation's plugin diagnostics and
	// leaves it native-only. LSP uses this degradation policy.
	PluginDiscardOnFailure
)

func (p PluginFailurePolicy) valid() bool {
	return p <= PluginDiscardOnFailure
}

// ObservationPolicy contains only producer and scheduling semantics shared by
// integrations; it does not name CLI, API, or LSP entrypoints.
type ObservationPolicy struct {
	Demand        ArtifactDemand
	Plugin        PluginExecution
	PluginFailure PluginFailurePolicy
}

// DeferredPluginRun is a single-use, generation-detached enrichment task. It
// retains frozen wire/config/text only; the executor injects its own transport
// under its own timeout and cancellation lifecycle.
type DeferredPluginRun func(
	ctx context.Context,
	dispatcher EslintPluginDispatcher,
) (EslintPluginDispatchOutcome, error)

// ProgressiveDiagnostics binds baseline presentation and asynchronous
// enrichment admission to one request-scoped identity. RunPipeline publishes
// the baseline synchronously only after releasing the source generation, then
// conditionally submits enrichment. Submit must register and take lifecycle
// ownership synchronously, return promptly without running the task inline, and
// invoke run exactly once, including cleanup when parentCtx is already canceled.
// Timeout, supersession, transport injection, admission, and presentation belong
// to the implementation.
type ProgressiveDiagnostics interface {
	PublishBaseline(ctx context.Context, diagnostics []rule.RuleDiagnostic)
	Submit(parentCtx context.Context, run DeferredPluginRun)
}

// AutofixPolicy configures bounded apply-and-observe behavior.
type AutofixPolicy struct {
	MaxRounds                int
	VerifyAfterLastRound     bool
	VerificationDemand       ArtifactDemand
	StopOnTargetSyntaxErrors bool
}

type pipelineRequestKind uint8

const (
	pipelineRequestLint pipelineRequestKind = iota
	pipelineRequestProgressiveLint
	pipelineRequestPlanOnce
	pipelineRequestAutofix
)

// PipelineRequest is sealed so callers cannot combine an observation provider
// with an unrelated mutation medium or construct deferred-fix states.
type PipelineRequest struct {
	kind        pipelineRequestKind
	source      GenerationProvider
	workspace   AutofixWorkspace
	policy      ObservationPolicy
	autofix     AutofixPolicy
	dispatcher  EslintPluginDispatcher
	progressive ProgressiveDiagnostics
}

// NewLintRequest constructs one complete non-mutating lint request.
func NewLintRequest(
	source GenerationProvider,
	policy ObservationPolicy,
	dispatcher EslintPluginDispatcher,
) PipelineRequest {
	return PipelineRequest{
		kind:       pipelineRequestLint,
		source:     source,
		policy:     policy,
		dispatcher: dispatcher,
	}
}

// NewProgressiveLintRequest constructs a non-mutating lint request whose
// complete baseline is synchronously presented before optional plugin
// enrichment is submitted. The constructor seals the release/order/syntax and
// failure semantics so the integration implements ports without coordinating
// pipeline stages.
func NewProgressiveLintRequest(
	source GenerationProvider,
	demand ArtifactDemand,
	progressive ProgressiveDiagnostics,
) PipelineRequest {
	return PipelineRequest{
		kind:   pipelineRequestProgressiveLint,
		source: source,
		policy: ObservationPolicy{
			Demand:        demand,
			Plugin:        pluginProgressiveAfterNative,
			PluginFailure: PluginDiscardOnFailure,
		},
		progressive: progressive,
	}
}

// NewPlanOnceRequest constructs a non-mutating request that plans one pure
// change set from the same pre-fix diagnostics and AST returned to the caller.
func NewPlanOnceRequest(
	source GenerationProvider,
	policy ObservationPolicy,
	dispatcher EslintPluginDispatcher,
) PipelineRequest {
	return PipelineRequest{
		kind:       pipelineRequestPlanOnce,
		source:     source,
		policy:     policy,
		dispatcher: dispatcher,
	}
}

// NewAutofixRequest constructs a bounded autofix request over one coherent
// workspace. RunPipeline privately uses the same dynamic object for every
// acquire and apply operation.
func NewAutofixRequest(
	workspace AutofixWorkspace,
	policy ObservationPolicy,
	autofix AutofixPolicy,
	dispatcher EslintPluginDispatcher,
) PipelineRequest {
	return PipelineRequest{
		kind:       pipelineRequestAutofix,
		source:     workspace,
		workspace:  workspace,
		policy:     policy,
		autofix:    autofix,
		dispatcher: dispatcher,
	}
}

func (r PipelineRequest) validate() error {
	if r.source == nil {
		return errors.New("linter pipeline: generation provider must not be nil")
	}
	if !r.policy.Demand.valid() {
		return errors.New("linter pipeline: artifact demand is invalid")
	}
	if !r.policy.Plugin.valid() {
		return errors.New("linter pipeline: plugin execution policy is invalid")
	}
	if !r.policy.PluginFailure.valid() {
		return errors.New("linter pipeline: plugin failure policy is invalid")
	}
	if r.policy.Plugin == pluginProgressiveAfterNative && r.kind != pipelineRequestProgressiveLint {
		return errors.New("linter pipeline: progressive execution requires a progressive request")
	}
	switch r.kind {
	case pipelineRequestLint:
		if r.workspace != nil {
			return errors.New("linter pipeline: lint request must not carry a change applier")
		}
		if r.progressive != nil {
			return errors.New("linter pipeline: lint request must not carry progressive presentation ports")
		}
	case pipelineRequestProgressiveLint:
		if r.workspace != nil || r.dispatcher != nil {
			return errors.New("linter pipeline: progressive request must inject transport through its executor")
		}
		if r.progressive == nil {
			return errors.New("linter pipeline: progressive diagnostics must not be nil")
		}
		if r.policy.Plugin != pluginProgressiveAfterNative {
			return errors.New("linter pipeline: progressive request execution policy is invalid")
		}
	case pipelineRequestPlanOnce:
		if r.workspace != nil {
			return errors.New("linter pipeline: plan-once request must not carry a change applier")
		}
		if r.policy.Plugin == pluginProgressiveAfterNative {
			return errors.New("linter pipeline: plan-once requires joined plugin work")
		}
		if !r.policy.Demand.plansAutofixes() {
			return errors.New("linter pipeline: plan-once must request autofix artifacts")
		}
	case pipelineRequestAutofix:
		if r.workspace == nil {
			return errors.New("linter pipeline: autofix workspace must not be nil")
		}
		if r.policy.Plugin == pluginProgressiveAfterNative {
			return errors.New("linter pipeline: autofix requires joined plugin work")
		}
		if !r.policy.Demand.plansAutofixes() {
			return errors.New("linter pipeline: autofix observations must request autofix artifacts")
		}
		if r.autofix.MaxRounds <= 0 {
			return errors.New("linter pipeline: autofix MaxRounds must be positive")
		}
		if r.autofix.MaxRounds > MaxFixRounds {
			return errors.New("linter pipeline: autofix MaxRounds exceeds the product safety bound")
		}
		if !r.autofix.VerificationDemand.valid() {
			return errors.New("linter pipeline: verification artifact demand is invalid")
		}
	default:
		return errors.New("linter pipeline: request kind is invalid")
	}
	return nil
}

// LintedFile is one complete selected target projection. It includes
// syntax-error and zero-rule files so API callers do not have to rediscover
// selection through diagnostic side effects.
type LintedFile struct {
	Path       string
	SourceFile *ast.SourceFile
}

// NativeObservation is the complete result of native lint for one generation.
type NativeObservation struct {
	Diagnostics           []rule.RuleDiagnostic
	Lint                  *LintResult
	Files                 []LintedFile
	HasTargetSyntaxErrors bool
}

type pluginObservationKind uint8

const (
	pluginObservationNone pluginObservationKind = iota
	pluginObservationJoined
	pluginObservationProgressive
)

// ObservationResult distinguishes complete joined observations from a
// progressively presented baseline. Asynchronous enrichment never mutates the
// returned value.
type ObservationResult struct {
	Index  int
	Native NativeObservation

	pluginKind    pluginObservationKind
	pluginOutcome EslintPluginDispatchOutcome
}

// CompleteDiagnostics returns a fresh combined slice when plugin production is
// complete. The boolean is false for a progressively presented baseline.
func (r ObservationResult) CompleteDiagnostics() ([]rule.RuleDiagnostic, bool) {
	if r.pluginKind == pluginObservationProgressive {
		return nil, false
	}
	diagnostics := append([]rule.RuleDiagnostic(nil), r.Native.Diagnostics...)
	if r.pluginKind == pluginObservationJoined {
		diagnostics = append(diagnostics, r.pluginOutcome.Diagnostics...)
	}
	return diagnostics, true
}

// JoinedPluginOutcome returns the structured result for joined plugin work.
func (r ObservationResult) JoinedPluginOutcome() (EslintPluginDispatchOutcome, bool) {
	if r.pluginKind != pluginObservationJoined {
		return EslintPluginDispatchOutcome{}, false
	}
	return clonePluginOutcome(r.pluginOutcome), true
}

type fixResultKind uint8

const (
	fixResultNone fixResultKind = iota
	fixResultPlanned
	fixResultApplied
)

// FixRoundResult records one non-empty commit attempt. AppliedDiagnostics is
// derived by the pipeline from confirmed paths and the immutable planned set.
type FixRoundResult struct {
	AttemptedPaths     []string
	ConfirmedPaths     []string
	AppliedDiagnostics int
	RestoredInitial    bool
}

// AppliedFixResult distinguishes the initially observed generation from the
// last observed one and states whether that last observation still describes
// the workspace after all confirmed commits.
type AppliedFixResult struct {
	Initial  ObservationResult
	Last     ObservationResult
	Rounds   []FixRoundResult
	Verified bool
}

// FixResult is a tagged result: exactly one of None, PlannedChanges, or Applied
// is meaningful for the request kind that produced it.
type FixResult struct {
	kind     fixResultKind
	planned  []FileChange
	initial  ObservationResult
	rounds   []FixRoundResult
	verified bool
}

// PlannedChanges returns the PlanOnce change set. Diagnostics and AST remain
// those of the same pre-fix PipelineResult.Observation.
func (r FixResult) PlannedChanges() ([]FileChange, bool) {
	if r.kind != fixResultPlanned {
		return nil, false
	}
	return cloneFileChanges(r.planned), true
}

func (r FixResult) applied(last ObservationResult) (AppliedFixResult, bool) {
	if r.kind != fixResultApplied {
		return AppliedFixResult{}, false
	}
	return AppliedFixResult{
		Initial:  r.initial,
		Last:     last,
		Rounds:   cloneFixRounds(r.rounds),
		Verified: r.verified,
	}, true
}

// PluginDispatchRecord carries recoverable protocol notices and a joined
// plugin transport failure for integration presentation. Historical
// diagnostics stay on observations instead of retaining every superseded
// generation through autofix rounds.
type PluginDispatchRecord struct {
	Observation   int
	Notices       []EslintPluginProtocolNotice
	DispatchError error
}

// PipelineResult exposes the last observed generation separately from the
// tagged fix state. With autofix and no final verification, Observation may be
// older than the workspace; AppliedFixes().Verified is then false.
type PipelineResult struct {
	Observation ObservationResult
	Fix         FixResult

	executedRules  map[string]struct{}
	pluginOutcomes []PluginDispatchRecord
}

// AppliedFixes returns the bounded autofix history with PipelineResult's own
// last observation. Keeping this accessor on the aggregate prevents callers
// from pairing fix history with an unrelated observation.
func (r PipelineResult) AppliedFixes() (AppliedFixResult, bool) {
	return r.Fix.applied(r.Observation)
}

// ExecutedRules returns the union of native rules executed across observations.
func (r PipelineResult) ExecutedRules() map[string]struct{} {
	result := make(map[string]struct{}, len(r.executedRules))
	for name := range r.executedRules {
		result[name] = struct{}{}
	}
	return result
}

// PluginOutcomes returns joined plugin transport outcomes in observation order.
func (r PipelineResult) PluginOutcomes() []PluginDispatchRecord {
	result := make([]PluginDispatchRecord, len(r.pluginOutcomes))
	for index, record := range r.pluginOutcomes {
		result[index] = record
		result[index].Notices = append([]EslintPluginProtocolNotice(nil), record.Notices...)
	}
	return result
}

type releaseLease struct {
	release ReleaseFunc
	once    sync.Once
}

func (l *releaseLease) close() {
	if l != nil {
		l.once.Do(func() {
			if l.release != nil {
				l.release()
			}
		})
	}
}

func clonePluginOutcome(outcome EslintPluginDispatchOutcome) EslintPluginDispatchOutcome {
	outcome.Diagnostics = append([]rule.RuleDiagnostic(nil), outcome.Diagnostics...)
	outcome.Notices = append([]EslintPluginProtocolNotice(nil), outcome.Notices...)
	return outcome
}

func cloneFileChanges(changes []FileChange) []FileChange {
	return append([]FileChange(nil), changes...)
}

func cloneFixRounds(rounds []FixRoundResult) []FixRoundResult {
	result := make([]FixRoundResult, len(rounds))
	for index, round := range rounds {
		result[index] = round
		result[index].AttemptedPaths = append([]string(nil), round.AttemptedPaths...)
		result[index].ConfirmedPaths = append([]string(nil), round.ConfirmedPaths...)
	}
	return result
}
