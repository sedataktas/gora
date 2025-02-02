package gora

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/hillu/go-yara/v4"

	"github.com/binalyze/gora/variables"
)

var ErrAlreadyCompiled = errors.New("already compiled")

// ScanTarget represents a target for yara scan.
type ScanTarget byte

// Scan targets are file system and process memory.
const (
	ScanFile ScanTarget = iota
	ScanProcess
)

// Compiled holds the compiled rules and its associated external variables.
type Compiled struct {
	vars    *variables.Variables
	rules   *yara.Rules
	scanner *yara.Scanner
}

func NewCompiled() *Compiled {
	return &Compiled{
		vars: new(variables.Variables),
	}
}

// RuleNamespace represents a rule and its namespace.
type RuleNamespace struct {
	Rule      string
	Namespace string
}

// CompileString compiles the YARA rules.
func (c *Compiled) CompileString(target ScanTarget, rule, namespace string) error {
	return c.CompileStrings(target, []RuleNamespace{{Rule: rule, Namespace: namespace}})
}

// CompileStrings compiles the YARA rules.
func (c *Compiled) CompileStrings(target ScanTarget, ruleNs []RuleNamespace) error {
	if c.rules != nil {
		return ErrAlreadyCompiled
	}

	compiler, err := yara.NewCompiler()
	if err != nil {
		return fmt.Errorf("yara compiler error: %w", err)
	}
	defer compiler.Destroy()

	parser := new(variables.Parser)
	var fallbackAllVars bool
	for _, rule := range ruleNs {
		if err = parser.ParseFromReader(strings.NewReader(rule.Rule)); err != nil {
			return fmt.Errorf("variable parser error: %w", err)
		}

		if len(parser.Includes()) > 0 {
			fallbackAllVars = true
		}
	}

	vars := parser.Variables()
	if fallbackAllVars {
		vars = variables.List()
	}

	if err := c.initVariables(target, vars); err != nil {
		return err
	}

	if err = c.vars.DefineCompilerVariables(compiler); err != nil {
		err = fmt.Errorf("compiler define variable error: %w", err)
		return compilerError(compiler, err)
	}

	for _, rule := range ruleNs {
		err = compiler.AddString(rule.Rule, rule.Namespace)
		if err != nil {
			err = fmt.Errorf("compiler add rule error: %w", err)
			return compilerError(compiler, err)
		}
	}

	c.rules, err = compiler.GetRules()
	if err != nil {
		err = fmt.Errorf("compiler get rules error: %w", err)
		return compilerError(compiler, err)
	}
	return nil
}

// CompileRulesFileOrDir compiles the YARA rules in the given directory or single file, and
// sets namespace of each file by cleaning file name(s).
func (c *Compiled) CompileFileOrDir(target ScanTarget, filenameNS bool, path string) error {
	if c.rules != nil {
		return ErrAlreadyCompiled
	}

	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return c.CompileDir(target, filenameNS, path)
	}
	if info.Mode().IsRegular() {
		return c.CompileFiles(target, filenameNS, path)
	}
	return fmt.Errorf("'%s' is not a directory or a regular file", path)
}

// CompileDir compiles the YARA rules in the given directory and
// sets namespace of each file by cleaning file name(s).
func (c *Compiled) CompileDir(target ScanTarget, filenameNS bool, dir string) error {
	if c.rules != nil {
		return ErrAlreadyCompiled
	}

	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close() // nolint errcheck

	names, err := f.Readdirnames(0)
	if err != nil {
		return err
	}

	paths := make([]string, 0, len(names))
	for _, name := range names {
		ext := filepath.Ext(name)
		if !strings.EqualFold(ext, ".yar") && !strings.EqualFold(ext, ".yara") {
			continue
		}
		path := filepath.Join(dir, name)
		paths = append(paths, path)
	}
	if len(paths) == 0 {
		return errors.New("no yara files")
	}
	return c.CompileFiles(target, filenameNS, paths...)
}

