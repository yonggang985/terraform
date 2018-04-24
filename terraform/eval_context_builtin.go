package terraform

import (
	"context"
	"fmt"
	"log"
	"sync"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/terraform/config/configschema"
	"github.com/hashicorp/terraform/tfdiags"

	"github.com/hashicorp/terraform/addrs"
	"github.com/zclconf/go-cty/cty"
)

// BuiltinEvalContext is an EvalContext implementation that is used by
// Terraform by default.
type BuiltinEvalContext struct {
	// StopContext is the context used to track whether we're complete
	StopContext context.Context

	// PathValue is the Path that this context is operating within.
	PathValue addrs.ModuleInstance

	// Evaluator is used for evaluating expressions within the scope of this
	// eval context.
	Evaluator *Evaluator

	ChildModuleCallArgs  map[string]map[string]cty.Value
	ChildModuleCallsLock *sync.Mutex

	Components          contextComponentFactory
	Hooks               []Hook
	InputValue          UIInput
	ProviderCache       map[string]ResourceProvider
	ProviderSchemas     map[string]*ProviderSchema
	ProviderInputConfig map[string]map[string]cty.Value
	ProviderLock        *sync.Mutex
	ProvisionerCache    map[string]ResourceProvisioner
	ProvisionerLock     *sync.Mutex
	DiffValue           *Diff
	DiffLock            *sync.RWMutex
	StateValue          *State
	StateLock           *sync.RWMutex

	once sync.Once
}

// BuiltinEvalContext implements EvalContext
var _ EvalContext = (*BuiltinEvalContext)(nil)

func (ctx *BuiltinEvalContext) Stopped() <-chan struct{} {
	// This can happen during tests. During tests, we just block forever.
	if ctx.StopContext == nil {
		return nil
	}

	return ctx.StopContext.Done()
}

func (ctx *BuiltinEvalContext) Hook(fn func(Hook) (HookAction, error)) error {
	for _, h := range ctx.Hooks {
		action, err := fn(h)
		if err != nil {
			return err
		}

		switch action {
		case HookActionContinue:
			continue
		case HookActionHalt:
			// Return an early exit error to trigger an early exit
			log.Printf("[WARN] Early exit triggered by hook: %T", h)
			return EvalEarlyExitError{}
		}
	}

	return nil
}

func (ctx *BuiltinEvalContext) Input() UIInput {
	return ctx.InputValue
}

func (ctx *BuiltinEvalContext) InitProvider(typeName, name string) (ResourceProvider, error) {
	ctx.once.Do(ctx.init)

	// If we already initialized, it is an error
	if p := ctx.Provider(name); p != nil {
		return nil, fmt.Errorf("Provider '%s' already initialized", name)
	}

	// Warning: make sure to acquire these locks AFTER the call to Provider
	// above, since it also acquires locks.
	ctx.ProviderLock.Lock()
	defer ctx.ProviderLock.Unlock()

	p, err := ctx.Components.ResourceProvider(typeName, name)
	if err != nil {
		return nil, err
	}

	ctx.ProviderCache[name] = p

	// Also fetch and cache the provider's schema.
	// FIXME: This is using a non-ideal provider API that requires us to
	// request specific resource types, but we actually just want _all_ the
	// resource types, so we'll list these first. Once the provider API is
	// updated we'll get enough data to populate this whole structure in
	// a single call.
	resourceTypes := p.Resources()
	dataSources := p.DataSources()
	resourceTypeNames := make([]string, len(resourceTypes))
	for i, t := range resourceTypes {
		resourceTypeNames[i] = t.Name
	}
	dataSourceNames := make([]string, len(dataSources))
	for i, t := range dataSources {
		dataSourceNames[i] = t.Name
	}
	schema, err := p.GetSchema(&ProviderSchemaRequest{
		DataSources:   dataSourceNames,
		ResourceTypes: resourceTypeNames,
	})
	if err != nil {
		return nil, fmt.Errorf("error fetching schema for %s: %s", name, err)
	}
	if ctx.ProviderSchemas == nil {
		ctx.ProviderSchemas = make(map[string]*ProviderSchema)
	}
	ctx.ProviderSchemas[name] = schema

	return p, nil
}

func (ctx *BuiltinEvalContext) Provider(n string) ResourceProvider {
	ctx.once.Do(ctx.init)

	ctx.ProviderLock.Lock()
	defer ctx.ProviderLock.Unlock()

	return ctx.ProviderCache[n]
}

func (ctx *BuiltinEvalContext) ProviderSchema(n string) *ProviderSchema {
	ctx.once.Do(ctx.init)

	ctx.ProviderLock.Lock()
	defer ctx.ProviderLock.Unlock()

	return ctx.ProviderSchemas[n]
}

func (ctx *BuiltinEvalContext) CloseProvider(n string) error {
	ctx.once.Do(ctx.init)

	ctx.ProviderLock.Lock()
	defer ctx.ProviderLock.Unlock()

	var provider interface{}
	provider = ctx.ProviderCache[n]
	if provider != nil {
		if p, ok := provider.(ResourceProviderCloser); ok {
			delete(ctx.ProviderCache, n)
			return p.Close()
		}
	}

	return nil
}

