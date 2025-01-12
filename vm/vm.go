// Package vm provides a VirtualMachine that executes compiled Risor code.
package vm

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/risor-io/risor/compiler"
	"github.com/risor-io/risor/importer"
	"github.com/risor-io/risor/limits"
	"github.com/risor-io/risor/object"
	"github.com/risor-io/risor/op"
)

const (
	MaxArgs       = 255
	MaxFrameDepth = 1024
	MaxStackDepth = 1024
	StopSignal    = -1
	MB            = 1024 * 1024
)

type VirtualMachine struct {
	ip           int // instruction pointer
	sp           int // stack pointer
	fp           int // frame pointer
	halt         int32
	stack        [MaxStackDepth]object.Object
	frames       [MaxFrameDepth]frame
	tmp          [MaxArgs]object.Object
	activeFrame  *frame
	activeCode   *code
	main         *compiler.Code
	importer     importer.Importer
	modules      map[string]*object.Module
	inputGlobals map[string]any
	globals      map[string]object.Object
	limits       limits.Limits
	loadedCode   map[*compiler.Code]*code
	running      bool
	concAllowed  bool
}

// Option is a configuration function for a Virtual Machine.
type Option func(*VirtualMachine)

// WithInstructionOffset sets the initial instruction offset.
func WithInstructionOffset(offset int) Option {
	return func(vm *VirtualMachine) {
		vm.ip = offset
	}
}

// WithImporter is used to supply an Importer to the Virtual Machine.
func WithImporter(importer importer.Importer) Option {
	return func(vm *VirtualMachine) {
		vm.importer = importer
	}
}

// WithLimits sets the limits for the Virtual Machine.
func WithLimits(limits limits.Limits) Option {
	return func(vm *VirtualMachine) {
		vm.limits = limits
	}
}

// WithGlobals provides global variables with the given names.
func WithGlobals(globals map[string]any) Option {
	return func(vm *VirtualMachine) {
		for name, value := range globals {
			vm.inputGlobals[name] = value
		}
	}
}

// WithConcurrency opts into allowing the spawning of Goroutines
func WithConcurrency() Option {
	return func(vm *VirtualMachine) {
		vm.concAllowed = true
	}
}

func defaultLimits() limits.Limits {
	return limits.New(limits.WithMaxBufferSize(100 * MB))
}

// Run the given code in a new Virtual Machine and return the result.
func Run(ctx context.Context, main *compiler.Code, options ...Option) (object.Object, error) {
	machine := New(main, options...)
	if err := machine.Run(ctx); err != nil {
		return nil, err
	}
	if result, exists := machine.TOS(); exists {
		return result, nil
	}
	return object.Nil, nil
}

// New creates a new Virtual Machine.
func New(main *compiler.Code, options ...Option) *VirtualMachine {
	vm := &VirtualMachine{
		sp:           -1,
		ip:           0,
		main:         main,
		modules:      map[string]*object.Module{},
		inputGlobals: map[string]any{},
		globals:      map[string]object.Object{},
		loadedCode:   map[*compiler.Code]*code{},
	}
	for _, opt := range options {
		opt(vm)
	}
	if vm.limits == nil {
		vm.limits = defaultLimits()
	}
	return vm
}

