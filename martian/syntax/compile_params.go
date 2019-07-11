// Copyright (c) 2018 10X Genomics, Inc. All rights reserved.

// compile/check params, bindings, and expressions.

package syntax

import (
	"fmt"
	"regexp"
	"strings"
)

func (params *InParams) compile(global *Ast) error {
	var errs ErrorList
	for _, param := range params.List {
		// Check for duplicates
		if _, ok := params.Table[param.GetId()]; ok {
			errs = append(errs, global.err(param,
				"DuplicateNameError: parameter '%s' was already declared when encountered again",
				param.GetId()))
		} else {
			params.Table[param.GetId()] = param
		}

		// Check that types exist.
		if t := global.TypeTable.Get(param.GetTname()); t == nil {
			errs = append(errs, global.err(param,
				"TypeError: undefined type '%s'",
				param.GetTname().Tname))
		} else {
			param.setIsFile(t.IsFile())
		}
	}
	return errs.If()
}

// IsLegalUnixFilename returns nil for legal file names, or an error
// describing the reason why the file name is illegal.
func IsLegalUnixFilename(name string) error {
	if len(name) > 255 {
		return fmt.Errorf("too long")
	}
	if name == "" {
		return fmt.Errorf("empty string")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("reserved name")
	}
	for _, c := range name {
		if c == '/' {
			return fmt.Errorf("'/' is not allowed in filenames")
		} else if c == 0 {
			return fmt.Errorf("null characters are not allowed in filenames")
		}
	}
	return nil
}

func (param *OutParam) compile(global *Ast) error {
	var errs ErrorList
	// Check that types exist.
	if t := global.TypeTable.Get(param.GetTname()); t == nil {
		errs = append(errs, global.err(param,
			"TypeError: undefined type '%s'",
			param.GetTname().Tname))
	} else {
		// Cache if param is file or path.
		param.setIsFile(t.IsFile())
		switch t.(type) {
		case *BuiltinType, *UserType:
			param.isComplex = false
		default:
			param.isComplex = true
		}
	}
	if fk := param.IsFile(); fk == KindIsFile || fk == KindIsDirectory {
		if param.OutName != "" {
			if err := IsLegalUnixFilename(param.OutName); err != nil {
				errs = append(errs, global.err(
					param,
					"OutName: illegal filename %q: %v",
					param.OutName, err))
			}
		}
	}
	return errs.If()
}

func (params *OutParams) compile(global *Ast) error {
	var errs ErrorList
	for _, param := range params.List {
		// Check for duplicates
		if _, ok := params.Table[param.GetId()]; ok {
			errs = append(errs, global.err(param,
				"DuplicateNameError: parameter '%s' was already declared when encountered again",
				param.GetId()))
		} else {
			params.Table[param.GetId()] = param
		}
		if err := param.compile(global); err != nil {
			errs = append(errs, err)
		}
	}
	return errs.If()
}

var windowsDeviceNameRe = regexp.MustCompile(`^(?:(?i:CON|PRN|AUX|NUL)|(?i:COM|LPT)[0-9])(?:$|\.)`)

func checkLegalFilename(name string) error {
	if len(name) > 128 {
		return fmt.Errorf("too long")
	}
	for _, c := range name {
		switch c {
		case '|', '/', '\\', '<', '>', '?', '*', ':', '"',
			'\a', '\b', '\f', '\n', '\r', '\t', '\v', 0:
			return fmt.Errorf("'%c' is not a legal character", c)
		}
	}
	if strings.HasSuffix(name, " ") {
		return fmt.Errorf("name cannot end with space")
	}
	if strings.HasSuffix(name, ".") {
		return fmt.Errorf("name cannot end with .")
	}
	if n := windowsDeviceNameRe.FindString(name); n != "" {
		return fmt.Errorf(
			"%s conflicts with a reserved windows device name",
			n)
	}
	return nil
}

func (param *OutParam) checkFilename() error {
	if fk := param.IsFile(); fk != KindIsFile && fk != KindIsDirectory {
		return nil
	}
	if param.OutName != "" {
		if err := checkLegalFilename(param.OutName); err != nil {
			return &wrapError{
				innerError: fmt.Errorf("out file name %q for parameter %s is not "+
					"legal under Microsoft Windows operating systems "+
					"and may cause issues for users who export their "+
					"results to such filesystems: %v",
					param.OutName, param.Id, err),
				loc: param.Node.Loc,
			}
		}
	} else if windowsDeviceNameRe.MatchString(param.Id) {
		return &wrapError{
			innerError: fmt.Errorf("parameter %s, which is a file output, "+
				"conflicts with a 'device file' name on Microsoft Windows, "+
				"and will cause issues for users on such filesystems",
				param.Id),
			loc: param.Node.Loc,
		}
	}
	return nil
}