func (ctx *BuiltinEvalContext) ConfigureProvider(n string, cfg cty.Value) tfdiags.Diagnostics {
	var diags tfdiags.Diagnostics
	p := ctx.Provider(n)
	if p == nil {
		diags = diags.Append(fmt.Errorf("Provider '%s' not initialized", n))
		return diags
	}
	// FIXME: The provider API isn't yet updated to take a cty.Value directly.
	rc := NewResourceConfigShimmed(cfg, ctx.ProviderSchema(n).Provider)
	err := p.Configure(rc)
	if err != nil {
		diags = diags.Append(err)
	}
	return diags
}

func (ctx *BuiltinEvalContext) ProviderInput(pc addrs.ProviderConfig) map[string]cty.Value {
	ctx.ProviderLock.Lock()
	defer ctx.ProviderLock.Unlock()

	// Go up the module tree, looking for input results for the given provider
	// configuration.
	path := ctx.Path()
	for i := len(path); i >= 0; i-- {
		k := pc.Absolute(path[:i]).String()
		if v, ok := ctx.ProviderInputConfig[k]; ok {
			return v
		}
	}

	return nil
}

func (ctx *BuiltinEvalContext) SetProviderInput(pc addrs.ProviderConfig, c map[string]cty.Value) {
	absProvider := pc.Absolute(ctx.Path())

	// Save the configuration
	ctx.ProviderLock.Lock()
	ctx.ProviderInputConfig[absProvider.String()] = c
	ctx.ProviderLock.Unlock()
}

func (ctx *BuiltinEvalContext) InitProvisioner(
	n string) (ResourceProvisioner, error) {
	ctx.once.Do(ctx.init)

	// If we already initialized, it is an error
	if p := ctx.Provisioner(n); p != nil {
		return nil, fmt.Errorf("Provisioner '%s' already initialized", n)
	}

	// Warning: make sure to acquire these locks AFTER the call to Provisioner
	// above, since it also acquires locks.
	ctx.ProvisionerLock.Lock()
	defer ctx.ProvisionerLock.Unlock()

	key := PathObjectCacheKey(ctx.Path(), n)

	p, err := ctx.Components.ResourceProvisioner(n, key)
	if err != nil {
		return nil, err
	}

	ctx.ProvisionerCache[key] = p
	return p, nil
}

func (ctx *BuiltinEvalContext) Provisioner(n string) ResourceProvisioner {
	ctx.once.Do(ctx.init)

	ctx.ProvisionerLock.Lock()
	defer ctx.ProvisionerLock.Unlock()

	key := PathObjectCacheKey(ctx.Path(), n)
	return ctx.ProvisionerCache[key]
}

func (ctx *BuiltinEvalContext) CloseProvisioner(n string) error {
	ctx.once.Do(ctx.init)

	ctx.ProvisionerLock.Lock()
	defer ctx.ProvisionerLock.Unlock()

	key := PathObjectCacheKey(ctx.Path(), n)

	var prov interface{}
	prov = ctx.ProvisionerCache[key]
	if prov != nil {
		if p, ok := prov.(ResourceProvisionerCloser); ok {
			delete(ctx.ProvisionerCache, key)
			return p.Close()
		}
	}

	return nil
}

func (ctx *BuiltinEvalContext) EvaluateBlock(body hcl.Body, schema *configschema.Block, current *Resource) (cty.Value, hcl.Body, tfdiags.Diagnostics) {
	var diags tfdiags.Diagnostics
	scope := ctx.Evaluator.Scope(ctx.PathValue, current)
	body, evalDiags := scope.ExpandBlock(body, schema)
	diags = diags.Append(evalDiags)
	val, evalDiags := scope.EvalBlock(body, schema)
	diags = diags.Append(evalDiags)
	return val, body, diags
}

func (ctx *BuiltinEvalContext) EvaluateExpr(expr hcl.Expression, wantType cty.Type, current *Resource) (cty.Value, tfdiags.Diagnostics) {
	scope := ctx.Evaluator.Scope(ctx.PathValue, current)
	return scope.EvalExpr(expr, wantType)
}

func (ctx *BuiltinEvalContext) Path() addrs.ModuleInstance {
	return ctx.PathValue
}

func (ctx *BuiltinEvalContext) SetModuleCallArguments(n addrs.ModuleInstanceStep, vals map[string]cty.Value) {
	ctx.ChildModuleCallsLock.Lock()
	defer ctx.ChildModuleCallsLock.Unlock()

	childPath := ctx.Path().Child(n.Name, n.InstanceKey)
	key := childPath.String()

	args := ctx.ChildModuleCallArgs[key]
	if args == nil {
		args = make(map[string]cty.Value)
		ctx.ChildModuleCallArgs[key] = args
	}

	for k, v := range vals {
		args[k] = v
	}
}

func (ctx *BuiltinEvalContext) Diff() (*Diff, *sync.RWMutex) {
	return ctx.DiffValue, ctx.DiffLock
}

func (ctx *BuiltinEvalContext) State() (*State, *sync.RWMutex) {
	return ctx.StateValue, ctx.StateLock
}

func (ctx *BuiltinEvalContext) init() {
}