func (vm *VirtualMachine) Run(ctx context.Context) (err error) {
	// Translate any panic into an error so the caller has a good guarantee
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()

	// Halt execution when the context is cancelled
	if doneChan := ctx.Done(); doneChan != nil {
		go func() {
			<-doneChan
			atomic.StoreInt32(&vm.halt, 1)
		}()
	}

	// Convert globals
	vm.globals, err = object.AsObjects(vm.inputGlobals)
	if err != nil {
		return err
	}

	// Add any globals that are modules cache
	for name, value := range vm.globals {
		if module, ok := value.(*object.Module); ok {
			vm.modules[name] = module
		}
	}

	// Keep `running` flag up-to-date
	vm.running = true
	defer func() { vm.running = false }()

	// Load the code for any functions that are constants in this main code.
	// Doing this in advance means the set of loaded code is constant once
	// execution has begun.
	c := vm.main.ConstantsCount()
	for i := 0; i < c; i++ {
		if fn, ok := vm.main.Constant(i).(*compiler.Function); ok {
			vm.load(fn.Code())
		}
	}

	// Activate the "main" entrypoint code in frame 0 and then run it
	var code *code
	if len(vm.loadedCode) > 0 {
		code = vm.reload(vm.main)
	} else {
		code = vm.load(vm.main)
	}
	vm.activateCode(0, vm.ip, code)
	ctx = object.WithCallFunc(ctx, vm.callFunction)
	ctx = limits.WithLimits(ctx, vm.limits)
	if vm.concAllowed {
		ctx = object.WithSpawnFunc(ctx, vm.spawnFunction)
	}
	err = vm.eval(ctx)
	return
}

// Get a global variable by name as a Risor Object. Returns an error if the
// variable can't be found.
func (vm *VirtualMachine) Get(name string) (object.Object, error) {
	code := vm.activeCode
	if code == nil {
		return nil, errors.New("no active code")
	}
	for i := 0; i < code.GlobalsCount(); i++ {
		if g := code.Global(i); g.Name() == name {
			return code.Globals[g.Index()], nil
		}
	}
	return nil, fmt.Errorf("global with name %q not found", name)
}

// GlobalNames returns the names of all global variables in the active code.
func (vm *VirtualMachine) GlobalNames() []string {
	if vm.activeCode == nil {
		return nil
	}
	count := vm.activeCode.GlobalsCount()
	names := make([]string, 0, count)
	for i := 0; i < count; i++ {
		names = append(names, vm.activeCode.Global(i).Name())
	}
	return names
}

