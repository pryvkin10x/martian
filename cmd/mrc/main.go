//
// Copyright (c) 2014 10X Genomics, Inc. All rights reserved.
//
// Martian command-line compiler. Primarily used for unit testing.
//
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"

	"github.com/martian-lang/martian/martian/core"
	"github.com/martian-lang/martian/martian/syntax"
	"github.com/martian-lang/martian/martian/util"

	"github.com/martian-lang/docopt.go"
)

func main() {
	util.SetPrintLogger(os.Stderr)
	util.SetupSignalHandlers()
	// Command-line arguments.
	doc := `Martian Compiler.

Usage:
    mrc [options] <file.mro>...
    mrc [options]
    mrc -h | --help | --version

Options:
    --all           Compile all files in $MROPATH.
    --graph         Output the resolved call graph as json.
    --json          Output abstract syntax tree as JSON.
    --strict        Strict syntax validation
    --no-check-src  Do not check that stage source paths exist.
    --dot           Render the top-level pipeline to graphviz dot format.

    -h --help       Show this message.
    --version       Show version.`
	martianVersion := util.GetVersion()
	opts, _ := docopt.Parse(doc, nil, true, martianVersion, false)

	// Martian environment variables.
	cwd, _ := os.Getwd()
	mroPaths := util.ParseMroPath(cwd)
	if value := os.Getenv("MROPATH"); len(value) > 0 {
		mroPaths = util.ParseMroPath(value)
	}
	checkSrcPath := true
	if opts["--no-check-src"].(bool) {
		checkSrcPath = false
	}

	// Setup strictness
	syntax.SetEnforcementLevel(syntax.EnforceLog)
	if opts["--strict"].(bool) {
		syntax.SetEnforcementLevel(syntax.EnforceError)
	} else if flags := os.Getenv("MROFLAGS"); flags != "" {
		re := regexp.MustCompile(`-strict=(log|alarm|error)`)
		if match := re.FindStringSubmatch(flags); len(match) > 1 {
			syntax.SetEnforcementLevel(syntax.ParseEnforcementLevel(match[1]))
		}
	}
	mkjson := opts["--json"].(bool)
	callgraph := opts["--graph"].(bool)
	mkdot := opts["--dot"].(bool)

	count := 0
	wasErr := false
	if opts["--all"].(bool) {
		// Compile all MRO files in MRO path.
		num, asts, err := core.CompileAll(mroPaths, checkSrcPath)

		if err != nil {
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}

		if mkjson {
			fmt.Printf("%s", syntax.JsonDumpAsts(asts))
		}
		if callgraph {
			printCallGraphs(asts)
		}
		if mkdot {
			for _, ast := range asts {
				if len(ast.Pipelines) > 0 {
					p := ast.Pipelines[len(ast.Pipelines)-1]
					fmt.Println(p.RenderDot(p.Id, ast.Callables.Table, "", "  "))
				}
			}
		}
		for _, ast := range asts {
			if ast.Callables != nil {
				for _, callable := range ast.Callables.List {
					if err := callable.GetOutParams().CheckFilenames(); err != nil {
						fmt.Fprintln(os.Stderr, err.Error())
						if syntax.GetEnforcementLevel() >= syntax.EnforceAlarm {
							wasErr = true
						}
					}
				}
			}
		}
		count += num
	} else {
		// Compile just the specified MRO files.
		var asts []*syntax.Ast
		for _, fname := range opts["<file.mro>"].([]string) {
			if !filepath.IsAbs(fname) {
				fname = path.Join(cwd, fname)
			}
			_, _, ast, err := syntax.Compile(fname, mroPaths, checkSrcPath)
			if err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				wasErr = true
			} else {
				if ast.Callables != nil {
					for _, callable := range ast.Callables.List {
						if err := callable.GetOutParams().CheckFilenames(); err != nil {
							fmt.Fprintln(os.Stderr, err.Error())
							if syntax.GetEnforcementLevel() >= syntax.EnforceAlarm {
								wasErr = true
							}
						}
					}
				}

				if mkjson || callgraph {
					asts = append(asts, ast)
				}
				if mkdot && len(ast.Pipelines) > 0 {
					p := ast.Pipelines[len(ast.Pipelines)-1]
					fmt.Println(p.RenderDot(p.Id, ast.Callables.Table, "", "  "))
				}
				count++
			}
		}
		if mkjson {
			fmt.Printf("%s\n", syntax.JsonDumpAsts(asts))
		}
		if callgraph {
			printCallGraphs(asts)
		}
	}
	fmt.Fprintln(os.Stderr, "Successfully compiled", count, "mro files.")

	if wasErr {
		os.Exit(1)
	}
}

func printCallGraphs(asts []*syntax.Ast) bool {
	wasErr := false
	graphs := make([]syntax.CallGraphNode, 0, len(asts))
	for _, ast := range asts {
		if c := getBestCall(ast); c != nil {
			if cg, err := ast.MakeCallGraph("", c); err != nil {
				fmt.Fprintln(os.Stderr, err.Error())
				wasErr = true
			} else {
				graphs = append(graphs, cg)
			}
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if len(graphs) == 1 {
		enc.Encode(graphs[0])
	} else {
		enc.Encode(graphs)
	}
	return wasErr
}

// If the AST has a call, return it.  Otherwise, return the last
// callable defined in the top-level file.
func getBestCall(ast *syntax.Ast) *syntax.CallStm {
	if ast.Call != nil {
		return ast.Call
	}
	var found syntax.Callable
	for _, c := range ast.Callables.List {
		if f := c.File(); f == nil || len(f.IncludedFrom) == 0 {
			found = c
		}
	}
	if found == nil && len(ast.Callables.List) > 0 {
		found = ast.Callables.List[len(ast.Callables.List)-1]
	}
	if found == nil {
		return nil
	}
	return syntax.GenerateCall(found, nil)
}