// CompileFiles compiles the YARA rules in the given file paths,
// sets namespace of each file by cleaning file name(s).
func (c *Compiled) CompileFiles(target ScanTarget, filenameNS bool, paths ...string) error {
	if c.rules != nil {
		return ErrAlreadyCompiled
	}

	compiler, err := yara.NewCompiler()
	if err != nil {
		return fmt.Errorf("compiler error: %w", err)
	}
	defer compiler.Destroy()

	parser := new(variables.Parser)
	files := make([]*os.File, 0, len(paths))

	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()

	var fallbackAllVars bool
	for _, path := range paths {
		var f *os.File
		f, err = os.Open(path)
		if err != nil {
			return err
		}

		if info, err := f.Stat(); err != nil {
			_ = f.Close()
			return err
		} else {
			if !info.Mode().IsRegular() {
				_ = f.Close()
				continue
			}
		}

		files = append(files, f)

		if err = parser.ParseFromReader(f); err != nil {
			return fmt.Errorf("variable parser error: %w", err)
		}
		_, _ = f.Seek(0, io.SeekStart)
		if len(parser.Includes()) > 0 {
			fallbackAllVars = true
		}
	}
	vars := parser.Variables()
	if fallbackAllVars {
		vars = variables.List()
	}

	if err := c.initVariables(target, vars); err != nil {
		return err
	}

	err = c.vars.DefineCompilerVariables(compiler)
	if err != nil {
		err = fmt.Errorf("compiler define variable error: %w", err)
		return compilerError(compiler, err)
	}

	c.rules, err = compileFiles(compiler, files, filenameNS)
	return err
}

func (c *Compiled) Variables() *variables.Variables {
	return c.vars
}

func (c *Compiled) Rules() *yara.Rules {
	return c.rules
}

func (c *Compiled) CreateScanner() error {
	s, err := yara.NewScanner(c.rules)
	if err != nil {
		return err
	}
	c.scanner = s
	return nil
}

func (c *Compiled) Scanner() *yara.Scanner {
	return c.scanner
}

func (c *Compiled) DefineScannerVariables(sctx variables.ScanContext) error {
	return c.vars.DefineScannerVariables(sctx, c.scanner)
}

func (c *Compiled) SetCallback(cb yara.ScanCallback) *Compiled {
	c.scanner.SetCallback(cb)
	return c
}

func (c *Compiled) ScanFileDescriptor(fd uintptr) error {
	return c.scanner.ScanFileDescriptor(fd)
}

func (c *Compiled) ScanFile(filename string) error {
	return c.scanner.ScanFile(filename)
}

func (c *Compiled) ScanProc(pid int) error {
	return c.scanner.ScanProc(pid)
}

func (c *Compiled) Destroy() {
	if c.scanner != nil {
		c.scanner.Destroy()
		c.scanner = nil
	}
	if c.rules != nil {
		c.rules.Destroy()
		c.rules = nil
	}
}

func compileFiles(compiler *yara.Compiler, files []*os.File, filenameNS bool) (*yara.Rules, error) {
	for _, file := range files {
		file := file

		var namespace string
		if filenameNS {
			namespace = filepath.Base(file.Name())
		}

		err := compiler.AddFile(file, namespace)
		if err != nil {
			err = fmt.Errorf("compiler add rule error: %w", err)
			return nil, compilerError(compiler, err)
		}
	}

	rules, err := compiler.GetRules()
	if err != nil {
		err = fmt.Errorf("compiler get rules error: %w", err)
		return nil, compilerError(compiler, err)
	}
	return rules, nil
}

func compilerError(c *yara.Compiler, err error) error {
	if c != nil && len(c.Errors) > 1 {
		err = fmt.Errorf("%w more: %s", err, mergeCompilerErrors(c.Errors[1:]))
	}
	return err
}

func mergeCompilerErrors(cm []yara.CompilerMessage) string {
	msgs := make([]string, 0, len(cm))
	for i, m := range cm {
		msgs = append(msgs, fmt.Sprintf("#%d '%s' '%s':%d", i+1, m.Text, filepath.Base(m.Filename), m.Line))
	}
	return strings.Join(msgs, " ; ")
}

func (c *Compiled) initVariables(target ScanTarget, vars []variables.VariableType) error {
	switch target {
	case ScanProcess:
		c.vars.InitProcessVariables(vars)
	case ScanFile:
		c.vars.InitFileVariables(vars)
	default:
		return errors.New("invalid scan target:" + strconv.Itoa(int(target)))
	}
	return nil
}