// Evaluate the active code. The caller must initialize the following variables
// before calling this function:
//   - vm.ip - instruction pointer within the active code
//   - vm.fp - frame pointer with the active code already set
//   - vm.activeCode - the code object to execute
//   - vm.activeFrame - the active call frame to use
//
// Assuming this function returns without error, the result of the evaluation
// will be on the top of the stack.
func (vm *VirtualMachine) eval(ctx context.Context) error {
	// Run to the end of the active code
	for vm.ip < len(vm.activeCode.Instructions) {

		if atomic.LoadInt32(&vm.halt) == 1 {
			return ctx.Err()
		}

		// The current instruction opcode
		opcode := vm.activeCode.Instructions[vm.ip]

		// fmt.Println("ip", vm.ip, op.GetInfo(opcode).Name, "sp", vm.sp)

		// Advance the instruction pointer to the next instruction. Note that
		// this is done before we actually execute the current instruction, so
		// relative jump instructions will need to take this into account.
		vm.ip++

		// Dispatch the instruction
		switch opcode {
		case op.Nop:
		case op.LoadAttr:
			obj := vm.pop()
			name := vm.activeCode.Names[vm.fetch()]
			value, found := obj.GetAttr(name)
			if !found {
				return fmt.Errorf("exec error: attribute %q not found on %s object",
					name, obj.Type())
			}
			switch value := value.(type) {
			case object.AttrResolver:
				attr, err := value.ResolveAttr(ctx, name)
				if err != nil {
					return err
				}
				vm.push(attr)
			default:
				vm.push(value)
			}
		case op.LoadConst:
			vm.push(vm.activeCode.Constants[vm.fetch()])
		case op.LoadFast:
			vm.push(vm.activeFrame.Locals()[vm.fetch()])
		case op.LoadGlobal:
			vm.push(vm.activeCode.Globals[vm.fetch()])
		case op.LoadFree:
			idx := vm.fetch()
			freeVars := vm.activeFrame.fn.FreeVars()
			obj := freeVars[idx].Value()
			vm.push(obj)
		case op.StoreFast:
			idx := vm.fetch()
			obj := vm.pop()
			vm.activeFrame.Locals()[idx] = obj
		case op.StoreGlobal:
			vm.activeCode.Globals[vm.fetch()] = vm.pop()
		case op.StoreFree:
			idx := vm.fetch()
			obj := vm.pop()
			freeVars := vm.activeFrame.fn.FreeVars()
			freeVars[idx].Set(obj)
		case op.StoreAttr:
			idx := vm.fetch()
			obj := vm.pop()
			value := vm.pop()
			name := vm.activeCode.Names[idx]
			if err := obj.SetAttr(name, value); err != nil {
				return err
			}
		case op.LoadClosure:
			constIndex := vm.fetch()
			freeCount := vm.fetch()
			free := make([]*object.Cell, freeCount)
			for i := uint16(0); i < freeCount; i++ {
				obj := vm.pop()
				switch obj := obj.(type) {
				case *object.Cell:
					free[freeCount-i-1] = obj
				default:
					return errors.New("exec error: expected cell")
				}
			}
			fn := vm.activeCode.Constants[constIndex].(*object.Function)
			vm.push(object.NewClosure(fn, free))
		case op.MakeCell:
			symbolIndex := vm.fetch()
			framesBack := int(vm.fetch())
			frameIndex := vm.fp - framesBack
			if frameIndex < 0 {
				return fmt.Errorf("exec error: no frame at depth %d", framesBack)
			}
			frame := &vm.frames[frameIndex]
			locals := frame.CaptureLocals()
			vm.push(object.NewCell(&locals[symbolIndex]))
		case op.Nil:
			vm.push(object.Nil)
		case op.True:
			vm.push(object.True)
		case op.False:
			vm.push(object.False)
		case op.CompareOp:
			opType := op.CompareOpType(vm.fetch())
			b := vm.pop()
			a := vm.pop()
			vm.push(object.Compare(opType, a, b))
		case op.BinaryOp:
			opType := op.BinaryOpType(vm.fetch())
			b := vm.pop()
			a := vm.pop()
			vm.push(object.BinaryOp(opType, a, b))
		case op.Call:
			argc := int(vm.fetch())
			for argIndex := argc - 1; argIndex >= 0; argIndex-- {
				vm.tmp[argIndex] = vm.pop()
			}
			obj := vm.pop()
			if err := vm.call(ctx, obj, vm.tmp[:argc]); err != nil {
				return err
			}
		case op.Partial:
			argc := int(vm.fetch())
			args := make([]object.Object, argc)
			for i := argc - 1; i >= 0; i-- {
				args[i] = vm.pop()
			}
			obj := vm.pop()
			partial := object.NewPartial(obj, args)
			vm.push(partial)
		case op.ReturnValue:
			activeFrame := vm.activeFrame
			returnAddr := activeFrame.returnAddr
			returnSp := activeFrame.returnSp
			returnFp := vm.fp - 1
			vm.resumeFrame(returnFp, returnAddr, returnSp)
			if returnAddr == StopSignal {
				// If StopSignal is found as the return address, it means the
				// current eval call should stop.
				return nil
			}
		case op.PopJumpForwardIfTrue:
			tos := vm.pop()
			delta := int(vm.fetch()) - 2
			if tos.IsTruthy() {
				vm.ip += delta
			}
		case op.PopJumpForwardIfFalse:
			tos := vm.pop()
			delta := int(vm.fetch()) - 2
			if !tos.IsTruthy() {
				vm.ip += delta
			}
		case op.PopJumpBackwardIfTrue:
			tos := vm.pop()
			delta := int(vm.fetch()) - 2
			if tos.IsTruthy() {
				vm.ip -= delta
			}
		case op.PopJumpBackwardIfFalse:
			tos := vm.pop()
			delta := int(vm.fetch()) - 2
			if !tos.IsTruthy() {
				vm.ip -= delta
			}
		case op.JumpForward:
			base := vm.ip - 1
			delta := int(vm.fetch())
			vm.ip = base + delta
		case op.JumpBackward:
			base := vm.ip - 1
			delta := int(vm.fetch())
			vm.ip = base - delta
		case op.BuildList:
			count := vm.fetch()
			items := make([]object.Object, count)
			for i := uint16(0); i < count; i++ {
				items[count-1-i] = vm.pop()
			}
			vm.push(object.NewList(items))
		case op.BuildMap:
			count := vm.fetch()
			items := make(map[string]object.Object, count)
			for i := uint16(0); i < count; i++ {
				v := vm.pop()
				k := vm.pop()
				items[k.(*object.String).Value()] = v
			}
			vm.push(object.NewMap(items))
		case op.BuildSet:
			count := vm.fetch()
			items := make([]object.Object, count)
			for i := uint16(0); i < count; i++ {
				items[i] = vm.pop()
			}
			vm.push(object.NewSet(items))
		case op.BinarySubscr:
			idx := vm.pop()
			lhs := vm.pop()
			container, ok := lhs.(object.Container)
			if !ok {
				return fmt.Errorf("type error: object is not a container (got %s)", lhs.Type())
			}
			result, err := container.GetItem(idx)
			if err != nil {
				return err.Value()
			}
			vm.push(result)
		case op.StoreSubscr:
			idx := vm.pop()
			lhs := vm.pop()
			rhs := vm.pop()
			container, ok := lhs.(object.Container)
			if !ok {
				return fmt.Errorf("type error: object is not a container (got %s)", lhs.Type())
			}
			if err := container.SetItem(idx, rhs); err != nil {
				return err.Value()
			}
		case op.UnaryNegative:
			obj := vm.pop()
			switch obj := obj.(type) {
			case *object.Int:
				vm.push(object.NewInt(-obj.Value()))
			case *object.Float:
				vm.push(object.NewFloat(-obj.Value()))
			default:
				return fmt.Errorf("type error: object is not a number (got %s)", obj.Type())
			}
		case op.UnaryNot:
			obj := vm.pop()
			if obj.IsTruthy() {
				vm.push(object.False)
			} else {
				vm.push(object.True)
			}
		case op.ContainsOp:
			obj := vm.pop()
			containerObj := vm.pop()
			invert := vm.fetch() == 1
			if container, ok := containerObj.(object.Container); ok {
				value := container.Contains(obj)
				if invert {
					value = object.Not(value)
				}
				vm.push(value)
			} else {
				return fmt.Errorf("type error: object is not a container (got %s)",
					containerObj.Type())
			}
		case op.Swap:
			vm.swap(int(vm.fetch()))
		case op.BuildString:
			count := vm.fetch()
			items := make([]string, count)
			for i := uint16(0); i < count; i++ {
				dst := count - 1 - i
				obj := vm.pop()
				switch obj := obj.(type) {
				case *object.Error:
					return obj.Value() // TODO: review this
				case *object.String:
					items[dst] = obj.Value()
				default:
					items[dst] = obj.Inspect()
				}
			}
			vm.push(object.NewString(strings.Join(items, "")))
		case op.Range:
			iterableObj := vm.pop()
			iterable, ok := iterableObj.(object.Iterable)
			if !ok {
				return fmt.Errorf("type error: object is not an iterable (got %s)",
					iterableObj.Type())
			}
			vm.push(iterable.Iter())
		case op.Slice:
			start := vm.pop()
			stop := vm.pop()
			containerObj := vm.pop()
			container, ok := containerObj.(object.Container)
			if !ok {
				return fmt.Errorf("type error: object is not a container (got %s)",
					containerObj.Type())
			}
			slice := object.Slice{Start: start, Stop: stop}
			result, err := container.GetSlice(slice)
			if err != nil {
				return err.Value()
			}
			vm.push(result)
		case op.Length:
			containerObj := vm.pop()
			container, ok := containerObj.(object.Container)
			if !ok {
				return fmt.Errorf("type error: object is not a container (got %s)",
					containerObj.Type())
			}
			vm.push(container.Len())
		case op.Copy:
			offset := vm.fetch()
			vm.push(vm.stack[vm.sp-int(offset)])
		case op.Import:
			name, ok := vm.pop().(*object.String)
			if !ok {
				return fmt.Errorf("type error: object is not a string (got %s)", name.Type())
			}
			module, err := vm.loadModule(ctx, name.Value())
			if err != nil {
				return err
			}
			vm.push(module)
		case op.FromImport:
			parentLen := vm.fetch()
			importsCount := vm.fetch()
			if importsCount > 255 {
				return fmt.Errorf("exec error: invalid imports count: %d", importsCount)
			}
			var names []string
			for i := uint16(0); i < importsCount; i++ {
				name, ok := vm.pop().(*object.String)
				if !ok {
					return fmt.Errorf("type error: object is not a string (got %s)", name.Type())
				}
				names = append(names, name.Value())
			}
			from := make([]string, parentLen)
			for i := int(parentLen - 1); i >= 0; i-- {
				val, ok := vm.pop().(*object.String)
				if !ok {
					return fmt.Errorf("type error: object is not a string (got %s)", val.Type())
				}
				from[i] = val.Value()
			}
			for _, name := range names {
				// check if the name matches a module
				module, err := vm.loadModule(ctx, filepath.Join(filepath.Join(from...), name))
				if err == nil {
					vm.push(module)
				} else {
					// otherwise, the name is a symbol inside a module
					module, err := vm.loadModule(ctx, filepath.Join(from...))
					if err != nil {
						return err
					}
					attr, found := module.GetAttr(name)
					if !found {
						return fmt.Errorf("import error: cannot import name %q from %q",
							name, module.Name())
					}
					vm.push(attr)
				}
			}
		case op.PopTop:
			vm.pop()
		case op.Unpack:
			containerObj := vm.pop()
			nameCount := int64(vm.fetch())
			container, ok := containerObj.(object.Container)
			if !ok {
				return fmt.Errorf("type error: object is not a container (got %s)",
					containerObj.Type())
			}
			containerSize := container.Len().Value()
			if containerSize != nameCount {
				return fmt.Errorf("exec error: unpack count mismatch: %d != %d",
					containerSize, nameCount)
			}
			iter := container.Iter()
			for {
				val, ok := iter.Next(ctx)
				if !ok {
					break
				}
				vm.push(val)
			}
		case op.GetIter:
			obj := vm.pop()
			switch obj := obj.(type) {
			case object.Iterable:
				vm.push(obj.Iter())
			case object.Iterator:
				vm.push(obj)
			default:
				return fmt.Errorf("type error: object is not iterable (got %s)", obj.Type())
			}
		case op.ForIter:
			base := vm.ip - 1
			jumpAmount := vm.fetch()
			nameCount := vm.fetch()
			iter := vm.pop().(object.Iterator)
			if _, ok := iter.Next(ctx); !ok {
				vm.ip = base + int(jumpAmount)
			} else {
				obj, _ := iter.Entry()
				vm.push(iter)
				if nameCount == 1 {
					vm.push(obj.Key())
				} else if nameCount == 2 {
					vm.push(obj.Value())
					vm.push(obj.Key())
				} else if nameCount != 0 {
					return fmt.Errorf("exec error: invalid iteration")
				}
			}
		case op.Go:
			obj := vm.pop()
			partial, ok := obj.(*object.Partial)
			if !ok {
				return fmt.Errorf("type error: object is not a partial (got %s)", obj.Type())
			}
			if _, err := object.Spawn(ctx, partial.Function(), partial.Args()); err != nil {
				return err
			}
		case op.Defer:
			obj := vm.pop()
			partial, ok := obj.(*object.Partial)
			if !ok {
				return fmt.Errorf("type error: object is not a partial (got %s)", obj.Type())
			}
			vm.activeFrame.Defer(partial)
		case op.Send:
			value := vm.pop()
			channel := vm.pop()
			ch, ok := channel.(*object.Chan)
			if !ok {
				return fmt.Errorf("type error: object is not a channel (got %s)", channel.Type())
			}
			if err := ch.Send(ctx, value); err != nil {
				return err
			}
		case op.Receive:
			channel := vm.pop()
			ch, ok := channel.(*object.Chan)
			if !ok {
				return fmt.Errorf("type error: object is not a channel (got %s)", channel.Type())
			}
			value, err := ch.Receive(ctx)
			if err != nil {
				return err
			}
			vm.push(value)
		case op.Halt:
			return nil
		default:
			return fmt.Errorf("exec error: unknown opcode: %d", opcode)
		}
	}
	return nil
}

