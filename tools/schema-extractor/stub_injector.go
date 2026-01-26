package main

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/printer"
	"go/token"
	"log"
	"os"
	"path/filepath"
	"strings"
)

var (
	dryRun     = flag.Bool("dry-run", false, "Print changes without writing files")
	serviceDir = flag.String("service-dir", "../../internal/service", "Path to service directory")
	verbose    = flag.Bool("v", false, "Verbose output")
	service    = flag.String("service", "", "Only process specific service (e.g., 's3', 'ec2')")
)

type stats struct {
	filesScanned    int
	filesModified   int
	functionsFound  int
	functionsModded int
	filesSkipped    int
	errors          []string
}

func other() {
	flag.Parse()

	log.SetFlags(0)
	log.Printf("AWS Provider Stub Check Injector\n")
	log.Printf("================================\n\n")

	if *dryRun {
		log.Printf("🔍 DRY RUN MODE - No files will be modified\n\n")
	}

	s := &stats{}

	err := filepath.Walk(*serviceDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}

		// Skip directories
		if info.IsDir() {
			return nil
		}

		// Skip non-Go files
		if filepath.Ext(path) != ".go" {
			return nil
		}

		// Skip generated and test files
		if strings.HasSuffix(path, "_gen.go") ||
			strings.HasSuffix(path, "_test.go") ||
			strings.Contains(path, "test-fixtures") {
			s.filesSkipped++
			return nil
		}

		// Filter by service if specified
		if *service != "" {
			serviceInPath := filepath.Base(filepath.Dir(path))
			if serviceInPath != *service {
				return nil
			}
		}

		s.filesScanned++
		if err := processFile(path, s); err != nil {
			errMsg := fmt.Sprintf("%s: %v", path, err)
			s.errors = append(s.errors, errMsg)
			log.Printf("❌ Error: %s\n", errMsg)
		}

		return nil
	})

	if err != nil {
		log.Fatalf("❌ Fatal error walking directory: %v\n", err)
	}

	// Print summary
	log.Printf("\n================================\n")
	log.Printf("Summary:\n")
	log.Printf("  Files scanned:     %d\n", s.filesScanned)
	log.Printf("  Files modified:    %d\n", s.filesModified)
	log.Printf("  Files skipped:     %d\n", s.filesSkipped)
	log.Printf("  Functions found:   %d\n", s.functionsFound)
	log.Printf("  Functions modded:  %d\n", s.functionsModded)

	if len(s.errors) > 0 {
		log.Printf("\n❌ Errors encountered: %d\n", len(s.errors))
		for _, e := range s.errors {
			log.Printf("  - %s\n", e)
		}
		os.Exit(1)
	}

	if *dryRun && s.functionsModded > 0 {
		log.Printf("\n💡 Run without -dry-run to apply changes\n")
	} else if s.functionsModded > 0 {
		log.Printf("\n✅ Successfully modified %d functions!\n", s.functionsModded)
	} else {
		log.Printf("\n✨ No functions needed modification\n")
	}
}

func processFile(path string, s *stats) error {
	fset := token.NewFileSet()

	// Parse the file
	node, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
	if err != nil {
		return fmt.Errorf("parsing: %w", err)
	}

	modified := false
	functionsModified := 0

	// Find service name from path
	serviceName := filepath.Base(filepath.Dir(path))

	// Process each function
	ast.Inspect(node, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		// Check if this is a Create function
		if !isCreateFunction(fn) {
			return true
		}

		s.functionsFound++

		// Check if stub check already exists
		if hasStubCheck(fn) {
			if *verbose {
				log.Printf("  ⏭️  %s already has stub check\n", fn.Name.Name)
			}
			return true
		}

		// Try to inject stub check
		if injected, err := injectStubCheck(fn, node, serviceName, fset); err != nil {
			log.Printf("  ⚠️  Could not inject into %s: %v\n", fn.Name.Name, err)
		} else if injected {
			modified = true
			functionsModified++
			s.functionsModded++
			log.Printf("  ✅ Modified %s in %s\n", fn.Name.Name, filepath.Base(path))
		}

		return true
	})

	if !modified {
		return nil
	}

	s.filesModified++

	if *dryRun {
		if *verbose {
			log.Printf("\n--- Dry run output for %s ---\n", path)
			printer.Fprint(os.Stdout, fset, node)
			log.Printf("\n--- End dry run output ---\n\n")
		}
		return nil
	}

	// Format and write the modified file
	var buf bytes.Buffer
	if err := format.Node(&buf, fset, node); err != nil {
		return fmt.Errorf("formatting: %w", err)
	}

	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("writing: %w", err)
	}

	return nil
}