// Returns an error if one or more of the output parameters will generate
// file names which are potentially problematic.
func (params *OutParams) CheckFilenames() error {
	if params == nil {
		return nil
	}
	var errs ErrorList
	for _, param := range params.List {
		if err := param.checkFilename(); err != nil {
			errs = append(errs, err)
		}
	}
	return errs.If()
}

func (exp *RefExp) resolveType(global *Ast, pipeline *Pipeline) (TypeId, error) {
	if pipeline == nil {
		return TypeId{}, global.err(exp,
			"ReferenceError: this binding cannot be resolved outside of a stage or pipeline.")
	}

	switch exp.getKind() {

	// Param: self.myparam
	case KindSelf:
		param, ok := pipeline.GetInParams().Table[exp.Id]
		if !ok {
			return TypeId{}, global.err(exp,
				"ScopeNameError: '%s' is not an input parameter of pipeline '%s'",
				exp.Id, pipeline.GetId())
		}
		if t, err := fieldType(param.GetTname(), &global.TypeTable, exp.OutputId); err != nil {
			return t, &StructFieldError{
				Message: "could not evaluate self." + exp.Id + "." + exp.OutputId,
				InnerError: &wrapError{
					innerError: err,
					loc:        exp.Node.Loc,
				},
			}
		} else {
			return t, nil
		}

	// Call: STAGE.myoutparam or STAGE
	case KindCall:
		callable, ok := pipeline.Callables.Table[exp.Id]
		if !ok {
			return TypeId{}, global.err(exp,
				"ScopeNameError: '%s' is not called in pipeline '%s'",
				exp.Id, pipeline.Id)
		}
		if exp.OutputId == "" {
			return TypeId{Tname: callable.GetId()}, nil
		}
		// Check referenced output is actually an output of the callable.
		idParts := strings.SplitN(exp.OutputId, ".", 2)
		param, ok := callable.GetOutParams().Table[idParts[0]]
		if !ok {
			return TypeId{}, global.err(exp,
				"NoSuchOutputError: '%s' is not an output parameter of '%s'",
				exp.OutputId, callable.GetId())
		}
		if len(idParts) == 1 {
			return param.GetTname(), nil
		} else if t, err := fieldType(param.GetTname(),
			&global.TypeTable, idParts[1]); err != nil {
			return t, &StructFieldError{
				Message: "could not evaluate " + exp.Id + "." + exp.OutputId,
				InnerError: &wrapError{
					innerError: err,
					loc:        exp.Node.Loc,
				},
			}
		} else {
			return t, nil
		}

	}
	return TypeId{}, nil
}

func (bindings *BindStms) compile(global *Ast, pipeline *Pipeline, params *InParams) error {
	// Check the bindings
	var errs ErrorList
	for _, binding := range bindings.List {
		// Collect bindings by id so we can check that all params are bound.
		if _, ok := bindings.Table[binding.Id]; ok {
			errs = append(errs, global.err(binding,
				"DuplicateBinding: '%s' already bound in this call",
				binding.Id))
		}
		// Building the bindings table could also happen in the grammar rules,
		// but then we lose the ability to detect duplicate parameters as we're
		// doing right above this comment. So leave this here.
		bindings.Table[binding.Id] = binding

		if err := binding.compile(global, pipeline, params); err != nil {
			errs = append(errs, err)
		}
	}

	if params != nil {
		// Check that all input params of the called segment are bound.
		for _, param := range params.List {
			if _, ok := bindings.Table[param.GetId()]; !ok {
				errs = append(errs, global.err(bindings,
					"ArgumentNotSuppliedError: no argument supplied for parameter '%s'",
					param.GetId()))
			}
		}
	}
	return errs.If()
}

func (binding *BindStm) compile(global *Ast, pipeline *Pipeline, params *InParams) error {
	// Make sure the bound-to id is a declared parameter of the callable.
	param, ok := params.Table[binding.Id]
	if !ok {
		return global.err(binding, "ArgumentError: '%s' is not a valid parameter",
			binding.Id)
	}
	return binding.compileParam(global, pipeline, param)
}

