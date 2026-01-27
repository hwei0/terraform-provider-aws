package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"go/types"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// SDKCallMetadata represents the metadata for an SDK call from the extract-sdk-calls command
type SDKCallMetadata struct {
	Name             string   `json:"Name"`
	PossibleServices []string `json:"PossibleServices"`
	Metadata         struct {
		StartPosition []int `json:"StartPosition"`
		EndPosition   []int `json:"EndPosition"`
	} `json:"Metadata"`
}

func extractPositions(fileset *token.FileSet, nodeList []*ast.FuncDecl) []token.Position {
	res := make([]token.Position, len(nodeList))
	for idx, node := range nodeList {
		res[idx] = fileset.Position(node.Pos())
	}

	return res
}

// parseLocation parses a location string in the format "file:line.column-line.column"
// and returns the starting position
func parseLocation(location string) (token.Position, error) {
	// Example: "./path/to/file.go:226.11-226.43"
	parts := strings.Split(location, ":")
	if len(parts) != 2 {
		return token.Position{}, fmt.Errorf("invalid location format: %s", location)
	}

	// Split the position part by '-' to get start and end
	positions := strings.Split(parts[1], "-")
	if len(positions) == 0 {
		return token.Position{}, fmt.Errorf("invalid position format: %s", location)
	}

	// Parse the start position "line.column"
	startParts := strings.Split(positions[0], ".")
	if len(startParts) != 2 {
		return token.Position{}, fmt.Errorf("invalid start position format: %s", positions[0])
	}

	line, err := strconv.Atoi(startParts[0])
	if err != nil {
		return token.Position{}, fmt.Errorf("parsing line number: %w", err)
	}

	column, err := strconv.Atoi(startParts[1])
	if err != nil {
		return token.Position{}, fmt.Errorf("parsing column number: %w", err)
	}

	return token.Position{
		Filename: parts[0],
		Line:     line,
		Column:   column,
	}, nil
}

// extractSDKCallPositions calls the extract-sdk-calls command and returns SDK call positions
func extractSDKCallPositions(filePath, extractorPath string) ([]token.Position, error) {
	// Run the extract-sdk-calls command
	cmd := exec.Command(extractorPath, "extract-sdk-calls", filePath, "--full-output", "--pretty")
	output, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("running extract-sdk-calls: %w", err)
	}

	// Parse the JSON output
	var sdkCalls []SDKCallMetadata
	if err := json.Unmarshal(output, &sdkCalls); err != nil {
		return nil, fmt.Errorf("parsing SDK calls JSON: %w", err)
	}
	// fmt.Printf("[debug] json ouptut: %v\n", sdkCalls)
	// Extract positions from SDK calls
	positions := make([]token.Position, 0, len(sdkCalls))
	for _, call := range sdkCalls {
		pos := token.Position{Line: call.Metadata.StartPosition[0], Column: call.Metadata.StartPosition[1]}
		positions = append(positions, pos)
	}

	return positions, nil
}

// writeFunctionsToFile writes a list of function declarations to a Go file
// with the package name and imports from the original file
func writeFunctionsToFile(funcDecls []*ast.FuncDecl, fileNode *ast.File, outputFilePath string) error {
	// Create output directory if it doesn't exist
	outputDir := filepath.Dir(outputFilePath)
	if err := os.MkdirAll(outputDir, 0755); err != nil {
		return fmt.Errorf("creating output directory: %w", err)
	}

	// Create a buffer to write the file content
	var buf bytes.Buffer
	fset := token.NewFileSet()

	// Write package declaration
	buf.WriteString("package ")
	buf.WriteString(fileNode.Name.Name)
	buf.WriteString("\n\n")

	// Write imports using printer.Fprint to copy the AST node
	if len(fileNode.Imports) > 0 {
		// Create a GenDecl for the imports
		importDecl := &ast.GenDecl{
			Tok:   token.IMPORT,
			Specs: make([]ast.Spec, len(fileNode.Imports)),
		}
		for i, imp := range fileNode.Imports {
			importDecl.Specs[i] = imp
		}

		if err := printer.Fprint(&buf, fset, importDecl); err != nil {
			return fmt.Errorf("printing imports: %w", err)
		}
		buf.WriteString("\n\n")
	}

	// Write each function declaration using printer.Fprint
	for _, funcDecl := range funcDecls {
		if err := printer.Fprint(&buf, fset, funcDecl); err != nil {
			return fmt.Errorf("printing function %s: %w", funcDecl.Name.Name, err)
		}
		buf.WriteString("\n\n")
	}

	// Write to file
	if err := os.WriteFile(outputFilePath, buf.Bytes(), 0644); err != nil {
		return fmt.Errorf("writing file %s: %w", outputFilePath, err)
	}

	return nil
}