func isCreateFunction(fn *ast.FuncDecl) bool {
	name := fn.Name.Name

	// Must contain "Create" (but not "createTags" or other helpers)
	if !strings.Contains(name, "Create") {
		return false
	}

	// Should start with "resource" typically
	if !strings.HasPrefix(name, "resource") {
		return false
	}

	// Must have 3 parameters: (ctx, d, meta)
	if fn.Type.Params == nil || len(fn.Type.Params.List) != 3 {
		return false
	}

	// Must return diag.Diagnostics
	if fn.Type.Results == nil || len(fn.Type.Results.List) != 1 {
		return false
	}

	return true
}

func hasStubCheck(fn *ast.FuncDecl) bool {
	if fn.Body == nil {
		return false
	}

	// Check if StubCreateOperation is already called
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		if callExpr, ok := n.(*ast.CallExpr); ok {
			if sel, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
				if sel.Sel.Name == "StubCreateOperation" {
					found = true
					return false
				}
			}
		}
		return true
	})

	return found
}

func injectStubCheck(fn *ast.FuncDecl, file *ast.File, serviceName string, fset *token.FileSet) (bool, error) {
	if fn.Body == nil || len(fn.Body.List) == 0 {
		return false, fmt.Errorf("empty function body")
	}

	// Find the API call that creates the resource
	apiCallInfo := findAPICall(fn)
	if apiCallInfo == nil {
		return false, fmt.Errorf("could not find API call")
	}

	if *verbose {
		log.Printf("    Found API call: %s (input: %s)\n", apiCallInfo.operation, apiCallInfo.inputVar)
	}

	// Find insertion point (right before the API call)
	insertIdx := -1
	for i, stmt := range fn.Body.List {
		if stmt == apiCallInfo.stmt {
			insertIdx = i
			break
		}
	}

	if insertIdx == -1 {
		return false, fmt.Errorf("could not find insertion point")
	}

	// Create the stub check statements
	stubStmts := createStubCheckStatements(serviceName, apiCallInfo)

	// Insert the stub check before the API call
	newBody := make([]ast.Stmt, 0, len(fn.Body.List)+len(stubStmts))
	newBody = append(newBody, fn.Body.List[:insertIdx]...)
	newBody = append(newBody, stubStmts...)
	newBody = append(newBody, fn.Body.List[insertIdx:]...)

	fn.Body.List = newBody

	// Ensure conns import exists
	ensureConnsImport(file)

	return true, nil
}

type apiCallInfo struct {
	operation string
	inputVar  string
	outputVar string
	stmt      ast.Stmt
}

func findAPICall(fn *ast.FuncDecl) *apiCallInfo {
	var info *apiCallInfo

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		// Look for assignment with API call: output, err := conn.SomeOperation(ctx, &input)
		if assignStmt, ok := n.(*ast.AssignStmt); ok {
			if len(assignStmt.Rhs) == 1 {
				if callExpr, ok := assignStmt.Rhs[0].(*ast.CallExpr); ok {
					if sel, ok := callExpr.Fun.(*ast.SelectorExpr); ok {
						// Check if it's calling conn.Something
						if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "conn" {
							operation := sel.Sel.Name

							// Extract input variable name
							inputVar := extractInputVar(callExpr)
							outputVar := ""

							// Extract output variable name
							if len(assignStmt.Lhs) >= 1 {
								if ident, ok := assignStmt.Lhs[0].(*ast.Ident); ok {
									outputVar = ident.Name
								}
							}

							info = &apiCallInfo{
								operation: operation,
								inputVar:  inputVar,
								outputVar: outputVar,
								stmt:      assignStmt,
							}

							return false
						}
					}
				}
			}
		}
		return true
	})

	return info
}

func extractInputVar(callExpr *ast.CallExpr) string {
	// Look for &input or &params in call arguments
	for _, arg := range callExpr.Args {
		if unary, ok := arg.(*ast.UnaryExpr); ok {
			if unary.Op == token.AND {
				if ident, ok := unary.X.(*ast.Ident); ok {
					return ident.Name
				}
			}
		}
	}
	return "input"
}