func (vm *VirtualMachine) loadModule(ctx context.Context, name string) (*object.Module, error) {
	if module, ok := vm.modules[name]; ok {
		return module, nil
	}
	if vm.importer == nil {
		return nil, fmt.Errorf("exec error: imports are disabled")
	}
	// Load and compile the module code
	module, err := vm.importer.Import(ctx, name)
	if err != nil {
		return nil, err
	}
	// Activate a new frame to evaluate the module code
	baseFP := vm.fp
	baseIP := vm.ip
	baseSP := vm.sp
	code := vm.load(module.Code())
	vm.activateCode(vm.fp+1, 0, code)
	// Restore the previous frame when done
	defer vm.resumeFrame(baseFP, baseIP, baseSP)
	// Evaluate the module code
	if err := vm.eval(ctx); err != nil {
		return nil, err
	}
	module.UseGlobals(code.Globals)
	// Cache the module
	vm.modules[name] = module
	return module, nil
}

// GetIP returns the current instruction pointer.
func (vm *VirtualMachine) GetIP() int {
	return vm.ip
}

// SetIP sets the current instruction pointer.
func (vm *VirtualMachine) SetIP(value int) {
	vm.ip = value
}

// TOS returns the top-of-stack object if there is one, without modifying the
// stack. The boolean return value indicates whether there was a TOS.
func (vm *VirtualMachine) TOS() (object.Object, bool) {
	if vm.sp >= 0 {
		return vm.stack[vm.sp], true
	}
	return nil, false
}