// shouldSkipFile returns true if the file should be skipped during processing
func shouldSkipFile(filename string) bool {
	// Skip test files
	if strings.HasSuffix(filename, "_test.go") {
		return true
	}

	// Skip specific generated and infrastructure files
	skipFiles := []string{
		"service_package_gen.go",
		"service_package.go",
		"service_endpoint_resolver_gen.go",
		"service_endpoints_gen_test.go",
		"tags.go",
		"tags_gen.go",
		"tags_gen_test.go",
	}

	for _, skipFile := range skipFiles {
		if filename == skipFile {
			return true
		}
	}

	return false
}

func isBefore(posA token.Position, posB token.Position) bool {
	if posA.Line < posB.Line {
		return true
	}
	if posA.Line == posB.Line {
		return posA.Column < posB.Column
	}
	return false
}

func isAfter(posA token.Position, posB token.Position) bool {
	if posA.Line > posB.Line {
		return true
	}
	if posA.Line == posB.Line {
		return posA.Column > posB.Column
	}
	return false
}

// resolveAllFuncDecl iterates through all Go files in a directory,
// parses their ASTs, and returns a map of function names to their declarations
func resolveAllFuncDecl(dir string) (*token.FileSet, map[string]*[]*ast.FuncDecl, error) {
	allServiceFuncDecl := make(map[string]*[]*ast.FuncDecl)
	fset := token.NewFileSet()

	// Read all entries in the directory
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, fmt.Errorf("reading directory %s: %w", dir, err)
	}

	// Iterate through all files in the directory
	for _, entry := range entries {
		// Skip directories and non-Go files
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") {
			continue
		}

		// Skip test files and generated files
		if shouldSkipFile(entry.Name()) {
			continue
		}

		// Construct full file path
		filePath := filepath.Join(dir, entry.Name())

		// Parse the Go file
		fileNode, err := parser.ParseFile(fset, filePath, nil, parser.ParseComments)
		if err != nil {
			return nil, nil, fmt.Errorf("parsing file %s: %w", filePath, err)
		}

		// Inspect the AST to find all function declarations
		ast.Inspect(fileNode, func(n ast.Node) bool {
			funcDecl, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}

			// Get the function name
			funcName := funcDecl.Name.Name

			// Add the function declaration to the map
			if _, exists := allServiceFuncDecl[funcName]; !exists {
				funcList := make([]*ast.FuncDecl, 0)
				allServiceFuncDecl[funcName] = &funcList
			}
			*allServiceFuncDecl[funcName] = append(*allServiceFuncDecl[funcName], funcDecl)
			// if len(*allServiceFuncDecl[funcName]) > 1 {
			// 	locations := make([]token.Position, len(*allServiceFuncDecl[funcName]))
			// 	for idx, funcDecl := range *allServiceFuncDecl[funcName] {
			// 		locations[idx] = fset.Position(funcDecl.Pos())
			// 	}
			// 	fmt.Printf("[warn] Multiple declarations found for function '%s' (count: %d) in %s: %v\n",
			// 		funcName, len(*allServiceFuncDecl[funcName]), filePath, locations)
			// }

			return true
		})
	}

	return fset, allServiceFuncDecl, nil
}

func createFuncDeclAnalyzedStatusMap(allServiceFuncDecl map[string]*[]*ast.FuncDecl) map[*ast.FuncDecl]bool {
	outMap := make(map[*ast.FuncDecl]bool, 10000)
	for _, astNodeList := range allServiceFuncDecl {
		for _, astNode := range *astNodeList {
			outMap[astNode] = false
		}
	}

	return outMap
}

type InitialFuncDeclTracker struct {
	beforeFuncDeclList       *[]*ast.FuncDecl
	intermediateFuncDeclList *[]*ast.FuncDecl
	afterFuncDeclList        *[]*ast.FuncDecl

	beforeFuncDeclAnalyzedStatus       map[*ast.FuncDecl]bool
	intermediateFuncDeclAnalyzedStatus map[*ast.FuncDecl]bool
	afterFuncDeclAnalyzedStatus        map[*ast.FuncDecl]bool

	firstCallPosition token.Position
	lastCallPosition  token.Position

	initialFileSet *token.FileSet
}

