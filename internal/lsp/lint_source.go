package lsp

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/microsoft/typescript-go/shim/ast"
	"github.com/microsoft/typescript-go/shim/compiler"
	"github.com/microsoft/typescript-go/shim/lsp/lsproto"
	"github.com/microsoft/typescript-go/shim/vfs"

	"github.com/web-infra-dev/rslint/internal/config"
	"github.com/web-infra-dev/rslint/internal/config/target"
	"github.com/web-infra-dev/rslint/internal/linter"
	lintprogram "github.com/web-infra-dev/rslint/internal/program"
	"github.com/web-infra-dev/rslint/internal/rule"
)

// rulesSkippedInEditors names rules whose evidence exists only in encoded file
// bytes. Editor documents contain decoded text, so those rules remain CLI-only.
var rulesSkippedInEditors = map[string]bool{
	"unicode-bom": true,
}

func rulesServedToEditors(configuredRules []rule.ConfiguredRule) []rule.ConfiguredRule {
	skipped := func(configured rule.ConfiguredRule) bool {
		return rulesSkippedInEditors[configured.Name]
	}
	if !slices.ContainsFunc(configuredRules, skipped) {
		return configuredRules
	}
	served := make([]rule.ConfiguredRule, 0, len(configuredRules))
	for _, configured := range configuredRules {
		if !skipped(configured) {
			served = append(served, configured)
		}
	}
	return served
}

// documentLintSource adapts the current LSP Session and document snapshot to
// the linter's source port. It selects and leases Programs but never decides
// how or when lint work runs.
type documentLintSource struct {
	server          *Server
	uri             lsproto.DocumentUri
	snapshot        documentLintSnapshot
	requestPrograms documentLintProgramRequest
	buildGeneration documentLintGenerationBuilder
}

type documentLintProgramRequest func(
	ctx context.Context,
	uri lsproto.DocumentUri,
	target target.File,
) (lintProjectLoaders, linter.ReleaseFunc)

type documentLintGenerationBuilder func(
	program *compiler.Program,
	sourceFile *ast.SourceFile,
	target target.File,
	processCwd string,
	hasTypeInfo bool,
	snapshot documentLintSnapshot,
) linter.Generation

func buildDocumentLintGeneration(
	program *compiler.Program,
	sourceFile *ast.SourceFile,
	target target.File,
	processCwd string,
	hasTypeInfo bool,
	snapshot documentLintSnapshot,
) linter.Generation {
	return newLSPGeneration(
		program,
		sourceFile,
		target,
		processCwd,
		hasTypeInfo,
		snapshot.resolvedConfig.EnabledRules,
		pluginFileConfigForLintSnapshot(snapshot),
		nil,
		nil,
	)
}

func (s *documentLintSource) AcquireGeneration(ctx context.Context) (linter.Generation, linter.ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return linter.Generation{}, nil, err
	}
	if s == nil || s.server == nil {
		return linter.Generation{}, nil, errors.New("LSP document lint source is not configured")
	}
	server := s.server
	snapshot := s.snapshot
	if !snapshot.configResolved {
		snapshot = resolveDocumentLintSnapshotConfig(snapshot, server.fs)
	}
	if isDefaultExcludedLintPath(snapshot.target.Path, server.cwd, server.fs) ||
		snapshot.resolvedConfig.GloballyIgnored {
		return emptyLSPGeneration(server.cwd), nil, nil
	}

	request := newStandaloneLintProjectRequest(
		snapshot.target,
		func() vfs.FS { return server.currentEditorOverlayFSForTarget(s.uri, snapshot.target) },
	)
	loaders := request.loaders()
	release := linter.ReleaseFunc(nil)
	releasePending := false
	defer func() {
		if releasePending && release != nil {
			release()
		}
	}()
	if s.requestPrograms != nil {
		loaders, release = s.requestPrograms(ctx, s.uri, snapshot.target)
		releasePending = release != nil
	} else if server.lintPrograms != nil && server.lintPrograms.Usable() {
		loadProgram, loadMetadata, finalize := server.lintPrograms.Request(
			ctx,
			s.uri,
			snapshot.target,
		)
		loaders = lintProjectLoaders{
			program:  loadProgram,
			metadata: loadMetadata,
		}
		release = finalize
		releasePending = release != nil
	}

	program, sourceFile, hasTypeInfo, err := selectLintProgram(
		s.uri,
		snapshot.target,
		server.session,
		ctx,
		snapshot.typeScriptConfigPaths,
		server.fs,
		loaders,
		server.lintSessionRoots,
	)
	if err != nil {
		return linter.Generation{}, nil, err
	}
	if sourceFile == nil {
		releasePending = false
		return emptyLSPGeneration(server.cwd), release, nil
	}
	buildGeneration := s.buildGeneration
	if buildGeneration == nil {
		buildGeneration = buildDocumentLintGeneration
	}
	generation := buildGeneration(
		program,
		sourceFile,
		snapshot.target,
		server.cwd,
		hasTypeInfo,
		snapshot,
	)
	// Ownership transfers only after the complete generation has been built.
	// A panic anywhere above this point leaves releasePending armed.
	releasePending = false
	return generation, release, nil
}

