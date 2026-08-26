package linter

import (
	"context"
	"errors"

	"github.com/web-infra-dev/rslint/internal/rule"
)

// RunPipeline is the single production lint orchestrator. It owns generation
// acquisition/release, preparation, native/plugin scheduling, path projection,
// fix planning, bounded apply rounds, re-observation, and rule aggregation.
// Integrations supply source and transport behavior only through the sealed
// request's ports.
func RunPipeline(ctx context.Context, request PipelineRequest) (PipelineResult, error) {
	result := PipelineResult{executedRules: make(map[string]struct{})}
	if ctx == nil {
		return result, errors.New("linter pipeline: context must not be nil")
	}
	if err := request.validate(); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	planChanges := request.kind == pipelineRequestPlanOnce || request.kind == pipelineRequestAutofix
	initial, err := executeObservation(
		ctx,
		request.source,
		request.policy,
		request.dispatcher,
		0,
		planChanges,
		request.kind == pipelineRequestProgressiveLint ||
			(request.kind == pipelineRequestAutofix && request.autofix.StopOnTargetSyntaxErrors),
	)
	recordPluginDispatch(&result, initial)
	if err != nil {
		return result, err
	}
	result.Observation = initial.observation
	mergeSuccessfulExecution(&result, initial)

	switch request.kind {
	case pipelineRequestLint:
		result.Fix = FixResult{kind: fixResultNone}
		return result, nil
	case pipelineRequestProgressiveLint:
		return presentProgressiveLint(ctx, request, result, initial)
	case pipelineRequestPlanOnce:
		diagnostics, complete := result.Observation.CompleteDiagnostics()
		if !complete {
			return result, errors.New("linter pipeline: plan-once observation is incomplete")
		}
		changes, planErr := planFixes(diagnostics, initial.fixBacking)
		result.Fix = FixResult{kind: fixResultPlanned, planned: changes}
		return result, joinContextError(planErr, ctx)
	case pipelineRequestAutofix:
		return runAutofixPipeline(ctx, request, result, initial)
	default:
		return result, errors.New("linter pipeline: request kind is invalid")
	}
}

func presentProgressiveLint(
	ctx context.Context,
	request PipelineRequest,
	result PipelineResult,
	execution observationExecution,
) (PipelineResult, error) {
	result.Fix = FixResult{kind: fixResultNone}
	var enrichment DeferredPluginRun
	if execution.deferredTask != nil {
		var err error
		enrichment, err = newDeferredPluginRun(*execution.deferredTask)
		if err != nil {
			return result, err
		}
	}
	baseline := append([]rule.RuleDiagnostic(nil), execution.observation.Native.Diagnostics...)
	request.progressive.PublishBaseline(ctx, baseline)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if enrichment != nil {
		request.progressive.Submit(ctx, enrichment)
	}
	return result, nil
}

func runAutofixPipeline(
	ctx context.Context,
	request PipelineRequest,
	result PipelineResult,
	initial observationExecution,
) (PipelineResult, error) {
	result.Fix = FixResult{
		kind:     fixResultApplied,
		initial:  initial.observation,
		verified: true,
	}
	current := initial
	if request.autofix.StopOnTargetSyntaxErrors && current.observation.Native.HasTargetSyntaxErrors {
		return result, nil
	}

	for roundIndex := range request.autofix.MaxRounds {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		diagnostics, complete := current.observation.CompleteDiagnostics()
		if !complete {
			return result, errors.New("linter pipeline: autofix observation is incomplete")
		}
		changes, err := planFixes(diagnostics, current.fixBacking)
		if err = joinContextError(err, ctx); err != nil {
			return result, err
		}
		if len(changes) == 0 {
			return result, nil
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}

		applyResult, applyErr := request.workspace.ApplyChanges(ctx, cloneFileChanges(changes))
		// Once the medium was asked to commit, unconfirmed state may differ from
		// the last observation even if no path can be declared successful.
		result.Fix.verified = false
		round, validatedErr := validateApplyResult(changes, applyResult, applyErr)
		result.Fix.rounds = append(result.Fix.rounds, round)
		if validatedErr != nil {
			return result, joinContextError(validatedErr, ctx)
		}
		if applyResult.RestoredInitial {
			result.Observation = initial.observation
			result.Fix.verified = true
			return result, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return result, err
		}

		lastAllowedRound := roundIndex+1 == request.autofix.MaxRounds
		if lastAllowedRound && !request.autofix.VerifyAfterLastRound {
			return result, nil
		}
		policy := request.policy
		planNextChanges := true
		if lastAllowedRound {
			policy.Demand = request.autofix.VerificationDemand
			planNextChanges = false
		}
		next, observeErr := executeObservation(
			ctx,
			request.source,
			policy,
			request.dispatcher,
			current.observation.Index+1,
			planNextChanges,
			request.autofix.StopOnTargetSyntaxErrors,
		)
		recordPluginDispatch(&result, next)
		if observeErr != nil {
			return result, observeErr
		}
		result.Observation = next.observation
		mergeSuccessfulExecution(&result, next)
		current = next
		result.Fix.verified = true
		if request.autofix.StopOnTargetSyntaxErrors && current.observation.Native.HasTargetSyntaxErrors {
			return result, nil
		}
		if lastAllowedRound {
			return result, nil
		}
	}
	return result, nil
}

func mergeSuccessfulExecution(result *PipelineResult, execution observationExecution) {
	if result == nil {
		return
	}
	if lintResult := execution.observation.Native.Lint; lintResult != nil {
		for name := range lintResult.ExecutedRules {
			result.executedRules[name] = struct{}{}
		}
	}
}

func recordPluginDispatch(result *PipelineResult, execution observationExecution) {
	if result != nil && execution.pluginOutcome != nil {
		result.pluginOutcomes = append(result.pluginOutcomes, PluginDispatchRecord{
			Observation:   execution.observation.Index,
			Notices:       append([]EslintPluginProtocolNotice(nil), execution.pluginOutcome.Notices...),
			DispatchError: execution.pluginOutcome.DispatchError,
		})
	}
}