type CreateCallMetadata struct {
	ServiceDirName        string
	FilePath              string
	TerraformResourceName []string
	SdkResourceName       []string
	CreateFunctionName    string
	ResourceDecorators    []string
	FirstCallRow          int
	FirstCallCol          int
	LastCallRow           int
	LastCallCol           int
	BeforeFuncDecls       []token.Position
	IntermediateFuncDecls []token.Position
	AfterFuncDecls        []token.Position
	AllFuncDecls          []token.Position
}

// MUST UPDATE RESLIST, FUNCDECLANALYZEDSTATUS BEFORE CALLING THIS FUNCTION.
// TODO: do you need to also collect all the imports, if resolving functions from multiple files??? eg DDB?
func recurseFunctionCallAnalysis(n *ast.FuncDecl, resList *[]*ast.FuncDecl, funcDeclAnalyzedStatus map[*ast.FuncDecl]bool, allServiceFuncDecl map[string]*[]*ast.FuncDecl, allServiceFileSet *token.FileSet, isInitial bool, initialStates InitialFuncDeclTracker) error {

	var err error
	if n.Body == nil {
		fmt.Printf("[warn] got a nil body: %s\n", n.Name.String())
		return nil
	}
	for _, stmt := range n.Body.List {
		ast.Inspect(stmt, func(n ast.Node) bool {
			funcCall, ok := n.(*ast.CallExpr)
			if err != nil {
				return false
			}
			if !ok {
				return true
			}
			var nextFuncName string
			switch callFun := funcCall.Fun.(type) {
			case *ast.SelectorExpr:
				nextFuncName = callFun.Sel.Name
				break
			case *ast.Ident:
				nextFuncName = callFun.Name
				break
			default:
				// var errPosition token.Position
				// if isInitial {
				// 	errPosition = initialStates.initialFileSet.Position(funcCall.Pos())
				// } else {
				// 	errPosition = initialStates.allFuncFileSet.Position(funcCall.Pos())
				// }

				fmt.Printf("[warn] unsupported function call type in recurseFunctionCallAnalysis: %T; %s\n",
					funcCall.Fun, types.ExprString(funcCall))
				// err = fmt.Errorf("unsupported function call type in recurseFunctionCallAnalysis: %T; %s\n",
				// 	funcCall.Fun, types.ExprString(funcCall))
				// return false
			}

			nextFuncList, ok := allServiceFuncDecl[nextFuncName]

			if !ok {
				return true
			}

			if len(*nextFuncList) > 1 {
				debugLocations := make([]token.Position, len(*nextFuncList))
				for idx, funcDecl := range *nextFuncList {
					debugLocations[idx] = allServiceFileSet.Position(funcDecl.Pos())
				}
				fmt.Printf("[warn] Multiple declarations found for function '%s' (count: %d): %v\n",
					nextFuncName, len(*nextFuncList), debugLocations)
			}

			if !ok {
				return true
			}

			for _, nextFunc := range *nextFuncList {
				var funcDeclAnalyzedStatusNext map[*ast.FuncDecl]bool
				var resListNext *[]*ast.FuncDecl

				if isInitial {
					if isBefore(initialStates.initialFileSet.Position(funcCall.Pos()), initialStates.firstCallPosition) {
						funcDeclAnalyzedStatusNext = initialStates.beforeFuncDeclAnalyzedStatus
						resListNext = initialStates.beforeFuncDeclList
					} else if isAfter(initialStates.initialFileSet.Position(funcCall.Pos()), initialStates.lastCallPosition) {
						funcDeclAnalyzedStatusNext = initialStates.afterFuncDeclAnalyzedStatus
						resListNext = initialStates.afterFuncDeclList
					} else {
						funcDeclAnalyzedStatusNext = initialStates.intermediateFuncDeclAnalyzedStatus
						resListNext = initialStates.intermediateFuncDeclList
					}
				} else {
					funcDeclAnalyzedStatusNext = funcDeclAnalyzedStatus
					resListNext = resList
				}

				nextFuncAnalyzed, ok := funcDeclAnalyzedStatusNext[nextFunc]

				if !ok {
					err = fmt.Errorf("function '%s' not found in analyzed status map (possible internal error in createFuncDeclAnalyzedStatusMap)", nextFuncName)
					return false
				}

				if nextFuncAnalyzed {
					continue
				}

				funcDeclAnalyzedStatusNext[nextFunc] = true

				*resListNext = append(*resListNext, nextFunc)

				errs := recurseFunctionCallAnalysis(nextFunc, resListNext, funcDeclAnalyzedStatusNext, allServiceFuncDecl, allServiceFileSet, false, initialStates)

				if errs != nil {
					err = errs
					return false
				}
			}

			return true
		})

		if err != nil {
			return err
		}
	}
	return err
}