func (vm *VirtualMachine) pop() object.Object {
	obj := vm.stack[vm.sp]
	vm.stack[vm.sp] = nil
	vm.sp--
	return obj
}

func (vm *VirtualMachine) push(obj object.Object) {
	vm.sp++
	vm.stack[vm.sp] = obj
}

func (vm *VirtualMachine) swap(pos int) {
	otherIndex := vm.sp - pos
	tos := vm.stack[vm.sp]
	other := vm.stack[otherIndex]
	vm.stack[otherIndex] = tos
	vm.stack[vm.sp] = other
}

func (vm *VirtualMachine) fetch() uint16 {
	ip := vm.ip
	vm.ip++
	return uint16(vm.activeCode.Instructions[ip])
}

// Call a function with the supplied arguments. If isolation between VMs is
// important to you, do not provide a function here that was obtained from
// another VM, since it could be a closure over variables in that VM. This
// method should only be called after this VM stops running. Otherwise, an
// error is returned.
func (vm *VirtualMachine) Call(ctx context.Context, fn *object.Function, args []object.Object) (object.Object, error) {
	if vm.running {
		return nil, errors.New("exec error: cannot call function while the vm is running")
	}
	return vm.callFunction(ctx, fn, args)
}

// Calls a compiled function with the given arguments. This is used internally
// when a Risor object calls a function, e.g. [1, 2, 3].map(func(x) { x + 1 }).
func (vm *VirtualMachine) callFunction(ctx context.Context, fn *object.Function, args []object.Object) (result object.Object, resultErr error) {
	baseFP := vm.fp
	baseIP := vm.ip
	baseSP := vm.sp

	// Check that the argument count is appropriate
	paramsCount := len(fn.Parameters())
	argc := len(args)
	if err := checkCallArgs(fn, argc); err != nil {
		return nil, err
	}

	// Restore the previous frame when done
	defer vm.resumeFrame(baseFP, baseIP, baseSP)

	// Assemble frame local variables in vm.tmp. The local variable order is:
	// 1. Function parameters
	// 2. Function name (if the function is named)
	copy(vm.tmp[:argc], args)
	if argc < paramsCount {
		defaults := fn.Defaults()
		for i := argc; i < len(defaults); i++ {
			vm.tmp[i] = defaults[i]
		}
		argc = paramsCount
	}
	code := fn.Code()
	if code.IsNamed() {
		vm.tmp[paramsCount] = fn
		argc++
	}

	// Activate a frame for the function call
	vm.activateFunction(vm.fp+1, 0, fn, vm.tmp[:argc])

	// Setting StopSignal as the return address will cause the eval function to
	// stop execution when it reaches the end of the active code.
	vm.activeFrame.returnAddr = StopSignal

	callFrame := vm.activeFrame

	// Fire any defers
	defer func() {
		for _, partial := range callFrame.defers {
			if err := vm.call(ctx, partial.Function(), partial.Args()); err != nil {
				result = nil
				resultErr = err
			}
		}
	}()

	// Evaluate the function code then return the result from TOS
	if err := vm.eval(ctx); err != nil {
		return nil, err
	}
	return vm.pop(), nil
}

