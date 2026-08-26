package linter

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const linterImportPath = "github.com/web-infra-dev/rslint/internal/linter"

func TestIntegrationExecutionBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate architecture test")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))

	allowed := map[string]int{
		"cmd/rslint:handleLintCommand":        0,
		"internal/api/server:handleLint":      0,
		"internal/lsp:pushDiagnostics":        0,
		"internal/lsp:handleFixAllCodeAction": 0,
	}
	directRequestConstructor := map[string]string{
		"internal/lsp:pushDiagnostics":        "NewProgressiveLintRequest",
		"internal/lsp:handleFixAllCodeAction": "NewAutofixRequest",
	}
	progressiveCalls := 0
	scanRoots := []string{
		"cmd",
		"internal",
	}
	var violations []string
	for _, scanRoot := range scanRoots {
		absoluteDirectory := filepath.Join(repositoryRoot, filepath.FromSlash(scanRoot))
		err := filepath.WalkDir(absoluteDirectory, func(filePath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				return nil
			}
			relativePath, err := filepath.Rel(repositoryRoot, filePath)
			if err != nil {
				return err
			}
			relativePath = filepath.ToSlash(relativePath)
			packagePath := filepath.ToSlash(filepath.Dir(relativePath))
			if isProductIntegrationPackage(packagePath) &&
				(entry.Name() == "lint_pipeline.go" || entry.Name() == "lint_execution.go") {
				violations = append(violations, relativePath+": integration owns a lint orchestration facade")
			}
			fileSet := token.NewFileSet()
			file, err := parser.ParseFile(fileSet, filePath, nil, 0)
			if err != nil {
				return err
			}
			aliases := make(map[string]struct{})
			for _, imported := range file.Imports {
				path, err := strconv.Unquote(imported.Path.Value)
				if err != nil {
					return err
				}
				if path == "github.com/web-infra-dev/rslint/internal/lintpipeline" {
					violations = append(violations, relativePath+": imports retired internal/lintpipeline")
				}
				if packagePath == "internal/linter" && prohibitedLinterDependency(path) {
					violations = append(violations, relativePath+": linter imports integration/persistence dependency "+path)
				}
				if path != linterImportPath {
					continue
				}
				alias := "linter"
				if imported.Name != nil {
					alias = imported.Name.Name
				}
				if alias == "." {
					violations = append(violations, relativePath+": dot-imports internal/linter")
					continue
				}
				if alias != "_" {
					aliases[alias] = struct{}{}
				}
			}

			calls := make(map[*ast.SelectorExpr]*ast.CallExpr)
			ast.Inspect(file, func(node ast.Node) bool {
				call, ok := node.(*ast.CallExpr)
				if !ok {
					return true
				}
				if selector, ok := call.Fun.(*ast.SelectorExpr); ok {
					calls[selector] = call
				}
				return true
			})

			ast.Inspect(file, func(node ast.Node) bool {
				selector, ok := node.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				identifier, ok := selector.X.(*ast.Ident)
				if !ok {
					return true
				}
				if _, ok := aliases[identifier.Name]; !ok {
					return true
				}
				symbol := selector.Sel.Name
				position := fileSet.Position(selector.Pos())
				location := filepath.ToSlash(strings.TrimPrefix(position.Filename, repositoryRoot+string(filepath.Separator))) + ":" + strconv.Itoa(position.Line)
				topLevelFunction := enclosingTopLevelFunction(file, selector.Pos())
				key := packagePath + ":" + topLevelFunction
				if symbol == "NewProgressiveLintRequest" {
					if _, called := calls[selector]; !called || key != "internal/lsp:pushDiagnostics" ||
						insideFunctionLiteral(file, selector.Pos()) || !directFunctionBodyCall(file, selector.Pos()) {
						violations = append(violations, location+": progressive request must be constructed directly by LSP pushDiagnostics")
					} else {
						progressiveCalls++
					}
				}
				if key == "internal/lsp:pushDiagnostics" &&
					(symbol == "NewLintRequest" || symbol == "NewPlanOnceRequest" || symbol == "NewAutofixRequest") {
					violations = append(violations, location+": LSP pushDiagnostics bypasses the progressive presentation port")
				}
				if prohibitedIntegrationLinterSymbol(symbol) && packagePath != "internal/rule_tester" {
					violations = append(violations, location+": integration references raw linter stage "+symbol)
					return true
				}
				if symbol != "RunPipeline" {
					return true
				}
				call, called := calls[selector]
				if !called {
					violations = append(violations, location+": RunPipeline escapes as a value")
					return true
				}
				if _, permitted := allowed[key]; !permitted {
					violations = append(violations, location+": RunPipeline is outside an admitted product operation")
					return true
				}
				if insideFunctionLiteral(file, selector.Pos()) {
					violations = append(violations, location+": RunPipeline is hidden inside a function literal")
					return true
				}
				if insideAsyncOrDeferredStatement(file, selector.Pos()) {
					violations = append(violations, location+": RunPipeline is launched with go or defer")
					return true
				}
				if !directFunctionBodyCall(file, selector.Pos()) {
					violations = append(violations, location+": RunPipeline is nested below the product operation body")
					return true
				}
				if constructor, enforce := directRequestConstructor[key]; enforce &&
					(len(call.Args) != 2 || !isDirectLinterCall(call.Args[1], aliases, constructor)) {
					violations = append(violations, location+": RunPipeline must receive a direct linter."+constructor+" call")
					return true
				}
				allowed[key]++
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("scan %s: %v", scanRoot, err)
		}
	}

	for operation, count := range allowed {
		if count != 1 {
			violations = append(violations, operation+": RunPipeline call count is "+strconv.Itoa(count)+", want 1")
		}
	}
	if progressiveCalls != 1 {
		violations = append(violations, "internal/lsp:pushDiagnostics progressive request call count is "+strconv.Itoa(progressiveCalls)+", want 1")
	}
	violations = append(violations, selfPipelineCalls(repositoryRoot)...)
	if len(violations) > 0 {
		sort.Strings(violations)
		t.Fatalf("lint execution boundary violations:\n%s", strings.Join(violations, "\n"))
	}
}

func prohibitedLinterDependency(importPath string) bool {
	return importPath == "os" ||
		importPath == "io/fs" ||
		importPath == "log" ||
		importPath == "github.com/web-infra-dev/rslint/internal/output" ||
		importPath == "github.com/web-infra-dev/rslint/internal/program/loader" ||
		strings.HasPrefix(importPath, "github.com/web-infra-dev/rslint/internal/api") ||
		strings.HasPrefix(importPath, "github.com/web-infra-dev/rslint/internal/config") ||
		strings.HasPrefix(importPath, "github.com/web-infra-dev/rslint/internal/lsp") ||
		strings.HasPrefix(importPath, "github.com/microsoft/typescript-go/shim/vfs")
}

func isProductIntegrationPackage(packagePath string) bool {
	return packagePath == "cmd/rslint" ||
		packagePath == "internal/api/server" ||
		packagePath == "internal/lsp"
}

func prohibitedIntegrationLinterSymbol(symbol string) bool {
	return strings.HasPrefix(symbol, "RunLinter") ||
		strings.HasPrefix(symbol, "PrepareLintPlan") ||
		symbol == "ApplyRuleFixes" ||
		strings.HasPrefix(symbol, "BuildEslintPluginFileInput") ||
		strings.HasPrefix(symbol, "DispatchEslintPluginRules") ||
		symbol == "LintSingleFile" ||
		symbol == "CollectFileSyntacticDiagnostics"
}

func enclosingTopLevelFunction(file *ast.File, position token.Pos) string {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if ok && function.Pos() <= position && position <= function.End() {
			return function.Name.Name
		}
	}
	return ""
}

