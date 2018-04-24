package terraform

import (
	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
	"github.com/hashicorp/terraform/dag"
	"github.com/hashicorp/terraform/tfdiags"
)

// ApplyGraphBuilder implements GraphBuilder and is responsible for building
// a graph for applying a Terraform diff.
//
// Because the graph is built from the diff (vs. the config or state),
// this helps ensure that the apply-time graph doesn't modify any resources
// that aren't explicitly in the diff. There are other scenarios where the
// diff can be deviated, so this is just one layer of protection.
type ApplyGraphBuilder struct {
	// Config is the configuration tree that the diff was built from.
	Config *configs.Config

	// Diff is the diff to apply.
	Diff *Diff

	// State is the current state
	State *State

	// Providers is the list of providers supported.
	Providers []string

	// Provisioners is the list of provisioners supported.
	Provisioners []string

	// Targets are resources to target. This is only required to make sure
	// unnecessary outputs aren't included in the apply graph. The plan
	// builder successfully handles targeting resources. In the future,
	// outputs should go into the diff so that this is unnecessary.
	Targets []string

	// DisableReduce, if true, will not reduce the graph. Great for testing.
	DisableReduce bool

	// Destroy, if true, represents a pure destroy operation
	Destroy bool

	// Validate will do structural validation of the graph.
	Validate bool
}

// See GraphBuilder
func (b *ApplyGraphBuilder) Build(path addrs.ModuleInstance) (*Graph, tfdiags.Diagnostics) {
	return (&BasicGraphBuilder{
		Steps:    b.Steps(),
		Validate: b.Validate,
		Name:     "ApplyGraphBuilder",
	}).Build(path)
}

// See GraphBuilder
func (b *ApplyGraphBuilder) Steps() []GraphTransformer {
	// Custom factory for creating providers.
	concreteProvider := func(a *NodeAbstractProvider) dag.Vertex {
		return &NodeApplyableProvider{
			NodeAbstractProvider: a,
		}
	}

	concreteResource := func(a *NodeAbstractResource) dag.Vertex {
		return &NodeApplyableResource{
			NodeAbstractResource: a,
		}
	}

	steps := []GraphTransformer{
		// Creates all the nodes represented in the diff.
		&DiffTransformer{
			Concrete: concreteResource,
			Diff:     b.Diff,
		},

		// Create orphan output nodes
		&OrphanOutputTransformer{Config: b.Config, State: b.State},

		// Attach the configuration to any resources
		&AttachResourceConfigTransformer{Config: b.Config},

		// Attach the state
		&AttachStateTransformer{State: b.State},

		// add providers
		TransformProviders(b.Providers, concreteProvider, b.Config),

		// Destruction ordering
		&DestroyEdgeTransformer{Config: b.Config, State: b.State},
		GraphTransformIf(
			func() bool { return !b.Destroy },
			&CBDEdgeTransformer{Config: b.Config, State: b.State},
		),

		// Provisioner-related transformations
		&MissingProvisionerTransformer{Provisioners: b.Provisioners},
		&ProvisionerTransformer{},

		// Add root variables
		&RootVariableTransformer{Config: b.Config},

		// Add the local values
		&LocalTransformer{Config: b.Config},

		// Add the outputs
		&OutputTransformer{Config: b.Config},

		// Add module variables
		&ModuleVariableTransformer{Config: b.Config},

		// Remove modules no longer present in the config
		&RemovedModuleTransformer{Config: b.Config, State: b.State},

		// Connect references so ordering is correct
		&ReferenceTransformer{},

		// Handle destroy time transformations for output and local values.
		// Reverse the edges from outputs and locals, so that
		// interpolations don't fail during destroy.
		// Create a destroy node for outputs to remove them from the state.
		// Prune unreferenced values, which may have interpolations that can't
		// be resolved.
		GraphTransformIf(
			func() bool { return b.Destroy },
			GraphTransformMulti(
				&DestroyValueReferenceTransformer{},
				&DestroyOutputTransformer{},
				&PruneUnusedValuesTransformer{},
			),
		),

		// Add the node to fix the state count boundaries
		&CountBoundaryTransformer{},

		// Target
		&TargetsTransformer{Targets: b.Targets},

		// Close opened plugin connections
		&CloseProviderTransformer{},
		&CloseProvisionerTransformer{},

		// Single root
		&RootTransformer{},
	}

	if !b.DisableReduce {
		// Perform the transitive reduction to make our graph a bit
		// more sane if possible (it usually is possible).
		steps = append(steps, &TransitiveReductionTransformer{})
	}

	return steps
}