func processResourceFile(inputPath string, serviceDirName string, extractorPath string, allServiceFuncDecl map[string]*[]*ast.FuncDecl, allServiceFileSet *token.FileSet, outputPath string) (bool, error) {
	fset := token.NewFileSet()

	// Parse the file
	fileNode, err := parser.ParseFile(fset, inputPath, nil, parser.ParseComments)
	if err != nil {
		return false, fmt.Errorf("parsing: %w", err)
	}

	var resourceHeaderNode *ast.FuncDecl

	ast.Inspect(fileNode, func(n ast.Node) bool {
		fn, ok := n.(*ast.FuncDecl)
		if !ok {
			return true
		}

		if fn.Type.Results == nil || fn.Type.Results.List == nil || len(fn.Type.Results.List) != 1 {
			return true
		}

		var resString = types.ExprString(fn.Type.Results.List[0].Type)
		if resString != "*schema.Resource" {
			return true
		}

		resourceHeaderNode = fn
		return true
	})

	if resourceHeaderNode == nil {
		return false, nil //fmt.Errorf("parsing %s failed as resource header not found", inputPath)
	}

	resourceHeaderComments := make([]string, 0)
	if resourceHeaderNode.Doc != nil {
		for _, commentNode := range resourceHeaderNode.Doc.List {
			resourceHeaderComments = append(resourceHeaderComments, commentNode.Text)
		}
	} else {
		return false, nil
	}

	fmt.Printf(".     here are the comment lines: %v\n", resourceHeaderComments)

	// Extract terraform resource name and SDK resource name from @SDKResource decorator
	// Pattern allows for whitespaces after // and multiple additional slashes
	terraformResourceName := make([]string, 0)
	sdkResourceName := make([]string, 0)
	sdkResourcePattern := regexp.MustCompile(`\s*//\s*/*\s*@SDKResource\(\s*"([^"]+)"\s*,\s*name="([^"]+)"\s*\)`)

	for _, comment := range resourceHeaderComments {
		matches := sdkResourcePattern.FindStringSubmatch(comment)
		if len(matches) == 3 {
			terraformResourceName = append(terraformResourceName, matches[1]) // e.g., "aws_acm_certificate"
			sdkResourceName = append(sdkResourceName, matches[2])             // e.g., "Certificate"
		}
	}

	if len(terraformResourceName) == 0 {
		return false, nil
	}

	resourceCreateFuncNameList := make([]string, 0)

	ast.Inspect(resourceHeaderNode, func(n ast.Node) bool {
		returnNode, ok := n.(*ast.ReturnStmt)
		if !ok {
			return true
		}

		if returnNode.Results == nil || len(returnNode.Results) != 1 {
			return true
		}

		returnExpr := returnNode.Results[0]

		returnUnaryExpr, ok := returnExpr.(*ast.UnaryExpr)

		if !ok {
			return true
		}

		returnUnaryExprCompositeLit, ok := returnUnaryExpr.X.(*ast.CompositeLit)
		if !ok || returnUnaryExpr.Op != token.AND {
			return true
		}

		if types.ExprString(returnUnaryExprCompositeLit.Type) != "schema.Resource" {
			return true
		}

		for _, resourceElem := range returnUnaryExprCompositeLit.Elts {
			resourceKeyValElem, ok := resourceElem.(*ast.KeyValueExpr)
			if !ok {
				continue
			}

			resourceElemKey, ok := resourceKeyValElem.Key.(*ast.Ident)
			if !ok || resourceElemKey.Name != "CreateWithoutTimeout" {
				continue
			}

			switch resourceElemValue := resourceKeyValElem.Value.(type) {
			case *ast.Ident:
				resourceCreateFuncNameList = append(resourceCreateFuncNameList, resourceElemValue.Name)
				break
			case *ast.CallExpr:
				// glue resource_policy.go
				resourceElemValueAlternateIdent, okAlt2 := resourceElemValue.Fun.(*ast.Ident)

				if !okAlt2 {
					continue
				}

				resourceCreateFuncNameList = append(resourceCreateFuncNameList, resourceElemValueAlternateIdent.Name)
				break
			case *ast.SelectorExpr:
				// sqs queue_policy.go
				identName := resourceElemValue.Sel.Name
				resourceCreateFuncNameList = append(resourceCreateFuncNameList, identName)
				break
			default:
				continue
			}
		}

		return true
	})

	if len(resourceCreateFuncNameList) == 0 {
		return false, fmt.Errorf("no resources")
	}
	if len(resourceCreateFuncNameList) > 1 {
		return false, fmt.Errorf("more than 1 resource functions")
	}

	for _, resourceCreateFuncName := range resourceCreateFuncNameList {
		var resourceCreateFunctionNode *ast.FuncDecl

		var err error

		ast.Inspect(fileNode, func(n ast.Node) bool {
			funcNode, ok := n.(*ast.FuncDecl)

			if err != nil {
				return false
			}

			if !ok {
				return true
			}

			if funcNode.Name.Name != resourceCreateFuncName {
				return true
			}

			if resourceCreateFunctionNode != nil {
				err = fmt.Errorf("duplicate create function '%s' found in %s (this should not happen)", resourceCreateFuncName, inputPath)
				return false
			}
			resourceCreateFunctionNode = funcNode
			return true
		})

		if resourceCreateFunctionNode == nil {
			//special handling for sqs queue_policy.go
			funcDeclList, ok := allServiceFuncDecl[resourceCreateFuncName]
			if !ok {
				return false, fmt.Errorf("did not find resourceCreateFunction: %s\n", resourceCreateFuncName)
			}
			if len(*funcDeclList) > 1 {
				return false, fmt.Errorf("tried going outside for functions matching %s, but found more than one.\n", resourceCreateFuncName)
			}

			resourceCreateFunctionNode = (*funcDeclList)[0]
		}

		// Extract SDK call positions from the file
		sdkPositions, err := extractSDKCallPositions(inputPath, extractorPath)
		if err != nil {
			return false, fmt.Errorf("extracting SDK call positions: %w", err)
		}
		fmt.Printf("    Found %d SDK call(s) in file\n", len(sdkPositions))

		firstSdkPosition := token.Position{Column: -1, Line: -1}
		lastSdkPosition := token.Position{Column: -1, Line: -1}

		for _, candidatePosition := range sdkPositions {
			if isBefore(candidatePosition, fset.Position(resourceCreateFunctionNode.Pos())) || isAfter(candidatePosition, fset.Position(resourceCreateFunctionNode.End())) {
				continue
			}
			if firstSdkPosition.Column < 0 {
				firstSdkPosition = candidatePosition
			}
			if lastSdkPosition.Column < 0 {
				lastSdkPosition = candidatePosition
			}

			if isBefore(candidatePosition, firstSdkPosition) {
				firstSdkPosition = candidatePosition
			}
			if isAfter(candidatePosition, lastSdkPosition) {
				lastSdkPosition = candidatePosition
			}
		}

		beforeFuncDeclList := make([]*ast.FuncDecl, 0) // todo: initialize
		intermediateFuncDeclList := make([]*ast.FuncDecl, 0)
		afterFuncDeclList := make([]*ast.FuncDecl, 0)

		beforeFuncDeclAnalyzedStatus := createFuncDeclAnalyzedStatusMap(allServiceFuncDecl) // todo: initialize
		intermediateFuncDeclAnalyzedStatus := createFuncDeclAnalyzedStatusMap(allServiceFuncDecl)
		afterFuncDeclAnalyzedStatus := createFuncDeclAnalyzedStatusMap(allServiceFuncDecl)

		// CREATE THE InitialFuncDeclTracker struct using the above vars, after initializing them

		initFuncDeclTracker := InitialFuncDeclTracker{
			beforeFuncDeclList:       &beforeFuncDeclList,
			intermediateFuncDeclList: &intermediateFuncDeclList,
			afterFuncDeclList:        &afterFuncDeclList,

			beforeFuncDeclAnalyzedStatus:       beforeFuncDeclAnalyzedStatus,
			intermediateFuncDeclAnalyzedStatus: intermediateFuncDeclAnalyzedStatus,
			afterFuncDeclAnalyzedStatus:        afterFuncDeclAnalyzedStatus,

			firstCallPosition: firstSdkPosition,
			lastCallPosition:  lastSdkPosition,
			initialFileSet:    fset,
		}

		// make FIRST call to recurseFunctionCallAnalysis, SETTING IS_INITIAL TO TRUE AND resList, funcDeclAnalyzedStatus TO NIL; this populates beforeCalls, intermediateCalls, and afterCalls files

		if err := recurseFunctionCallAnalysis(resourceCreateFunctionNode, nil, nil, allServiceFuncDecl, allServiceFileSet, true, initFuncDeclTracker); err != nil {
			return false, err
		}

		allFuncDeclList := make([]*ast.FuncDecl, 1)                                      // todo: initialize
		allFuncDeclAnalyzedStatus := createFuncDeclAnalyzedStatusMap(allServiceFuncDecl) // todo: initialize

		//NOTE: even though we add to this list, we don't mark &resourceCreateFunctionNode as visited in allFuncDeclAnalyzedStatus, because our parsing that we construct via the ParseCall in this function, has AstNodes that are DIFFERENT and  DUPLICATES of the same AstNodes in the same file under allFuncDeclAnalyzedStatus.
		allFuncDeclList[0] = resourceCreateFunctionNode

		// make SECOND call to recurseFunctionCallAnalysis, SETTING IS_INITIAL TO FALSE AND resList, funcDeclAnalyzedStatus TO allFuncDeclList, allFuncDeclAnalyzedStatus; this populates the allCalls file

		if err := recurseFunctionCallAnalysis(resourceCreateFunctionNode, &allFuncDeclList, allFuncDeclAnalyzedStatus, allServiceFuncDecl, allServiceFileSet, false, InitialFuncDeclTracker{}); err != nil {
			return false, err
		}

		metadataStruct := CreateCallMetadata{
			ServiceDirName:        serviceDirName,
			FilePath:              inputPath,
			TerraformResourceName: terraformResourceName,
			SdkResourceName:       sdkResourceName,
			CreateFunctionName:    resourceCreateFuncName,
			ResourceDecorators:    resourceHeaderComments,
			FirstCallRow:          firstSdkPosition.Line,
			FirstCallCol:          firstSdkPosition.Column,
			LastCallRow:           lastSdkPosition.Line,
			LastCallCol:           lastSdkPosition.Column,
			BeforeFuncDecls:       extractPositions(allServiceFileSet, *initFuncDeclTracker.beforeFuncDeclList),
			IntermediateFuncDecls: extractPositions(allServiceFileSet, *initFuncDeclTracker.intermediateFuncDeclList),
			AfterFuncDecls:        extractPositions(allServiceFileSet, *initFuncDeclTracker.afterFuncDeclList),
			AllFuncDecls:          extractPositions(allServiceFileSet, *initFuncDeclTracker.afterFuncDeclList),
		}

		// Create subdirectory using the terraform resource name
		var resourceOutputPath string
		if len(terraformResourceName) > 0 {
			resourceOutputPath = filepath.Join(outputPath, terraformResourceName[0])
		} else {
			return false, fmt.Errorf("did not get terraformResourceName")
		}

		// Write before_calls.go
		beforeCallsPath := filepath.Join(resourceOutputPath, "before_calls.go")
		if err := writeFunctionsToFile(beforeFuncDeclList, fileNode, beforeCallsPath); err != nil {
			return false, fmt.Errorf("writing before_calls.go: %w", err)
		}

		// Write intermediate_calls.go
		intermediateCallsPath := filepath.Join(resourceOutputPath, "intermediate_calls.go")
		if err := writeFunctionsToFile(intermediateFuncDeclList, fileNode, intermediateCallsPath); err != nil {
			return false, fmt.Errorf("writing intermediate_calls.go: %w", err)
		}

		// Write after_calls.go
		afterCallsPath := filepath.Join(resourceOutputPath, "after_calls.go")
		if err := writeFunctionsToFile(afterFuncDeclList, fileNode, afterCallsPath); err != nil {
			return false, fmt.Errorf("writing after_calls.go: %w", err)
		}

		// Write create_function_calls.go
		createFunctionCallsPath := filepath.Join(resourceOutputPath, "create_function_calls.go")
		if err := writeFunctionsToFile(allFuncDeclList, fileNode, createFunctionCallsPath); err != nil {
			return false, fmt.Errorf("writing create_function_calls.go: %w", err)
		}

		// Write create_function_only.go
		createFunctionOnlyPath := filepath.Join(resourceOutputPath, "create_function_only.go")
		if err := writeFunctionsToFile([]*ast.FuncDecl{resourceCreateFunctionNode}, fileNode, createFunctionOnlyPath); err != nil {
			return false, fmt.Errorf("writing create_function_only.go: %w", err)
		}

		// Write metadata.json
		metadataPath := filepath.Join(resourceOutputPath, "metadata.json")
		metadataJSON, err := json.MarshalIndent(metadataStruct, "", "  ")
		if err != nil {
			return false, fmt.Errorf("marshaling metadata: %w", err)
		}
		if err := os.WriteFile(metadataPath, metadataJSON, 0644); err != nil {
			return false, fmt.Errorf("writing metadata.json: %w", err)
		}

		// Log statistics
		fmt.Printf("    Resource: %s (SDK: %s)\n", terraformResourceName[0], sdkResourceName[0])
		fmt.Printf("    Create function: %s\n", resourceCreateFuncName)
		fmt.Printf("    Function counts:\n")
		fmt.Printf("      - Before auxiliary function calls: %d functions\n", len(beforeFuncDeclList))
		fmt.Printf("      - Intermediate (between auxiliary function calls): %d functions\n", len(intermediateFuncDeclList))
		fmt.Printf("      - After auxiliary function calls: %d functions\n", len(afterFuncDeclList))
		fmt.Printf("      - Total functions in call chain: %d functions\n", len(allFuncDeclList))
		fmt.Printf("    Output directory: %s\n", resourceOutputPath)
	}

	return true, nil
}

