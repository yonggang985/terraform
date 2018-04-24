package terraform

import (
	"fmt"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/zclconf/go-cty/cty"
)

// EvalBuildProviderConfig outputs a *ResourceConfig that is properly
// merged with parents and inputs on top of what is configured in the file.
type EvalBuildProviderConfig struct {
	Provider addrs.ProviderConfig
	Config   *hcl.Body
	Output   *hcl.Body
}

func (n *EvalBuildProviderConfig) Eval(ctx EvalContext) (interface{}, error) {
	cfg := *n.Config

	// If we have an Input configuration set, then merge that in
	if input := ctx.ProviderInput(n.Provider); input != nil {
		// "input" is a map of the subset of config values that were known
		// during the input walk, set by EvalInputProvider. Note that
		// in particular it does *not* include attributes that had
		// computed values at input time; those appear *only* in
		// "cfg" here.

		inputBody := configs.SynthBody("<input prompt>", input)
		cfg = configs.MergeBodies(cfg, inputBody)
	}

	*n.Output = cfg
	return nil, nil
}

// EvalConfigProvider is an EvalNode implementation that configures
// a provider that is already initialized and retrieved.
type EvalConfigProvider struct {
	Provider string
	Config   *cty.Value
}

func (n *EvalConfigProvider) Eval(ctx EvalContext) (interface{}, error) {
	return nil, ctx.ConfigureProvider(n.Provider, *n.Config).ErrWithWarnings()
}

// EvalInitProvider is an EvalNode implementation that initializes a provider
// and returns nothing. The provider can be retrieved again with the
// EvalGetProvider node.
type EvalInitProvider struct {
	TypeName string
	Name     string
}

func (n *EvalInitProvider) Eval(ctx EvalContext) (interface{}, error) {
	return ctx.InitProvider(n.TypeName, n.Name)
}

// EvalCloseProvider is an EvalNode implementation that closes provider
// connections that aren't needed anymore.
type EvalCloseProvider struct {
	Name string
}

func (n *EvalCloseProvider) Eval(ctx EvalContext) (interface{}, error) {
	ctx.CloseProvider(n.Name)
	return nil, nil
}

// EvalGetProvider is an EvalNode implementation that retrieves an already
// initialized provider instance for the given name.
type EvalGetProvider struct {
	Name   string
	Output *ResourceProvider
}

func (n *EvalGetProvider) Eval(ctx EvalContext) (interface{}, error) {
	result := ctx.Provider(n.Name)
	if result == nil {
		return nil, fmt.Errorf("provider %s not initialized", n.Name)
	}

	if n.Output != nil {
		*n.Output = result
	}

	return nil, nil
}

// EvalInputProvider is an EvalNode implementation that asks for input
// for the given provider configurations.
type EvalInputProvider struct {
	Name     string
	Provider *ResourceProvider
	Config   **ResourceConfig
}

func (n *EvalInputProvider) Eval(ctx EvalContext) (interface{}, error) {
	rc := *n.Config
	orig := rc.DeepCopy()

	// Wrap the input into a namespace
	input := &PrefixUIInput{
		IdPrefix:    fmt.Sprintf("provider.%s", n.Name),
		QueryPrefix: fmt.Sprintf("provider.%s.", n.Name),
		UIInput:     ctx.Input(),
	}

	// Go through each provider and capture the input necessary
	// to satisfy it.
	config, err := (*n.Provider).Input(input, rc)
	if err != nil {
		return nil, fmt.Errorf(
			"Error configuring %s: %s", n.Name, err)
	}

	// We only store values that have changed through Input.
	// The goal is to cache cache input responses, not to provide a complete
	// config for other providers.
	confMap := make(map[string]interface{})
	if config != nil && len(config.Config) > 0 {
		// any values that weren't in the original ResourcConfig will be cached
		for k, v := range config.Config {
			if _, ok := orig.Config[k]; !ok {
				confMap[k] = v
			}
		}
	}

	ctx.SetProviderInput(n.Name, confMap)

	return nil, nil
}