func (vm *VirtualMachine) call(ctx context.Context, fn object.Object, args []object.Object) error {
	argc := len(args)
	switch fn := fn.(type) {
	case *object.Function:
		result, err := vm.callFunction(ctx, fn, args)
		if err != nil {
			return err
		}
		vm.push(result)
	case *object.Partial:
		// Combine the current arguments with the partial's arguments
		expandedCount := argc + len(fn.Args())
		if expandedCount > MaxArgs {
			return fmt.Errorf("exec error: max arguments limit of %d exceeded (got %d)", MaxArgs, expandedCount)
		}
		copy(vm.tmp[:argc], args)
		copy(vm.tmp[argc:], fn.Args())
		return vm.call(ctx, fn.Function(), vm.tmp[:expandedCount])
	case object.Callable:
		result := fn.Call(ctx, args...)
		if err, ok := result.(*object.Error); ok {
			return err.Value()
		}
		vm.push(result)
	default:
		return fmt.Errorf("type error: object is not callable (got %s)", fn.Type())
	}
	return nil
}

// Wrap the *compiler.Code in a *code object to make it usable by the VM.
func (vm *VirtualMachine) load(cc *compiler.Code) *code {
	if code, ok := vm.loadedCode[cc]; ok {
		return code
	}
	// Loading is slightly different if this is the "root" (entrypoint) code
	// vs. a child of that. The root code owns the globals array, while the
	// children will reuse the globals from the root.
	rootCompiled := cc.Root()
	if rootCompiled == cc {
		c := loadRootCode(cc, vm.globals)
		vm.loadedCode[cc] = c
		return c
	}
	rootLoaded := vm.load(rootCompiled)
	c := loadChildCode(rootLoaded, cc)
	vm.loadedCode[cc] = c
	return c
}