func main() {
	// Parse command-line arguments
	if len(os.Args) < 4 {
		fmt.Fprintf(os.Stderr, "Usage: %s <input_directory> <extractor_path> <output_directory>\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  input_directory: directory containing service subdirectories (e.g., internal/service)\n")
		fmt.Fprintf(os.Stderr, "  extractor_path: path to the iam-policy-autopilot extractor executable\n")
		fmt.Fprintf(os.Stderr, "  output_directory: directory to write output files\n")
		os.Exit(1)
	}

	inputDir := os.Args[1]
	extractorPath := os.Args[2]
	outputDir := os.Args[3]

	// Read all subdirectories in the input directory
	entries, err := os.ReadDir(inputDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading input directory %s: %v\n", inputDir, err)
		os.Exit(1)
	}

	// Iterate through each subdirectory
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		serviceDirName := entry.Name()
		servicePath := filepath.Join(inputDir, serviceDirName)

		// if serviceDirName != "sqs" {
		// 	continue
		// }

		fmt.Printf("Processing service: %s\n", serviceDirName)

		// Call resolveAllFuncDecl for this service directory
		allServiceFileSet, allServiceFuncDecl, err := resolveAllFuncDecl(servicePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error resolving function declarations for %s: %v\n", serviceDirName, err)
			return
		}

		// Read all Go files in the service directory
		serviceEntries, err := os.ReadDir(servicePath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading service directory %s: %v\n", servicePath, err)
			return
		}

		// Process each Go file in the subdirectory
		for _, fileEntry := range serviceEntries {
			if fileEntry.IsDir() || !strings.HasSuffix(fileEntry.Name(), ".go") {
				continue
			}

			// Skip test files and generated files
			if shouldSkipFile(fileEntry.Name()) {
				fmt.Printf("  Skipping non-resource file %s\n", fileEntry.Name())
				continue
			}

			filePath := filepath.Join(servicePath, fileEntry.Name())
			fmt.Printf("  Processing file: %s\n", fileEntry.Name())

			// Invoke processResourceFile
			isResourceFile, err := processResourceFile(filePath, serviceDirName, extractorPath, allServiceFuncDecl, allServiceFileSet, outputDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "  Error processing %s: %v\n", filePath, err)
				return
			}
			if isResourceFile {
				fmt.Printf("  Successfully processed: %s\n", fileEntry.Name())
			} else {
				fmt.Printf("  Skipping non-resource file %s\n", fileEntry.Name())
			}
		}
	}

	fmt.Println("Processing complete!")
}
