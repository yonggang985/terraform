package terraform

import (
	"log"

	"github.com/hashicorp/hcl2/hcl"
	"github.com/hashicorp/terraform/config/configschema"
	"github.com/zclconf/go-cty/cty"
)

// EvalConfigBlock is an EvalNode implementation that takes a raw
// configuration block and evaluates any expressions within it.
//
// ExpandedConfig is populated with the result of expanding any "dynamic"
// blocks in the given body, which can be useful for extracting correct source
// location information for specific attributes in the result.
type EvalConfigBlock struct {
	Config          hcl.Body
	Schema          *configschema.Block
	CurrentResource *Resource
	Output          *cty.Value
	ExpandedConfig  *hcl.Body
	ContinueOnErr   bool
}

func (n *EvalConfigBlock) Eval(ctx EvalContext) (interface{}, error) {
	val, body, diags := ctx.EvaluateBlock(n.Config, n.Schema, n.CurrentResource)
	if diags.HasErrors() && n.ContinueOnErr {
		log.Printf("[WARN] Block evaluation failed: %s", diags.Err())
		return nil, EvalEarlyExitError{}
	}

	if n.Output != nil {
		*n.Output = val
	}
	if n.ExpandedConfig != nil {
		*n.ExpandedConfig = body
	}

	return nil, diags.ErrWithWarnings()
}

// EvalConfigExpr is an EvalNode implementation that takes a raw configuration
// expression and evaluates it.
type EvalConfigExpr struct {
	Expr            hcl.Expression
	CurrentResource *Resource
	Output          *cty.Value
}

func (n *EvalConfigExpr) Eval(ctx EvalContext) (interface{}, error) {
	val, diags := ctx.EvaluateExpr(n.Expr, cty.DynamicPseudoType, n.CurrentResource)

	if n.Output != nil {
		*n.Output = val
	}

	return nil, diags.ErrWithWarnings()
}