// Reloads the main code while preserving global variables.
func (vm *VirtualMachine) reload(main *compiler.Code) *code {
	oldWrappedMain, ok := vm.loadedCode[main]
	if !ok {
		panic("main code not loaded")
	}
	vm.loadedCode = map[*compiler.Code]*code{}
	newWrappedMain := vm.load(main)
	copy(newWrappedMain.Globals, oldWrappedMain.Globals)
	return newWrappedMain
}

// Resume the frame at the given frame pointer, restoring the given IP and SP.
func (vm *VirtualMachine) resumeFrame(fp, ip, sp int) *frame {
	// The return value of the previous frame is on the top of the stack
	var frameResult object.Object = nil
	if vm.sp > sp {
		frameResult = vm.pop()
	}
	// Remove any items left on the stack by the previous frame
	for i := vm.sp; i > sp; i-- {
		vm.stack[i] = nil
	}
	vm.sp = sp
	// Push the frame result back onto the stack
	if frameResult != nil {
		vm.push(frameResult)
	}
	// Activate the resumed frame
	vm.fp = fp
	vm.ip = ip
	vm.activeFrame = &vm.frames[fp]
	vm.activeCode = vm.activeFrame.code
	return vm.activeFrame
}

// Activate a frame with the given code. This is typically used to begin
// running the entrypoint for a module or script.
func (vm *VirtualMachine) activateCode(fp, ip int, code *code) *frame {
	vm.fp = fp
	vm.ip = ip
	vm.activeFrame = &vm.frames[fp]
	vm.activeFrame.ActivateCode(code)
	vm.activeCode = code
	return vm.activeFrame
}

