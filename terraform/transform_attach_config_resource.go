package terraform

import (
	"fmt"
	"log"

	"github.com/hashicorp/terraform/addrs"
	"github.com/hashicorp/terraform/configs"
)

// GraphNodeAttachResourceConfig is an interface that must be implemented by nodes
// that want resource configurations attached.
type GraphNodeAttachResourceConfig interface {
	// ResourceAddr is the address to the resource
	ResourceAddr() *addrs.AbsResource

	// Sets the configuration
	AttachResourceConfig(*configs.Resource)
}

// AttachResourceConfigTransformer goes through the graph and attaches
// resource configuration structures to nodes that implement
// GraphNodeAttachManagedResourceConfig or GraphNodeAttachDataResourceConfig.
//
// The attached configuration structures are directly from the configuration.
// If they're going to be modified, a copy should be made.
type AttachResourceConfigTransformer struct {
	Config *configs.Config // Config is the root node in the config tree
}

func (t *AttachResourceConfigTransformer) Transform(g *Graph) error {
	log.Printf("[TRACE] AttachResourceConfigTransformer: Beginning...")

	// Go through and find GraphNodeAttachResource
	for _, v := range g.Vertices() {
		// Only care about GraphNodeAttachResource implementations
		arn, ok := v.(GraphNodeAttachResourceConfig)
		if !ok {
			continue
		}

		// Determine what we're looking for
		addr := arn.ResourceAddr()
		log.Printf(
			"[TRACE] AttachResourceConfigTransformer: Attach resource "+
				"config request: %s", addr)

		// Get the configuration.
		path := normalizeModulePath(addr.Path)
		path = path[1:]
		tree := t.Module.Child(path)
		if tree == nil {
			continue
		}

		// Go through the resource configs to find the matching config
		for _, r := range tree.Config().Resources {
			// Get a resource address so we can compare
			a, err := parseResourceAddressConfig(r)
			if err != nil {
				panic(fmt.Sprintf(
					"Error parsing config address, this is a bug: %#v", r))
			}
			a.Path = addr.Path

			// If this is not the same resource, then continue
			if !a.Equals(addr) {
				continue
			}

			log.Printf("[TRACE] Attaching resource config: %#v", r)
			arn.AttachResourceConfig(r)
			break
		}
	}

	return nil
}