func isBackwardsCompatibleType(t Type) bool {
	switch t := t.(type) {
	case *StructType, *TypedMapType:
		return false
	case *ArrayType:
		return isBackwardsCompatibleType(t.Elem)
	default:
		return true
	}
}

// In martian 3 and below, a call binding like "arg = STAGE" was shorthand for
// "arg = STAGE.default".  Now it means to bind all of the outputs of STAGE as
// a struct.  However, for backwards compatibility here we check to see if
// adding .default will work, though not for "complex" types like structs,
// arrays, or typed maps.
func (binding *BindStm) rewriteToDefaultOutput(global *Ast,
	pipeline *Pipeline, t Type) bool {
	if exp, ok := binding.Exp.(*RefExp); ok && exp.OutputId == "" {
		if !isBackwardsCompatibleType(t) {
			return false
		}
		defExp := *exp
		defExp.OutputId = default_out_name
		if tname, err := defExp.resolveType(global, pipeline); err == nil {
			if tname.MapDim == 0 {
				if rt := global.TypeTable.Get(tname); rt != nil &&
					t.IsAssignableFrom(rt, &global.TypeTable) == nil {
					binding.Exp = &defExp
					return true
				}
			}
		}
	}
	return false
}

func (binding *BindStm) compileParam(global *Ast, pipeline *Pipeline, param Param) error {
	binding.Tname = param.GetTname()
	t := global.TypeTable.Get(binding.Tname)
	if t == nil {
		return global.err(binding, fmt.Sprintf(
			"BindingError: invalid type %q for parameter %q",
			binding.Tname, binding.Id))
	}
	if err := t.IsValidExpression(binding.Exp, pipeline, global); err != nil {
		if !binding.rewriteToDefaultOutput(global, pipeline, t) {
			return &wrapError{
				innerError: &IncompatibleTypeError{
					Message: "TypeMismatchError: binding parameter " +
						binding.Id + " to value " + binding.Exp.GoString(),
					Reason: err,
				},
				loc: binding.getNode().Loc,
			}
		}
	}
	return nil
}

func (bindings *BindStms) compileReturns(global *Ast, pipeline *Pipeline, params *OutParams) error {
	// Check the bindings
	var errs ErrorList
	for _, binding := range bindings.List {
		// Collect bindings by id so we can check that all params are bound.
		if _, ok := bindings.Table[binding.Id]; ok {
			errs = append(errs, global.err(binding,
				"DuplicateBinding: '%s' already bound in this call",
				binding.Id))
		}
		// Building the bindings table could also happen in the grammar rules,
		// but then we lose the ability to detect duplicate parameters as we're
		// doing right above this comment. So leave this here.
		bindings.Table[binding.Id] = binding

		if err := binding.compileReturns(global, pipeline, params); err != nil {
			errs = append(errs, err)
		}
	}

	if params != nil {
		// Check that all input params of the called segment are bound.
		for _, param := range params.List {
			if _, ok := bindings.Table[param.GetId()]; !ok {
				errs = append(errs, global.err(bindings,
					"ArgumentNotSuppliedError: no argument supplied for parameter '%s'",
					param.GetId()))
			}
		}
	}
	return errs.If()
}

func (binding *BindStm) compileReturns(global *Ast, pipeline *Pipeline, params *OutParams) error {
	// Make sure the bound-to id is a declared parameter of the callable.
	param, ok := params.Table[binding.Id]
	if !ok {
		return global.err(binding, "ArgumentError: '%s' is not a valid parameter",
			binding.Id)
	}
	return binding.compileParam(global, pipeline, param)
}

func getBoundParamIds(exp Exp) []string {
	switch exp := exp.(type) {
	case *RefExp:
		if exp.Kind == KindSelf {
			return []string{exp.Id}
		}
	case *ArrayExp:
		ids := make([]string, 0, len(exp.Value))
		for _, subExp := range exp.Value {
			ids = append(ids, getBoundParamIds(subExp)...)
		}
		return ids
	case *MapExp:
		ids := make([]string, 0, len(exp.Value))
		for _, subExp := range exp.Value {
			ids = append(ids, getBoundParamIds(subExp)...)
		}
		return ids
	}
	return nil
}