func newLSPGeneration(
	program *compiler.Program,
	sourceFile *ast.SourceFile,
	target target.File,
	processCwd string,
	hasTypeInfo bool,
	enabledRules []rule.ConfiguredRule,
	pluginConfig *linter.EslintPluginFileConfig,
	inlinePluginText func(targetPath string, source ast.SourceFileLike) (string, error),
	readFixText func(targetPath string, source ast.SourceFileLike) (string, error),
) linter.Generation {
	sourceProgram := lintprogram.NewFromCompiler(program)
	servedRules := rulesServedToEditors(enabledRules)
	if !hasTypeInfo {
		servedRules = rule.FilterNonTypeAwareRules(servedRules)
	}
	var plugin *linter.PluginGeneration
	if pluginConfig != nil {
		if inlinePluginText == nil {
			inlinePluginText = func(_ string, source ast.SourceFileLike) (string, error) {
				return source.Text(), nil
			}
		}
		plugin = &linter.PluginGeneration{
			ConfigForFile: func(string) linter.EslintPluginFileConfig {
				return *pluginConfig
			},
			WirePath: func(path string) string {
				if path == sourceFile.FileName() {
					return target.Path
				}
				return path
			},
			InlineText: inlinePluginText,
		}
	}
	return linter.Generation{
		Native: linter.NativeGeneration{
			Programs:       []*lintprogram.Program{sourceProgram},
			Targets:        [][]string{{sourceFile.FileName()}},
			SingleThreaded: true,
			Cwd:            processCwd,
			RulesForFile: func(*ast.SourceFile) []rule.ConfiguredRule {
				return servedRules
			},
		},
		Target: linter.TargetProjection{
			Path: func(path string) string {
				if path == sourceFile.FileName() {
					return target.Path
				}
				return path
			},
			ReadFixText: readFixText,
		},
		Plugin: plugin,
	}
}

func pluginFileConfigForLintSnapshot(snapshot documentLintSnapshot) *linter.EslintPluginFileConfig {
	if !snapshot.configResolved || snapshot.resolvedConfig.MergedConfig == nil {
		return nil
	}
	configKey := ""
	if snapshot.usesJavaScriptConfig {
		configKey = snapshot.target.ConfigDirectory
	}
	languageOptions, settings := config.PluginMergedMaps(snapshot.resolvedConfig.MergedConfig)
	return &linter.EslintPluginFileConfig{
		ConfigKey:       configKey,
		LanguageOptions: languageOptions,
		Settings:        settings,
	}
}

type speculativeLintEnvironment struct {
	baseFS     vfs.FS
	processCwd string
	openFiles  map[string]string
}

func (s *Server) freezeSpeculativeLintEnvironment(
	uri lsproto.DocumentUri,
	target target.File,
) speculativeLintEnvironment {
	openFiles, _ := s.currentEditorOverlayFilesForFrozenTarget(uri, target, "", false)
	return speculativeLintEnvironment{
		baseFS:     s.fs,
		processCwd: s.cwd,
		openFiles:  openFiles,
	}
}

func acquireSpeculativeLintGeneration(
	ctx context.Context,
	content string,
	snapshot documentLintSnapshot,
	environment speculativeLintEnvironment,
) (linter.Generation, linter.ReleaseFunc, error) {
	if err := ctx.Err(); err != nil {
		return linter.Generation{}, nil, err
	}
	if !snapshot.configResolved {
		snapshot = resolveDocumentLintSnapshotConfig(snapshot, environment.baseFS)
	}
	target := snapshot.target
	if isDefaultExcludedLintPath(target.Path, environment.processCwd, environment.baseFS) ||
		snapshot.resolvedConfig.GloballyIgnored {
		return emptyLSPGeneration(environment.processCwd), nil, nil
	}

	files := make(map[string]string, len(environment.openFiles)+2)
	for path, text := range environment.openFiles {
		files[path] = text
	}
	addEditorOverlayTarget(files, target, content)
	overlayFS := newFrozenLintTargetOverlayFS(environment.baseFS, files, target)

	request := newStandaloneLintProjectRequestWithFS(target, overlayFS)
	selected, found, err := selectConfiguredLintProject(
		snapshot.typeScriptConfigPaths,
		target,
		request.loaders(),
	)
	if err != nil {
		return linter.Generation{}, nil, err
	}
	newGeneration := func(program *compiler.Program, sourceFile *ast.SourceFile, hasTypeInfo bool) linter.Generation {
		inlineText := func(path string, _ ast.SourceFileLike) (string, error) {
			if path != target.Path {
				return "", fmt.Errorf("unexpected speculative lint target %q", path)
			}
			return content, nil
		}
		return newLSPGeneration(
			program,
			sourceFile,
			target,
			environment.processCwd,
			hasTypeInfo,
			snapshot.resolvedConfig.EnabledRules,
			pluginFileConfigForLintSnapshot(snapshot),
			inlineText,
			inlineText,
		)
	}
	if found {
		if selected.sourceFile == nil {
			return emptyLSPGeneration(environment.processCwd), nil, nil
		}
		return newGeneration(selected.program, selected.sourceFile, true), nil, nil
	}

	program, err := createStandaloneFallbackProgram(target.Path, target.ConfigDirectory, overlayFS)
	if err != nil {
		return linter.Generation{}, nil, fmt.Errorf("create fallback lint program: %w", err)
	}
	sourceFile := sourceFileForTarget(program, target, overlayFS)
	if sourceFile == nil {
		return emptyLSPGeneration(environment.processCwd), nil, nil
	}
	return newGeneration(program, sourceFile, false), nil, nil
}

func emptyLSPGeneration(processCwd string) linter.Generation {
	return linter.Generation{Native: linter.NativeGeneration{
		Cwd:            processCwd,
		SingleThreaded: true,
	}}
}