// Activate a frame with the given function, to implement a function call.
func (vm *VirtualMachine) activateFunction(fp, ip int, fn *object.Function, locals []object.Object) *frame {
	code := vm.load(fn.Code())
	returnAddr := vm.ip
	returnSp := vm.sp
	vm.fp = fp
	vm.ip = ip
	vm.activeFrame = &vm.frames[fp]
	vm.activeFrame.ActivateFunction(fn, code, returnAddr, returnSp, locals)
	vm.activeCode = code
	return vm.activeFrame
}

// Clone the Virtual Machine. The returned clone has its own independent stack,
// ip, fp, sp, and loaded code, which makes its execution independent of the
// original Virtual Machine. However the clone shares the same modules and
// globals and any objects directly or indirectly referenced by those.
//
// Consequently, the caller and/or the user code is responsible for thread
// safety when using modules and global variables, as concurrently executing
// cloned VMs can modify the same objects and hence have standard thread safety
// issues.
//
// The returned clone has an empty stack and frame stack, which makes this
// most useful for cloning a VM then using vm.Call() to call a function, rather
// than calling vm.Run() on the clone, which would start execution at the
// beginning of the original entrypoint.
//
// Do not use this if you want a strict guarantee of isolation between VMs.
//
// The VM limits are not currently copied from the original because limits
// implementations are not currently thread safe. Consequently it's not safe to
// use Clone when limits are required.
//
// Another current limitation that may be addressed in the future is that
// cloned VMs do not have the ability to import additional modules.
func (vm *VirtualMachine) Clone() (*VirtualMachine, error) {
	// Capture a snapshot of the loaded modules. This is needed for threadsafe
	// access to the modules map, since the parent VM can continue to modify it.
	modules := make(map[string]*object.Module, len(vm.modules))
	for name, module := range vm.modules {
		modules[name] = module
	}
	// Capture a snapshot of the loaded code for thread safety reasons
	loadedCode := make(map[*compiler.Code]*code, len(vm.loadedCode))
	for cc, c := range vm.loadedCode {
		loadedCode[cc] = c
	}
	clone := &VirtualMachine{
		sp:           -1,
		ip:           0,
		fp:           0,
		limits:       nil,
		importer:     nil,
		running:      false,
		main:         vm.main,
		inputGlobals: vm.inputGlobals,
		globals:      vm.globals,
		loadedCode:   loadedCode,
		modules:      modules,
	}
	clone.activateCode(0, vm.ip, clone.load(clone.main))
	return clone, nil
}

func (vm *VirtualMachine) spawnFunction(ctx context.Context, fn object.Callable, args []object.Object) (*object.Thread, error) {
	clone, err := vm.Clone()
	if err != nil {
		return nil, err
	}
	// Create a ctx with the call and spawn functions set to the clone's methods!
	ctx = object.WithCallFunc(ctx, clone.callFunction)
	ctx = object.WithSpawnFunc(ctx, clone.spawnFunction)
	ctx = limits.WithLimits(ctx, nil)
	// NewThread runs a goroutine
	return object.NewThread(ctx, fn, args), nil
}

func checkCallArgs(fn *object.Function, argc int) error {
	// Number of parameters in the function signature
	paramsCount := len(fn.Parameters())

	// Number of required args when the function is called (those without defaults)
	requiredArgsCount := fn.RequiredArgsCount()

	// Check if too many or too few arguments were passed
	if argc > paramsCount || argc < requiredArgsCount {
		switch paramsCount {
		case 0:
			return fmt.Errorf("type error: function takes no arguments (%d given)", argc)
		case 1:
			return fmt.Errorf("type error: function takes 1 argument (%d given)", argc)
		default:
			return fmt.Errorf("type error: function takes %d arguments (%d given)", paramsCount, argc)
		}
	}
	return nil
}