func createStubCheckStatements(serviceName string, info *apiCallInfo) []ast.Stmt {
	// Create variable declaration for output
	outputDecl := &ast.DeclStmt{
		Decl: &ast.GenDecl{
			Tok: token.VAR,
			Specs: []ast.Spec{
				&ast.ValueSpec{
					Names: []*ast.Ident{ast.NewIdent("output")},
					Type: &ast.StarExpr{
						X: &ast.SelectorExpr{
							X:   ast.NewIdent(serviceName),
							Sel: ast.NewIdent(info.operation + "Output"),
						},
					},
				},
			},
		},
	}

	// Create if statement with stub check
	ifStmt := &ast.IfStmt{
		Cond: &ast.CallExpr{
			Fun: &ast.SelectorExpr{
				X:   ast.NewIdent("conns"),
				Sel: ast.NewIdent("StubCreateOperation"),
			},
			Args: []ast.Expr{
				ast.NewIdent("ctx"),
				&ast.BasicLit{
					Kind:  token.STRING,
					Value: fmt.Sprintf(`"%s"`, serviceName),
				},
				&ast.BasicLit{
					Kind:  token.STRING,
					Value: fmt.Sprintf(`"%s"`, info.operation),
				},
				&ast.UnaryExpr{
					Op: token.AND,
					X:  ast.NewIdent(info.inputVar),
				},
				&ast.UnaryExpr{
					Op: token.AND,
					X:  ast.NewIdent("output"),
				},
			},
		},
		Body: &ast.BlockStmt{
			List: []ast.Stmt{
				// Comment: Stub mode - use stubbed output
				&ast.ExprStmt{
					X: &ast.BasicLit{
						Kind:  token.COMMENT,
						Value: "// Stub mode - use stubbed output",
					},
				},
				// d.SetId(...) - simplified, would need to determine the right field
				&ast.ExprStmt{
					X: &ast.CallExpr{
						Fun: &ast.SelectorExpr{
							X:   ast.NewIdent("d"),
							Sel: ast.NewIdent("SetId"),
						},
						Args: []ast.Expr{
							&ast.CallExpr{
								Fun: &ast.SelectorExpr{
									X:   ast.NewIdent("aws"),
									Sel: ast.NewIdent("ToString"),
								},
								Args: []ast.Expr{
									&ast.SelectorExpr{
										X:   ast.NewIdent("output"),
										Sel: ast.NewIdent("Arn"),
									},
								},
							},
						},
					},
				},
				// return diags
				&ast.ReturnStmt{
					Results: []ast.Expr{
						ast.NewIdent("diags"),
					},
				},
			},
		},
	}

	// Add blank line comment for readability
	blankLine := &ast.EmptyStmt{}

	return []ast.Stmt{blankLine, outputDecl, blankLine, ifStmt, blankLine}
}

func ensureConnsImport(file *ast.File) {
	// Check if conns is already imported
	connsImported := false
	for _, imp := range file.Imports {
		if imp.Path.Value == `"github.com/hashicorp/terraform-provider-aws/internal/conns"` {
			connsImported = true
			break
		}
	}

	if connsImported {
		return
	}

	// Find or create import declaration
	for _, decl := range file.Decls {
		if genDecl, ok := decl.(*ast.GenDecl); ok && genDecl.Tok == token.IMPORT {
			// Add to existing import block
			newImport := &ast.ImportSpec{
				Path: &ast.BasicLit{
					Kind:  token.STRING,
					Value: `"github.com/hashicorp/terraform-provider-aws/internal/conns"`,
				},
			}
			genDecl.Specs = append(genDecl.Specs, newImport)
			return
		}
	}

	// No import block exists, create one
	newImportDecl := &ast.GenDecl{
		Tok: token.IMPORT,
		Specs: []ast.Spec{
			&ast.ImportSpec{
				Path: &ast.BasicLit{
					Kind:  token.STRING,
					Value: `"github.com/hashicorp/terraform-provider-aws/internal/conns"`,
				},
			},
		},
	}

	// Insert at the beginning of declarations
	file.Decls = append([]ast.Decl{newImportDecl}, file.Decls...)
}