func isDirectLinterCall(expression ast.Expr, aliases map[string]struct{}, symbol string) bool {
	for {
		parenthesized, ok := expression.(*ast.ParenExpr)
		if !ok {
			break
		}
		expression = parenthesized.X
	}
	call, ok := expression.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || selector.Sel.Name != symbol {
		return false
	}
	identifier, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	_, imported := aliases[identifier.Name]
	return imported
}

func insideFunctionLiteral(file *ast.File, position token.Pos) bool {
	inside := false
	ast.Inspect(file, func(node ast.Node) bool {
		if inside || node == nil {
			return false
		}
		literal, ok := node.(*ast.FuncLit)
		if ok && literal.Pos() <= position && position <= literal.End() {
			inside = true
			return false
		}
		return true
	})
	return inside
}

func insideAsyncOrDeferredStatement(file *ast.File, position token.Pos) bool {
	inside := false
	ast.Inspect(file, func(node ast.Node) bool {
		if inside || node == nil {
			return false
		}
		switch statement := node.(type) {
		case *ast.GoStmt:
			inside = statement.Pos() <= position && position <= statement.End()
		case *ast.DeferStmt:
			inside = statement.Pos() <= position && position <= statement.End()
		}
		return !inside
	})
	return inside
}

func directFunctionBodyCall(file *ast.File, position token.Pos) bool {
	for _, declaration := range file.Decls {
		function, ok := declaration.(*ast.FuncDecl)
		if !ok || function.Body == nil || position < function.Body.Pos() || position > function.Body.End() {
			continue
		}
		for _, statement := range function.Body.List {
			if position < statement.Pos() || position > statement.End() {
				continue
			}
			switch statement.(type) {
			case *ast.AssignStmt, *ast.ExprStmt, *ast.ReturnStmt:
				return true
			default:
				return false
			}
		}
	}
	return false
}

func selfPipelineCalls(repositoryRoot string) []string {
	directory := filepath.Join(repositoryRoot, "internal", "linter")
	entries, err := os.ReadDir(directory)
	if err != nil {
		return []string{"internal/linter: " + err.Error()}
	}
	var violations []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		fileSet := token.NewFileSet()
		filePath := filepath.Join(directory, entry.Name())
		file, parseErr := parser.ParseFile(fileSet, filePath, nil, 0)
		if parseErr != nil {
			violations = append(violations, "parse "+filePath+": "+parseErr.Error())
			continue
		}
		ast.Inspect(file, func(node ast.Node) bool {
			identifier, ok := node.(*ast.Ident)
			if !ok || identifier.Name != "RunPipeline" {
				return true
			}
			if identifier.Obj != nil {
				declaration, declaredHere := identifier.Obj.Decl.(*ast.FuncDecl)
				if declaredHere && declaration.Name == identifier && declaration.Recv == nil {
					return true
				}
			}
			position := fileSet.Position(identifier.Pos())
			violations = append(violations, "internal/linter/"+entry.Name()+":"+strconv.Itoa(position.Line)+": production code references RunPipeline outside its declaration")
			return true
		})
	}
	return violations
}
